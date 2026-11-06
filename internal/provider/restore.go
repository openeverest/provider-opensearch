package provider

import (
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// SyncRestore restores a snapshot in place, driving it through the cluster's
// own REST API: the operator has no restore CR. Used both for a
// user-created Restore CR and for the Restore the runtime creates from
// Instance.spec.dataSource (see ReconcileDataSource in Sync).
//
// Only user indices are restored (see restorableIndices). OpenSearch rejects
// restoring an index over an open index of the same name, so the target
// indices are closed first: the restore overwrites them.
//
// Restore.status.startedAt, which the runtime keeps once set, records that
// the restore was started; afterwards only its progress is polled.
func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	l := log.FromContext(c.Context())
	l.Info("Syncing restore", "name", restore.Name)

	if restore.Spec.DataSource.Type != backupv1alpha1.DataSourceTypeBackup || restore.Spec.DataSource.Backup == nil {
		return failedRestore("point-in-time recovery is not supported; restore from a Backup"), nil
	}

	backup := &backupv1alpha1.Backup{}
	if err := c.Get(backup, restore.Spec.DataSource.Backup.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return pendingRestore("waiting for the source Backup to exist"), nil
		}
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get source backup: %w", err)
	}

	if ready, err := clusterReady(c); err != nil {
		return controller.RestoreExecutionStatus{}, err
	} else if !ready && restore.Status.StartedAt == nil {
		return pendingRestore("waiting for the OpenSearch cluster to be running"), nil
	}

	osClient, err := newOpenSearchClientFunc(c)
	if err != nil {
		return pendingRestore(fmt.Sprintf("waiting for the cluster to be reachable: %v", err)), nil
	}

	if err := requireSourceStorage(c, backup); err != nil {
		return failedRestore(err.Error()), nil
	}
	repo, err := sourceRepository(c, osClient, backup)
	if err != nil {
		// Right after bootstrap the storage client can still be loading;
		// registration is retried.
		return pendingRestore(err.Error()), nil
	}
	snap := backup.Name

	info, err := osClient.getSnapshot(repo, snap)
	if isNotFound(err) {
		return failedRestore(fmt.Sprintf("snapshot %q does not exist in repository %q", snap, repo)), nil
	}
	if err != nil {
		return pendingRestore(fmt.Sprintf("reading snapshot %q: %v", snap, err)), nil
	}
	switch info.State {
	case snapshotStateSuccess:
	case snapshotStateInProgress:
		return pendingRestore("waiting for the source snapshot to finish"), nil
	default:
		return failedRestore(fmt.Sprintf("source snapshot is %q and cannot be restored", info.State)), nil
	}

	indices := restorableIndices(info.Indices)

	if restore.Status.StartedAt == nil {
		if len(indices) > 0 {
			if err := osClient.closeIndices(indices); err != nil {
				return pendingRestore(fmt.Sprintf("closing target indices: %v", err)), nil
			}
			if err := osClient.restoreSnapshot(repo, snap, indices); err != nil {
				return pendingRestore(fmt.Sprintf("starting restore: %v", err)), nil
			}
		}
		now := metav1.Now()
		return controller.RestoreExecutionStatus{
			State:     backupv1alpha1.RestoreStateRunning,
			Message:   fmt.Sprintf("restoring %d indices", len(indices)),
			StartedAt: &now,
		}, nil
	}

	progress, err := osClient.restoreRecoveryStatus(indices)
	if err != nil {
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateRunning,
			Message: fmt.Sprintf("reading restore progress: %v", err)}, nil
	}
	if progress != restoreDone {
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateRunning}, nil
	}
	now := metav1.Now()
	return controller.RestoreExecutionStatus{
		State:       backupv1alpha1.RestoreStateSucceeded,
		Message:     fmt.Sprintf("restored %d indices", len(indices)),
		CompletedAt: &now,
	}, nil
}

func pendingRestore(message string) controller.RestoreExecutionStatus {
	return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStatePending, Message: message}
}

func failedRestore(message string) controller.RestoreExecutionStatus {
	now := metav1.Now()
	return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateFailed, Message: message, CompletedAt: &now}
}

// clusterReady reports whether the operator has the cluster running, which a
// new Instance seeded from spec.dataSource reaches only after bootstrapping.
func clusterReady(c *controller.Context) (bool, error) {
	cluster := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(cluster, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get OpenSearchCluster: %w", err)
	}
	return cluster.Status.Initialized && cluster.Status.Phase == opensearchv1.PhaseRunning, nil
}

// sourceRepository returns the repository, on this cluster, that holds
// backup's snapshot. A backup of this Instance is in its own repository for
// the storage. A backup of another Instance is under that Instance's base
// path, so it is exposed through a read-only repository the provider
// registers: two clusters must never write to the same repository.
func sourceRepository(c *controller.Context, osClient *openSearchClient, backup *backupv1alpha1.Backup) (string, error) {
	storage, origin := backup.Spec.StorageRef.Name, backupOrigin(backup)
	if origin == "" || origin == c.Name() {
		return storage, nil
	}

	bs, err := c.BackupStorage(storage)
	if err != nil {
		return "", fmt.Errorf("backup storage %q: %w", storage, err)
	}
	settings, err := s3RepositorySettings(bs, repositoryBasePath(backup.Namespace, origin))
	if err != nil {
		return "", err
	}
	settings["readonly"] = "true"

	repo := sourceRepositoryName(storage, origin)
	if err := osClient.registerRepository(repo, s3RepoType, settings); err != nil {
		return "", fmt.Errorf("register repository %q for the snapshots of Instance %q: %w", repo, origin, err)
	}
	return repo, nil
}

// backupOrigin returns the Instance a Backup was taken of, or "" when unknown.
func backupOrigin(backup *backupv1alpha1.Backup) string {
	if ref := backup.Spec.Origin.InstanceRef; ref != nil {
		return ref.Name
	}
	return ""
}

// requireSourceStorage rejects restoring another Instance's backup through a
// storage this Instance does not use: the repository authenticates with the
// storage's client, whose keys are only in the keystore of Instances that
// list the storage in spec.backup.storages.
func requireSourceStorage(c *controller.Context, backup *backupv1alpha1.Backup) error {
	origin, storage := backupOrigin(backup), backup.Spec.StorageRef.Name
	if origin == "" || origin == c.Name() {
		return nil
	}
	backupCfg := c.Instance().Spec.Backup
	if backupCfg != nil && backupCfg.Enabled && slices.ContainsFunc(backupCfg.Storages, func(s corev1alpha1.InstanceBackupStorage) bool {
		return s.StorageRef.Name == storage
	}) {
		return nil
	}
	return fmt.Errorf("restoring a backup of Instance %q needs backup storage %q in this Instance's spec.backup.storages", origin, storage)
}

// sourceRepositoryName names the read-only repository exposing another
// Instance's snapshots in a storage.
func sourceRepositoryName(storage, origin string) string {
	return fmt.Sprintf("%s-from-%s", storage, origin)
}

// CleanupRestore has nothing to roll back: the data is already restored in
// place, and deleting the Restore record should not undo it.
func (p *Provider) CleanupRestore(_ *controller.Context, _ *backupv1alpha1.Restore) (bool, error) {
	return true, nil
}

// RestoreWatches implements controller.RestoreWatcher. Recovery progress
// lives only in the OpenSearch REST API, not in a Kubernetes resource, so
// there is nothing to watch; the runtime's normal requeue interval drives
// polling. See BackupWatches.
func (p *Provider) RestoreWatches() []controller.WatchConfig {
	return []controller.WatchConfig{}
}
