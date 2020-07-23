## # Building Images
##

## make                    # Alias to `make build-all`.
## make build
.PHONY: build
build: build-all

## make build-all          # Build the manager, app, and nonvnc-proxy images.
build-all: build-manager build-app build-novnc-proxy

## make build-manager      # Build the manager docker image.
build-manager:
	$(call build_docker,manager,${MANAGER_IMAGE})

## make build-app          # Build the app docker image.
build-app:
	$(call build_docker,app,${APP_IMAGE})

## make build-novnc-proxy  # Build the novnc-proxy image.
build-novnc-proxy:
	$(call build_docker,novnc-proxy,${NOVNC_PROXY_IMAGE})

##
## # Pushing images
##

## make push               # Alias to make push-all.
push: build-manager push-manager push-novnc-proxy

## make push-all           # Push the manager, app, and novnc-proxy images.
push-all: push-manager push-app push-novnc-proxy

## make push-manager       # Push the manager docker image.
push-manager: build-manager
	docker push ${MANAGER_IMAGE}

## make push-app           # Push the app docker image.
push-app: build-app
	docker push ${APP_IMAGE}

## make push-nonvnc-proxy  # Push the novnc-proxy docker image.
push-novnc-proxy: build-novnc-proxy
	docker push ${NOVNC_PROXY_IMAGE}

##
## # Helm Chart Functions
##

## make chart-yaml     # Generate the Chart.yaml from the template in hack/Makevars.mk.
chart-yaml:
	echo "$$CHART_YAML" > deploy/charts/kvdi/Chart.yaml

## make package-chart  # Packages the helm chart.
package-chart: ${HELM} chart-yaml
	cd deploy/charts && helm package kvdi

## make package-index  # Create the helm repo package index.
package-index:
	cd deploy/charts && helm repo index .

##
## # Codegen Functions
##

${OPERATOR_SDK}:
	$(call download_bin,${OPERATOR_SDK},${OPERATOR_SDK_URL})

## make generate            # Generates deep copy code for the k8s apis.
generate: ${OPERATOR_SDK}
	GOROOT=${GOROOT} ${OPERATOR_SDK} generate k8s --verbose

## make manifests           # Generates CRD manifest.
manifests: ${OPERATOR_SDK}
	${OPERATOR_SDK} generate crds --verbose

##
## # Linting and Testing
##

${GOLANGCI_LINT}:
	mkdir -p $(dir ${GOLANGCI_LINT})
	cd $(dir ${GOLANGCI_LINT}) && curl -JL ${GOLANGCI_DOWNLOAD_URL} | tar xzf -
	chmod +x $(dir ${GOLANGCI_LINT})golangci-lint-${GOLANGCI_VERSION}-$(shell uname | tr A-Z a-z)-amd64/golangci-lint
	ln -s golangci-lint-${GOLANGCI_VERSION}-$(shell uname | tr A-Z a-z)-amd64/golangci-lint ${GOLANGCI_LINT}

## make lint   # Lint files
lint: ${GOLANGCI_LINT}
	${GOLANGCI_LINT} run -v --timeout 300s

## make test   # Run unit tests
TEST_FLAGS ?= -v -cover -race -coverpkg=./... -coverprofile=profile.cov
test:
	go test ${TEST_FLAGS} ./...
	go tool cover -func profile.cov
	rm profile.cov

##
## # Local Testing with Kind
##

# Ensures a repo-local installation of kind
${KIND}:
	$(call download_bin,${KIND},${KIND_DOWNLOAD_URL})

# Ensures a repo-local installation of kubectl
${KUBECTL}:
	$(call download_bin,${KUBECTL},${KUBECTL_DOWNLOAD_URL})

# Ensures a repo-local installation of helm
${HELM}:
	$(call get_helm)

## make test-cluster           # Make a local kind cluster for testing.
test-cluster: ${KIND}
	echo -e "$$KIND_CLUSTER_MANIFEST"
	echo "$$KIND_CLUSTER_MANIFEST" | ${KIND} \
			create cluster \
			--config - \
			--image kindest/node:${KUBERNETES_VERSION} \
			--name ${CLUSTER_NAME} \
			--kubeconfig ${KIND_KUBECONFIG}

## make load-all               # Load all the docker images into the local kind cluster.
load-all: load-manager load-app load-novnc-proxy

## make load-manager
load-manager: ${KIND} build-manager
	$(call load_image,${MANAGER_IMAGE})

## make load-app
load-app: ${KIND} build-app
	$(call load_image,${APP_IMAGE})

## make load-novnc-proxy
load-novnc-proxy: ${KIND} build-novnc-proxy
	$(call load_image,${NOVNC_PROXY_IMAGE})

KUBECTL_KIND = ${KUBECTL} --kubeconfig ${KIND_KUBECONFIG}
HELM_KIND = ${HELM} --kubeconfig ${KIND_KUBECONFIG}

## make test-ingress           # Deploys metallb load balancer to the kind cluster.
test-ingress: ${KUBECTL}
	${KUBECTL_KIND} apply -f https://raw.githubusercontent.com/google/metallb/${METALLB_VERSION}/manifests/namespace.yaml
	${KUBECTL_KIND} apply -f https://raw.githubusercontent.com/google/metallb/${METALLB_VERSION}/manifests/metallb.yaml
	${KUBECTL_KIND} create secret generic -n metallb-system memberlist --from-literal=secretkey="`openssl rand -base64 128`" || echo
	echo "$$METALLB_CONFIG" | ${KUBECTL_KIND} apply -f -

