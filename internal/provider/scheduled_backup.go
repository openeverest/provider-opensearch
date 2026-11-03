package provider

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// backupClassName is the BackupClass scheduled (and on-demand) backups are
// mirrored against.
const backupClassName = "opensearch-snapshot"

// instanceLabel is set by c.ObjectMeta on every resource this provider
// creates; used to scope the OpensearchSnapshotPolicy cleanup list to this
// Instance.
const instanceLabel = "app.kubernetes.io/instance"

// snapshotPolicyName names both the OpensearchSnapshotPolicy CR and the
// underlying OpenSearch SM policy for a schedule, and is the prefix
// mirrorScheduledBackups matches generated snapshot names against. Schedule
// names are unique across all storages on an Instance, so the schedule name
// alone is enough.
func snapshotPolicyName(instanceName, scheduleName string) string {
	return instanceName + "-" + scheduleName
}

// syncScheduledBackups creates or updates one OpensearchSnapshotPolicy per
// enabled schedule in Instance.spec.backup.storages[].schedules, and removes
// policies for schedules that were removed or disabled.
func syncScheduledBackups(c *controller.Context) error {
	want := map[string]bool{}

	if backupCfg := c.Instance().Spec.Backup; backupCfg != nil && backupCfg.Enabled {
		for _, storage := range backupCfg.Storages {
			for _, sched := range storage.Schedules {
				if !sched.Enabled {
					continue
				}
				name := snapshotPolicyName(c.Name(), sched.Name)
				want[name] = true
				policy := buildSnapshotPolicy(c, storage.StorageRef.Name, sched)
				if err := c.Apply(policy); err != nil {
					return fmt.Errorf("sync snapshot policy %q: %w", name, err)
				}
			}
		}
	}

	return deleteStaleSnapshotPolicies(c, want)
}

func buildSnapshotPolicy(c *controller.Context, storageName string, sched corev1alpha1.InstanceBackupSchedule) *opensearchv1.OpensearchSnapshotPolicy {
	name := snapshotPolicyName(c.Name(), sched.Name)
	cron := opensearchv1.CronSchedule{Cron: opensearchv1.CronExpression{Expression: sched.Cron, Timezone: "UTC"}}

	policy := &opensearchv1.OpensearchSnapshotPolicy{
		ObjectMeta: c.ObjectMeta(name),
		Spec: opensearchv1.OpensearchSnapshotPolicySpec{
			OpensearchRef: corev1.LocalObjectReference{Name: c.Name()},
			PolicyName:    name,
			Enabled:       ptr.To(true),
			SnapshotConfig: opensearchv1.SnapshotConfig{
				Repository:         storageName,
				Indices:            "*",
				IgnoreUnavailable:  ptr.To(true),
				IncludeGlobalState: ptr.To(true),
			},
			Creation: opensearchv1.SnapshotCreation{Schedule: cron},
		},
	}
	if sched.RetentionCopies > 0 {
		policy.Spec.Deletion = &opensearchv1.SnapshotDeletion{
			// Without its own schedule, a deletion condition never runs:
			// check it on the same cadence as creation.
			Schedule:        &cron,
			DeleteCondition: &opensearchv1.SnapshotDeleteCondition{MaxCount: ptr.To(int(sched.RetentionCopies))},
		}
	}
	return policy
}

func deleteStaleSnapshotPolicies(c *controller.Context, want map[string]bool) error {
	existing := &opensearchv1.OpensearchSnapshotPolicyList{}
	if err := c.List(existing, client.MatchingLabels{instanceLabel: c.Name()}); err != nil {
		return fmt.Errorf("list snapshot policies: %w", err)
	}
	for i := range existing.Items {
		p := &existing.Items[i]
		if !want[p.Name] {
			if err := c.Delete(p); err != nil {
				return fmt.Errorf("delete stale snapshot policy %q: %w", p.Name, err)
			}
		}
	}
	return nil
}

// mirrorScheduledBackups lists snapshots created by each enabled schedule
// and creates a Backup CR for any that do not have one yet, so scheduled
// snapshots become visible to users the same way on-demand ones are.
//
// controller.BackupMirror does not fit here: it is triggered by watching a
// single operator-native backup CR, but OpenSearch snapshots are not
// Kubernetes objects at all — they exist only in the cluster's REST API.
// Polling during Sync is used instead.
func mirrorScheduledBackups(c *controller.Context) error {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled {
		return nil
	}

	var osClient *openSearchClient
	for _, storage := range backupCfg.Storages {
		for _, sched := range storage.Schedules {
			if !sched.Enabled {
				continue
			}
			if osClient == nil {
				var err error
				if osClient, err = newOpenSearchClientFunc(c); err != nil {
					// Cluster not reachable yet; mirroring will catch up on
					// a later reconcile.
					return nil
				}
			}

			snapshots, err := osClient.listSnapshots(storage.StorageRef.Name)
			if err != nil {
				continue
			}
			prefix := snapshotPolicyName(c.Name(), sched.Name) + "-"
			for _, snap := range snapshots {
				if snap.State != snapshotStateSuccess || !strings.HasPrefix(snap.Snapshot, prefix) {
					continue
				}
				if err := mirrorSnapshot(c, storage.StorageRef.Name, sched.Name, snap.Snapshot); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// mirrorSnapshot creates a Backup CR for a scheduler-produced snapshot,
// using the snapshot's own name so creating it twice is a no-op. It is
// deliberately not owned by the Instance: Backup.spec.origin.instanceRef is
// how the runtime already tracks that relationship, and its own
// InstanceDeletionPolicy (Cascade/Retain) governs cleanup — a Kubernetes
// ownerReference here would bypass that.
func mirrorSnapshot(c *controller.Context, storageName, scheduleName, snapshotName string) error {
	backup := &backupv1alpha1.Backup{
		ObjectMeta: c.ObjectMeta(snapshotName),
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: c.Name()},
			},
			ClassRef:     commonv1alpha1.ObjectRef{Name: backupClassName},
			StorageRef:   commonv1alpha1.ObjectRef{Name: storageName},
			ScheduleName: scheduleName,
		},
	}
	err := c.Client().Create(c.Context(), backup)
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
