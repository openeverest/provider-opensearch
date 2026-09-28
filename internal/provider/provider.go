package provider

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// Compile-time check that Provider implements the required interface.
var _ controller.ProviderInterface = (*Provider)(nil)

// Provider implements controller.ProviderInterface for OpenSearch, backed by
// the OpenSearch Kubernetes operator.
type Provider struct {
	controller.BaseProvider
}

// New creates a new Provider instance.
func New() *Provider {
	return &Provider{
		BaseProvider: controller.BaseProvider{
			ProviderName: common.ProviderName,
			SchemeFuncs: []func(*runtime.Scheme) error{
				opensearchv1.AddToScheme,
			},
			WatchConfigs: []controller.WatchConfig{
				// No predicate: status changes (phase, health) must trigger a reconcile.
				controller.WatchOwned(&opensearchv1.OpenSearchCluster{}),
			},
		},
	}
}

// Validate checks if the Instance spec is valid.
func (p *Provider) Validate(c *controller.Context) error {
	return validate(c)
}

// Sync creates or updates the OpenSearchCluster for the Instance.
func (p *Provider) Sync(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Syncing OpenSearchCluster", "name", c.Name())

	spec, err := c.ProviderSpec()
	if err != nil {
		return err
	}

	engine := c.Instance().Spec.Components[common.ComponentEngine]
	version := resolveVersion(spec, engine)
	image := engine.Image
	if image == "" {
		image = controller.GetImageForVersion(spec, common.ComponentEngine, version)
	}

	cluster := &opensearchv1.OpenSearchCluster{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       buildClusterSpec(c.Name(), engine, version, image),
	}

	// Apply replaces metadata wholesale; keep the operator's finalizer so it
	// is not stripped and re-added on every reconcile.
	existing := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(existing, c.Name()); err == nil {
		cluster.Finalizers = existing.Finalizers
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	return c.Apply(cluster)
}

// resolveVersion returns the requested engine version, or the provider default
// when the Instance does not set one.
func resolveVersion(spec *corev1alpha1.ProviderSpec, engine corev1alpha1.ComponentSpec) string {
	if engine.Version != "" {
		return engine.Version
	}
	if v := controller.GetDefaultVersion(spec, common.ComponentTypeOpensearch); v != nil {
		return v.Version
	}
	return ""
}

// Status translates the OpenSearchCluster status into the Instance status.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	cluster := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(cluster, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.Provisioning("Waiting for OpenSearchCluster to be created"), nil
		}
		return controller.Status{}, err
	}

	var details controller.ConnectionDetails
	if cluster.Status.Initialized {
		secret := &corev1.Secret{}
		if err := c.Get(secret, adminSecretName(c.Name())); err != nil {
			// Also returned for NotFound: an error requeues, while the secret's
			// creation alone would not trigger another reconcile.
			return controller.Status{}, fmt.Errorf("get admin credentials secret: %w", err)
		}
		details = connectionDetails(cluster, secret)
	}

	return clusterStatus(cluster, details), nil
}

// Cleanup deletes the OpenSearchCluster and the operator-generated secrets
// that are not garbage collected through owner references.
func (p *Provider) Cleanup(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up OpenSearchCluster", "name", c.Name())

	cluster := &opensearchv1.OpenSearchCluster{ObjectMeta: c.ObjectMeta(c.Name())}
	if err := c.Delete(cluster); err != nil {
		return err
	}
	exists, err := c.Exists(&opensearchv1.OpenSearchCluster{}, c.Name())
	if err != nil {
		return err
	}
	if exists {
		return controller.WaitFor("waiting for OpenSearchCluster to be deleted")
	}

	var errs []error
	for _, name := range unownedSecretNames(c.Name()) {
		secret := &corev1.Secret{ObjectMeta: c.ObjectMeta(name)}
		errs = append(errs, c.Delete(secret))
	}
	return errors.Join(errs...)
}
