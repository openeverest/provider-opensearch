package provider

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// Compile-time interface checks.
var _ controller.BackupProvider = (*Provider)(nil)
var _ controller.BackupWatcher = (*Provider)(nil)
var _ controller.RestoreWatcher = (*Provider)(nil)

// newOpenSearchClientFunc is a seam for tests to substitute a client wired
// to a test server, bypassing the real secret lookup and TLS dial.
var newOpenSearchClientFunc = newOpenSearchClient

// SyncBackup drives an on-demand OpenSearch snapshot through the cluster's
// REST API: the operator has no snapshot CR, so there is nothing to apply —
// this creates the snapshot on first sync and polls its state afterwards.
// The target repository is backup.Spec.StorageRef.Name, registered on the
// cluster by buildBackupConfig from Instance.spec.backup.storages.
func (p *Provider) SyncBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	l := log.FromContext(c.Context())
	l.Info("Syncing backup", "name", backup.Name)

	osClient, err := newOpenSearchClientFunc(c)
	if err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("build OpenSearch client: %w", err)
	}

	repo := backup.Spec.StorageRef.Name
	snap := backup.Name

	info, err := osClient.getSnapshot(repo, snap)
	if isNotFound(err) {
		if err := osClient.createSnapshot(repo, snap); err != nil {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStatePending,
				Message: fmt.Sprintf("starting snapshot: %v", err),
			}, nil
		}
		return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStatePending}, nil
	}
	if err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("get snapshot %q in repository %q: %w", snap, repo, err)
	}

	return backupExecutionStatus(osClient, repo, info), nil
}

func backupExecutionStatus(osClient *openSearchClient, repo string, info *snapshotInfo) controller.BackupExecutionStatus {
	exec := controller.BackupExecutionStatus{}
	if info.StartTimeInMillis > 0 {
		t := metav1.NewTime(time.UnixMilli(info.StartTimeInMillis))
		exec.StartedAt = &t
	}

	switch info.State {
	case snapshotStateInProgress:
		exec.State = backupv1alpha1.BackupStateRunning
	case snapshotStateSuccess:
		exec.State = backupv1alpha1.BackupStateSucceeded
		if info.EndTimeInMillis > 0 {
			t := metav1.NewTime(time.UnixMilli(info.EndTimeInMillis))
			exec.CompletedAt = &t
		}
		if size, err := osClient.getSnapshotSize(repo, info.Snapshot); err == nil && size > 0 {
			s := fmt.Sprintf("%d", size)
			exec.Size = &s
		}
	case snapshotStatePartial, snapshotStateFailed, snapshotStateIncompatible:
		exec.State = backupv1alpha1.BackupStateFailed
		exec.Message = fmt.Sprintf("snapshot state is %q", info.State)
		if info.EndTimeInMillis > 0 {
			t := metav1.NewTime(time.UnixMilli(info.EndTimeInMillis))
			exec.CompletedAt = &t
		}
	default:
		exec.State = backupv1alpha1.BackupStatePending
	}
	return exec
}

// CleanupBackup deletes the snapshot via the REST API, unless
// DeletionPolicy is Retain, in which case the snapshot data is left in the
// repository and only the Backup CR's finalizer is released.
func (p *Provider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up backup", "name", backup.Name)

	if backup.Spec.DeletionPolicy == backupv1alpha1.BackupDeletionPolicyRetain {
		return true, nil
	}

	osClient, err := newOpenSearchClientFunc(c)
	if err != nil {
		return false, fmt.Errorf("build OpenSearch client: %w", err)
	}
	if err := osClient.deleteSnapshot(backup.Spec.StorageRef.Name, backup.Name); err != nil {
		return false, fmt.Errorf("delete snapshot %q in repository %q: %w", backup.Name, backup.Spec.StorageRef.Name, err)
	}
	return true, nil
}

// BackupWatches implements controller.BackupWatcher. Snapshot progress lives
// only in the OpenSearch REST API, not in a Kubernetes resource, so there is
// nothing to watch; the runtime's normal requeue interval drives polling.
func (p *Provider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{}
}
