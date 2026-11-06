package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// backupClassName is the BackupClass scheduled (and on-demand) backups are
// mirrored against.
const backupClassName = "opensearch-snapshot"

// instanceLabel is set by c.ObjectMeta on every resource this provider
// creates; used to scope the OpensearchSnapshotPolicy cleanup list to this
// Instance.
const instanceLabel = "app.kubernetes.io/instance"

// snapshotDateFormat is the date part of the snapshot names Snapshot
// Management generates (<policy>-<date>-<random>). Those names become Backup
// CR names, so the date must be valid in a Kubernetes name: SM's default,
// yyyy-MM-dd'T'HH:mm:ss, contains colons.
const snapshotDateFormat = "yyyyMMdd-HHmmss"

// mirrorInterval is how often scheduled snapshots are mirrored into Backup
// CRs. Snapshot Management creates them inside OpenSearch, with no event on
// any Kubernetes object to react to.
const mirrorInterval = time.Minute

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
				policy, err := buildSnapshotPolicy(c, storage.StorageRef.Name, sched)
				if err != nil {
					return fmt.Errorf("schedule %q: %w", sched.Name, err)
				}
				if err := c.Apply(policy); err != nil {
					return fmt.Errorf("sync snapshot policy %q: %w", name, err)
				}
			}
		}
	}

	return deleteStaleSnapshotPolicies(c, want)
}

func buildSnapshotPolicy(c *controller.Context, storageName string, sched corev1alpha1.InstanceBackupSchedule) (*opensearchv1.OpensearchSnapshotPolicy, error) {
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
				DateFormat:         snapshotDateFormat,
				DateFormatTimezone: "UTC",
			},
			Creation: opensearchv1.SnapshotCreation{Schedule: cron},
		},
	}

	deleteCondition, err := snapshotDeleteCondition(sched.Retention)
	if err != nil {
		return nil, err
	}
	if deleteCondition != nil {
		policy.Spec.Deletion = &opensearchv1.SnapshotDeletion{
			// Without its own schedule, a deletion condition never runs:
			// check it on the same cadence as creation.
			Schedule:        &cron,
			DeleteCondition: deleteCondition,
		}
	}
	return policy, nil
}

// snapshotDeleteCondition maps a schedule's retention onto SM's deletion
// condition. Nil retention keeps every snapshot.
//
// MinCount is always set to SM's default of 1: OpenSearch stores it when it
// is omitted, the operator then sees the stored policy differ from the CR and
// rewrites it on every reconcile, and every rewrite restarts the SM job before
// it ever runs.
func snapshotDeleteCondition(retention *corev1alpha1.BackupScheduleRetention) (*opensearchv1.SnapshotDeleteCondition, error) {
	if retention == nil {
		return nil, nil
	}
	switch retention.Type {
	case corev1alpha1.BackupScheduleRetentionTypeCount:
		if retention.Count == nil {
			return nil, nil
		}
		return &opensearchv1.SnapshotDeleteCondition{MaxCount: ptr.To(int(*retention.Count)), MinCount: ptr.To(1)}, nil
	case corev1alpha1.BackupScheduleRetentionTypeTime:
		maxAge, err := retentionMaxAge(retention.Duration)
		if err != nil {
			return nil, err
		}
		return &opensearchv1.SnapshotDeleteCondition{MaxAge: &maxAge, MinCount: ptr.To(1)}, nil
	default:
		return nil, fmt.Errorf("unsupported retention type %q", retention.Type)
	}
}

// retentionMaxAge converts a retention duration ("30d", "4w", "2m" for
// months) to an OpenSearch time value, which has no week or month unit.
// A month counts as 30 days.
func retentionMaxAge(duration string) (string, error) {
	if len(duration) < 2 {
		return "", fmt.Errorf("invalid retention duration %q", duration)
	}
	n, err := strconv.Atoi(duration[:len(duration)-1])
	if err != nil || n < 1 {
		return "", fmt.Errorf("invalid retention duration %q", duration)
	}
	switch duration[len(duration)-1] {
	case 'd':
		return fmt.Sprintf("%dd", n), nil
	case 'w':
		return fmt.Sprintf("%dd", n*7), nil
	case 'm':
		return fmt.Sprintf("%dd", n*30), nil
	default:
		return "", fmt.Errorf("invalid retention duration %q: the unit must be d, w or m", duration)
	}
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

// SnapshotMirror periodically mirrors the snapshots Snapshot Management takes
// into Backup CRs, and removes the Backup CRs of snapshots its retention
// deleted. controller.BackupMirror does not fit: it watches an operator CR per
// backup, and SM snapshots exist only in the cluster's REST API.
//
// It runs as a leader-elected manager Runnable next to the reconcilers.
type SnapshotMirror struct {
	client       client.Client
	providerName string
	interval     time.Duration
}

// NewSnapshotMirror returns the Runnable cmd/provider adds to the manager.
func NewSnapshotMirror(c client.Client) *SnapshotMirror {
	return &SnapshotMirror{client: c, providerName: common.ProviderName, interval: mirrorInterval}
}

// Start implements manager.Runnable.
func (m *SnapshotMirror) Start(ctx context.Context) error {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.mirrorAll(ctx)
		}
	}
}

