package app

import (
	"github.com/tinyzimmer/kvdi/pkg/apis/kvdi/v1alpha1"
	v1 "github.com/tinyzimmer/kvdi/pkg/apis/meta/v1"

	"github.com/tinyzimmer/kvdi/pkg/auth"
	"github.com/tinyzimmer/kvdi/pkg/pki"
	"github.com/tinyzimmer/kvdi/pkg/resources"
	"github.com/tinyzimmer/kvdi/pkg/secrets"
	"github.com/tinyzimmer/kvdi/pkg/util/common"
	"github.com/tinyzimmer/kvdi/pkg/util/errors"
	"github.com/tinyzimmer/kvdi/pkg/util/reconcile"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reconciler implements a reconciler for app-related resources.
type Reconciler struct {
	resources.VDIReconciler

	client client.Client
	scheme *runtime.Scheme
}

var _ resources.VDIReconciler = &Reconciler{}

// New returns a new App reconciler
func New(c client.Client, s *runtime.Scheme) resources.VDIReconciler {
	return &Reconciler{client: c, scheme: s}
}

// Reconcile reconciles all the core-components of a kVDI cluster.
func (f *Reconciler) Reconcile(reqLogger logr.Logger, instance *v1alpha1.VDICluster) error {
	// Generate the admin secret
	adminPass, err := f.reconcileAdminSecret(reqLogger, instance)
	if err != nil {
		return err
	}

	// Set up a temporary connection to the secrets engine
	secretsEngine := secrets.GetSecretEngine(instance)
	if err := secretsEngine.Setup(f.client, instance); err != nil {
		return err
	}
	defer func() {
		if err := secretsEngine.Close(); err != nil {
			reqLogger.Error(err, "Error cleaning up secrets engine")
		}
	}()

	// Reconcile a secret for generating JWT tokens
	if _, err := secretsEngine.ReadSecret(v1.JWTSecretKey, false); err != nil {
		if !errors.IsSecretNotFoundError(err) {
			return err
		}
		jwtSecret := common.GeneratePassword(32)
		if err := secretsEngine.WriteSecret(v1.JWTSecretKey, []byte(jwtSecret)); err != nil {
			return err
		}
	}

	// Reconcile the built-in roles.
	if err := reconcile.ReconcileVDIRole(reqLogger, f.client, instance.GetAdminRole()); err != nil {
		return err
	}

	if err := reconcile.ReconcileVDIRole(reqLogger, f.client, instance.GetLaunchTemplatesRole()); err != nil {
		return err
	}

	// reconcile any resources needed for the auth provider
	authProvider := auth.GetAuthProvider(instance)
	if err := authProvider.Reconcile(reqLogger, f.client, instance, adminPass); err != nil {
		return err
	}
	if err := authProvider.Close(); err != nil {
		reqLogger.Error(err, "Failed to close auth provider cleanly")
	}

	// Service account and cluster role/binding
	if err := reconcile.ReconcileServiceAccount(reqLogger, f.client, newAppServiceAccountForCR(instance)); err != nil {
		return err
	}
	if err := reconcile.ReconcileClusterRole(reqLogger, f.client, newAppClusterRoleForCR(instance)); err != nil {
		return err
	}
	if err := reconcile.ReconcileClusterRoleBinding(reqLogger, f.client, newRoleBindingsForCR(instance)); err != nil {
		return err
	}

	// Reconcile the PKI. This will ensure a CA as well as both server and client certificates
	// for the app deployment.
	if err := pki.New(f.client, instance, secretsEngine).Reconcile(reqLogger); err != nil {
		return err
	}

	// App deployment and service
	if err := reconcile.ReconcileDeployment(reqLogger, f.client, newAppDeploymentForCR(instance), true); err != nil {
		return err
	}
	if err := reconcile.ReconcileService(reqLogger, f.client, newAppServiceForCR(instance)); err != nil {
		return err
	}

	return nil
}
