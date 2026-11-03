package provider

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// SyncRestore restores a snapshot in place, driving it through the cluster's
// own REST API: the operator has no restore CR. Used both for a
// user-created Restore CR and for the Restore the runtime creates from
// Instance.spec.dataSource (see ReconcileDataSource in Sync).
//
// OpenSearch rejects restoring an index over an open index of the same
// name, so the target indices are closed first; this is a genuine
// overwrite-in-place, matching how the snapshot itself always captures
// indices:"*", include_global_state:true.
func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	l := log.FromContext(c.Context())
	l.Info("Syncing restore", "name", restore.Name)

	if restore.Spec.DataSource.Type != backupv1alpha1.DataSourceTypeBackup {
		return controller.RestoreExecutionStatus{}, fmt.Errorf(
			"unsupported data source type %q: this engine has no point-in-time recovery, only type Backup is supported",
			restore.Spec.DataSource.Type)
	}

	backup := &backupv1alpha1.Backup{}
	if err := c.Get(backup, restore.Spec.DataSource.Backup.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStatePending,
				Message: "waiting for the source Backup to exist",
			}, nil
		}
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get source backup: %w", err)
	}

	osClient, err := newOpenSearchClientFunc(c)
	if err != nil {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStatePending,
			Message: fmt.Sprintf("waiting for the cluster to be reachable: %v", err),
		}, nil
	}

	repo, snap := backup.Spec.StorageRef.Name, backup.Name

	info, err := osClient.getSnapshot(repo, snap)
	if isNotFound(err) {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("snapshot %q no longer exists in repository %q", snap, repo),
		}, nil
	}
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get snapshot %q in repository %q: %w", snap, repo, err)
	}
	if info.State != snapshotStateSuccess {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStatePending,
			Message: fmt.Sprintf("source snapshot is %q, not finished yet", info.State),
		}, nil
	}

	progress, err := osClient.restoreRecoveryStatus(info.Indices)
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get recovery status: %w", err)
	}

	switch progress {
	case restoreNotStarted:
		if err := osClient.closeIndices(info.Indices); err != nil {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStatePending,
				Message: fmt.Sprintf("closing target indices: %v", err),
			}, nil
		}
		if err := osClient.restoreSnapshot(repo, snap, info.Indices); err != nil {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStatePending,
				Message: fmt.Sprintf("starting restore: %v", err),
			}, nil
		}
		now := metav1.Now()
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateRunning, StartedAt: &now}, nil
	case restoreRunning:
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateRunning}, nil
	default: // restoreDone
		now := metav1.Now()
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateSucceeded, CompletedAt: &now}, nil
	}
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