func (m *SnapshotMirror) mirrorAll(ctx context.Context) {
	l := log.FromContext(ctx).WithName("snapshot-mirror")
	instances := &corev1alpha1.InstanceList{}
	if err := m.client.List(ctx, instances); err != nil {
		l.Error(err, "list instances")
		return
	}
	for i := range instances.Items {
		in := &instances.Items[i]
		if in.Spec.ProviderRef.Name != m.providerName || !in.DeletionTimestamp.IsZero() || !hasSchedules(in) {
			continue
		}
		c := controller.NewContext(ctx, m.client, in, m.providerName)
		if err := mirrorScheduledBackups(c); err != nil {
			l.Error(err, "mirror scheduled backups", "namespace", in.Namespace, "instance", in.Name)
		}
	}
}

// hasSchedules reports whether the Instance has backups enabled with any
// schedule, enabled or not: Backup CRs of a disabled schedule still need
// pruning while its retention catches up.
func hasSchedules(in *corev1alpha1.Instance) bool {
	if in.Spec.Backup == nil || !in.Spec.Backup.Enabled {
		return false
	}
	for _, storage := range in.Spec.Backup.Storages {
		if len(storage.Schedules) > 0 {
			return true
		}
	}
	return false
}

// mirrorScheduledBackups creates a Backup CR for every successful snapshot a
// schedule of this Instance produced, and deletes the Backup CRs of scheduled
// snapshots that no longer exist in the repository (removed by retention).
func mirrorScheduledBackups(c *controller.Context) error {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled {
		return nil
	}

	osClient, err := newOpenSearchClientFunc(c)
	if err != nil {
		// Cluster not reachable yet; mirroring catches up on a later pass.
		return nil
	}

	backups, err := c.BackupsForInstance()
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}

	var errs []error
	for _, storage := range backupCfg.Storages {
		snapshots, err := osClient.listSnapshots(storage.StorageRef.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("list snapshots in %q: %w", storage.StorageRef.Name, err))
			continue
		}
		existing := make(map[string]bool, len(snapshots))
		for _, snap := range snapshots {
			existing[snap.Snapshot] = true
		}

		for _, sched := range storage.Schedules {
			prefix := snapshotPolicyName(c.Name(), sched.Name) + "-"
			for _, snap := range snapshots {
				if snap.State != snapshotStateSuccess || !strings.HasPrefix(snap.Snapshot, prefix) {
					continue
				}
				if err := mirrorSnapshot(c, storage.StorageRef.Name, sched.Name, snap.Snapshot); err != nil {
					errs = append(errs, err)
				}
			}
		}

		for i := range backups {
			b := &backups[i]
			if b.Spec.ScheduleName == "" || b.Spec.StorageRef.Name != storage.StorageRef.Name ||
				b.Status.State != backupv1alpha1.BackupStateSucceeded || !b.DeletionTimestamp.IsZero() ||
				existing[b.Name] {
				continue
			}
			if err := c.Delete(b); err != nil {
				errs = append(errs, fmt.Errorf("delete backup %q of a removed snapshot: %w", b.Name, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
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
			ClassRef:   commonv1alpha1.ObjectRef{Name: backupClassName},
			StorageRef: commonv1alpha1.ObjectRef{Name: storageName},
			// Retention removes the snapshot; deleting the Backup CR by hand
			// should remove it too, like an on-demand backup.
			DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
			ScheduleName:   scheduleName,
		},
	}
	err := c.Client().Create(c.Context(), backup)
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mirror snapshot %q: %w", snapshotName, err)
	}
	return nil
}
