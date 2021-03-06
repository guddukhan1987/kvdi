package api

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tinyzimmer/kvdi/pkg/util/apiutil"
)

// Prometheus gatherers

var (
	// requestDuration tracks request latency for all routes
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "kvdi",
		Name:      "http_request_duration_seconds",
		Help:      "The latency of HTTP requests by path and method.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"path", "method"})

	// requestsTotal tracks response codes and methods for all routes
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kvdi",
		Name:      "http_requests_total",
		Help:      "Total number of HTTP requests by status code, path, and method.",
	}, []string{"path", "code", "method"})

	// displayBytesSentTotal tracks bytes sent over a websocket display stream
	displayBytesSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kvdi",
		Name:      "ws_display_bytes_sent_total",
		Help:      "Total bytes sent over websocket display connections by desktop and client.",
	}, []string{"desktop", "client"})

	// audioBytesSentTotal tracks bytes sent over a websocket audio stream
	audioBytesSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kvdi",
		Name:      "ws_audio_bytes_sent_total",
		Help:      "Total bytes sent over websocket audio connections by desktop and client.",
	}, []string{"desktop", "client"})

	// displayBytesSentTotal tracks bytes received over a websocket display stream
	displayBytesReceivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kvdi",
		Name:      "ws_display_bytes_rcvd_total",
		Help:      "Total bytes received over websocket display connections by desktop and client.",
	}, []string{"desktop", "client"})

	// audioBytesSentTotal tracks bytes received over a websocket audio stream
	audioBytesReceivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kvdi",
		Name:      "ws_audio_bytes_rcvd_total",
		Help:      "Total bytes received over websocket audio connections by desktop and client.",
	}, []string{"desktop", "client"})

	// activeDisplayStreams tracks the number of active display connections
	activeDisplayStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "kvdi",
		Name:      "active_display_streams",
		Help:      "The current number of active display streams.",
	})

	// activeDisplayStreams tracks the number of active audio connections
	activeAudioStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "kvdi",
		Name:      "active_audio_streams",
		Help:      "The current number of active audio streams.",
	})
)

// apiResponseWriter extends the regular http.ResponseWriter and stores the
// status code internally to be referenced by the metrics collector.
// When a Hijack is requested for a websocket connection, the net.Conn interface
// is wrapped with an object that sends data transfer metrics to prometheus.
type apiResponseWriter struct {
	http.ResponseWriter
	status int

	isAudio, isDisplay      bool
	clientAddr, desktopName string
}

func (a *apiResponseWriter) WriteHeader(s int) {
	a.ResponseWriter.WriteHeader(s)
	a.status = s
}

func (a *apiResponseWriter) Status() int { return a.status }

func (a *apiResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h := a.ResponseWriter.(http.Hijacker)
	conn, rw, err := h.Hijack()
	if err == nil && a.status == http.StatusOK {
		// The status will be StatusSwitchingProtocols if there was no error and
		// WriteHeader has not been called yet
		a.status = http.StatusSwitchingProtocols
	}
	return &websocketWatcher{
		Conn:        conn,
		isAudio:     a.isAudio,
		isDisplay:   a.isDisplay,
		clientAddr:  a.clientAddr,
		desktopName: a.desktopName,
	}, rw, err
}

type websocketWatcher struct {
	net.Conn

	rsize int
	wsize int

	isAudio, isDisplay      bool
	clientAddr, desktopName string
}

func (w *websocketWatcher) Read(b []byte) (int, error) {
	size, err := w.Conn.Read(b)
	w.rsize += size
	if w.isDisplay {
		displayBytesReceivedTotal.With(w.prometheusLabels()).Add(float64(size))
	}
	if w.isAudio {
		audioBytesReceivedTotal.With(w.prometheusLabels()).Add(float64(size))
	}
	return size, err
}

func (w *websocketWatcher) Write(b []byte) (int, error) {
	size, err := w.Conn.Write(b)
	w.wsize += size
	if w.isDisplay {
		displayBytesSentTotal.With(w.prometheusLabels()).Add(float64(size))
	}
	if w.isAudio {
		audioBytesSentTotal.With(w.prometheusLabels()).Add(float64(size))
	}
	return size, err
}

func (w *websocketWatcher) prometheusLabels() prometheus.Labels {
	return prometheus.Labels{"desktop": w.desktopName, "client": w.clientAddr}
}

// prometheusMiddleware implements mux.MiddlewareFunc and tracks request metrics.s
func prometheusMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// get the path for this request
		path := strings.TrimSuffix(apiutil.GetGorillaPath(r), "/")

		// determine if this is a websocket path
		isWebsocket := strings.HasSuffix(path, "websockify") || strings.HasSuffix(path, "wsaudio")

		var timer *prometheus.Timer

		// wrap the response writer so we can intercept request metadata
		aw := &apiResponseWriter{ResponseWriter: w, status: http.StatusOK}

		if !isWebsocket {
			// start a timer for non-websocket endpoints
			timer = prometheus.NewTimer(requestDuration.With(prometheus.Labels{
				"path":   path,
				"method": r.Method,
			}))
		} else {
			// Track active websocket connections
			aw.clientAddr = strings.Split(r.RemoteAddr, ":")[0]
			aw.desktopName = apiutil.GetNamespacedNameFromRequest(r).String()
			if strings.HasSuffix(path, "websockify") {
				// this is a display connection
				activeDisplayStreams.Inc()
				aw.isDisplay = true
			} else if strings.HasSuffix(path, "wsaudio") {
				// this is an audio connection
				activeAudioStreams.Inc()
				aw.isAudio = true
			}
		}

		// run the request flow
		next.ServeHTTP(aw, r)

		// post request flow logic

		if !isWebsocket {
			// incremement the requestsTotal metric
			requestsTotal.With(prometheus.Labels{
				"path":   path,
				"method": r.Method,
				"code":   strconv.Itoa(aw.Status()),
			}).Inc()
			// record the duration of the request
			timer.ObserveDuration()
		} else {
			if strings.HasSuffix(path, "websockify") {
				// this was a display connection
				activeDisplayStreams.Dec()
			} else if strings.HasSuffix(path, "wsaudio") {
				// this was an audio connection
				activeAudioStreams.Dec()
			}
		}

	})
}