## make test-vault             # Deploys a vault instance into the kind cluster.
test-vault: ${KUBECTL} ${HELM}
	${HELM} repo add hashicorp https://helm.releases.hashicorp.com
	${HELM_KIND} upgrade --install vault hashicorp/vault \
		--set server.dev.enabled=true \
		--wait
	${KUBECTL_KIND} wait --for=condition=ready pod vault-0 --timeout=300s
	${KUBECTL_KIND} exec -it vault-0 -- vault auth enable kubernetes
	${KUBECTL_KIND} \
		config view --raw --minify --flatten -o jsonpath='{.clusters[].cluster.certificate-authority-data}' | \
		base64 --decode > ca.crt
	${KUBECTL_KIND} exec -it vault-0 -- vault write auth/kubernetes/config \
		token_reviewer_jwt=`${KUBECTL_KIND} exec -it vault-0 -- cat /var/run/secrets/kubernetes.io/serviceaccount/token` \
		kubernetes_host=https://kubernetes.default:443 \
		kubernetes_ca_cert="`cat ca.crt`"
	rm ca.crt
	echo "$$VAULT_POLICY" | ${KUBECTL_KIND} exec -it vault-0 -- vault policy write kvdi -
	${KUBECTL_KIND} exec -it vault-0 -- vault secrets enable --path=kvdi/ kv
	${KUBECTL_KIND} exec -it vault-0 -- vault write auth/kubernetes/role/kvdi \
	    bound_service_account_names=kvdi-app,kvdi-manager \
	    bound_service_account_namespaces=default \
	    policies=kvdi \
	    ttl=1h

## make test-ldap              # Deploys a test LDAP server into the kind cluster.
test-ldap:
	${KUBECTL_KIND} apply -f hack/glauth.yaml

##
## make full-test-cluster      # Builds a kind cluster with metallb and cert-manager installed.
full-test-cluster: test-cluster test-ingress test-certmanager

##
## make example-vdi-templates  # Deploys the example VDITemplates into the kind cluster.
example-vdi-templates: ${KUBECTL}
	${KUBECTL_KIND} apply \
		-f deploy/examples/example-desktop-templates.yaml

##
## make restart-manager    # Restart the manager pod.
restart-manager: ${KUBECTL}
	${KUBECTL_KIND} delete pod -l component=kvdi-manager

## make restart-app        # Restart the app pod.
restart-app: ${KUBECTL}
	${KUBECTL_KIND} delete pod -l vdiComponent=app

## make restart            # Restart the manager and app pod.
restart: restart-manager restart-app

## make clean-cluster      # Remove all kVDI components from the cluster for a fresh start.
clean-cluster: ${KUBECTL} ${HELM}
	${KUBECTL_KIND} delete --ignore-not-found certificate --all
	${HELM_KIND} del kvdi

## make remove-cluster     # Deletes the kind cluster.
remove-cluster: ${KIND}
	${KIND} delete cluster --name ${CLUSTER_NAME}
	rm -f ${KIND_KUBECONFIG}

##
## # Runtime Helpers
##

## make forward-app         # Run a kubectl port-forward to the app pod.
forward-app: ${KUBECTL}
	${KUBECTL_KIND} get pod | grep app | awk '{print$$1}' | xargs -I% ${KUBECTL_KIND} port-forward % 8443

## make get-app-secret      # Get the app client TLS certificate for debugging.
get-app-secret: ${KUBECTL}
	${KUBECTL_KIND} get secret kvdi-app-client -o json | jq -r '.data["ca.crt"]' | base64 -d > _bin/ca.crt
	${KUBECTL_KIND} get secret kvdi-app-client -o json | jq -r '.data["tls.crt"]' | base64 -d > _bin/tls.crt
	${KUBECTL_KIND} get secret kvdi-app-client -o json | jq -r '.data["tls.key"]' | base64 -d > _bin/tls.key

## make get-admin-password  # Get the generated admin password for kVDI.
get-admin-password: ${KUBECTL}
	${KUBECTL_KIND} get secret kvdi-admin-secret -o json | jq -r .data.password | base64 -d && echo

# Builds and deploys the manager into a local kind cluster, requires helm.
.PHONY: deploy
HELM_ARGS ?=
deploy: ${HELM} package-chart
	${HELM_KIND} upgrade --install ${NAME} deploy/charts/${NAME}-${VERSION}.tgz --wait ${HELM_ARGS}

##
## # Doc generation
##

${REFDOCS_CLONE}:
	mkdir -p $(dir ${REFDOCS})
	git clone https://github.com/ahmetb/gen-crd-api-reference-docs "${REFDOCS_CLONE}"

${REFDOCS}: ${REFDOCS_CLONE}
	cd "${REFDOCS_CLONE}" && go build .
	mv "${REFDOCS_CLONE}/gen-crd-api-reference-docs" "${REFDOCS}"

${HELM_DOCS}:
	$(call get_helm_docs)

## make api-docs            # Generate the CRD API documentation.
api-docs: ${REFDOCS}
	go mod vendor
	bash hack/update-api-docs.sh

## make helm-docs           # Generates the helm chart documentation.
HELM_DOCS_VERSION ?= 0.13.0
helm-docs: ${HELM_DOCS} chart-yaml
	${HELM_DOCS}


##
## ######################################################################################
##
## make help                # Print this help message
help:
	@echo "# MAKEFILE USAGE" && echo
	@fgrep -h "##" $(MAKEFILE_LIST) | fgrep -v fgrep | sed -e 's/\\$$//' | sed -e 's/##//'


# includes
-include hack/Makevars.mk
-include hack/Manifests.mk
-include hack/MakeDesktops.mk