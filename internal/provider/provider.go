package provider

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
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
				// Read by ValidateReferences to check volume expansion support.
				storagev1.AddToScheme,
				// The Dashboards Deployment the operator leaves behind is deleted by Sync.
				appsv1.AddToScheme,
			},
			WatchConfigs: []controller.WatchConfig{
				// No predicate: status changes (phase, health) must trigger a reconcile.
				controller.WatchOwned(&opensearchv1.OpenSearchCluster{}),
				// The connection details publish the address a LoadBalancer is
				// given, which arrives after the Service is created.
				controller.WatchOwned(&corev1.Service{}),
				// Status reports Restoring while a Restore of the Instance runs.
				controller.WatchExternal(&backupv1alpha1.Restore{}, handler.EnqueueRequestsFromMapFunc(restoreInstance)),
			},
		},
	}
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
	version := resolveVersion(spec, engine, common.ComponentTypeOpensearch)
	image := engine.Image
	if image == "" {
		image = controller.GetImageForVersion(spec, common.ComponentEngine, version)
	}

	additionalConfig, err := resolveAdditionalConfig(engine)
	if err != nil {
		return fmt.Errorf("%s: configuration: %w", common.ComponentEngine, err)
	}

	snapshotRepos, keystore, backupPlugins, err := buildBackupConfig(c)
	if err != nil {
		return err
	}

	cluster := &opensearchv1.OpenSearchCluster{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       buildClusterSpec(c.Name(), engine, version, image, additionalConfig, snapshotRepos, keystore, backupPlugins),
	}

	// Apply replaces metadata wholesale; keep the operator's finalizer so it
	// is not stripped and re-added on every reconcile.
	existing := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(existing, c.Name()); err == nil {
		cluster.Finalizers = existing.Finalizers
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	if dashboards, ok := c.Instance().Spec.Components[common.ComponentDashboards]; ok {
		dVersion := resolveVersion(spec, dashboards, common.ComponentTypeDashboards)
		dImage := dashboards.Image
		if dImage == "" {
			dImage = controller.GetImageForVersion(spec, common.ComponentDashboards, dVersion)
		}
		cluster.Spec.Dashboards = buildDashboardsConfig(dashboards, dVersion, dImage)
	} else if existing.Spec.Dashboards.Enable || existing.Spec.Dashboards.Version != "" {
		// Once configured, the block stays in every apply, disabled.
		cluster.Spec.Dashboards = disabledDashboardsConfig(existing.Spec.Dashboards)
	}

	if err := c.Apply(cluster); err != nil {
		return err
	}

	if err := syncExposedService(c, engine); err != nil {
		return err
	}

	if _, ok := c.Instance().Spec.Components[common.ComponentDashboards]; !ok {
		if err := deleteDashboards(c); err != nil {
			return err
		}
	}

	// Scheduled snapshots are mirrored into Backup CRs by the snapshotMirror
	// loop, since they appear without any event on the Instance.
	if err := syncScheduledBackups(c); err != nil {
		return err
	}

	// Instance.spec.dataSource seeds a new Instance from an existing Backup.
	// ReconcileDataSource creates (or reads back) a Restore CR for it, which
	// SyncRestore then drives the same way as any user-created Restore.
	if c.Instance().Spec.DataSource != nil {
		if _, err := c.ReconcileDataSource(); err != nil {
			return fmt.Errorf("reconcile data source: %w", err)
		}
	}

	return nil
}

// resolveVersion returns the requested component version, or the provider
// default for componentType when the Instance does not set one.
func resolveVersion(spec *corev1alpha1.ProviderSpec, component corev1alpha1.ComponentSpec, componentType string) string {
	if component.Version != "" {
		return component.Version
	}
	if v := controller.GetDefaultVersion(spec, componentType); v != nil {
		return v.Version
	}
	return ""
}

// Status translates the OpenSearchCluster status into the Instance status.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	// While the initial seeding from .spec.dataSource has not reached a
	// terminal state, surface that instead of the cluster's own health:
	// the cluster itself can be green while restore is still in progress.
	if ds := c.GetDataSourceStatus(); ds != nil {
		if !ds.Done {
			return controller.Restoring(ds.Message), nil
		}
		if ds.State == controller.DataSourceStateFailed {
			return controller.Failed(ds.Message), nil
		}
	}

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
		if host, err := externalHost(c); err != nil {
			return controller.Status{}, err
		} else if host != "" {
			details = withHost(details, host)
		}
	}

	status := clusterStatus(cluster, details)
	if status.Phase == corev1alpha1.InstancePhaseReady {
		restoring, err := activeRestore(c)
		if err != nil {
			return controller.Status{}, err
		}
		if restoring != "" {
			restoringStatus := controller.Restoring(fmt.Sprintf("Restoring from Restore %q", restoring))
			restoringStatus.ConnectionDetails = details
			return restoringStatus, nil
		}
	}
	return status, nil
}

// restoreInstance maps a Restore to the Instance it restores into.
func restoreInstance(_ context.Context, obj client.Object) []reconcile.Request {
	restore, ok := obj.(*backupv1alpha1.Restore)
	if !ok || restore.Spec.InstanceRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: restore.Namespace, Name: restore.Spec.InstanceRef.Name,
	}}}
}

// activeRestore returns the name of a Restore of this Instance that has not
// finished yet, or "" when there is none.
func activeRestore(c *controller.Context) (string, error) {
	restores, err := c.RestoresForInstance()
	if err != nil {
		return "", fmt.Errorf("list restores: %w", err)
	}
	for _, r := range restores {
		if r.DeletionTimestamp.IsZero() && r.Status.State == backupv1alpha1.RestoreStateRunning {
			return r.Name, nil
		}
	}
	return "", nil
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
