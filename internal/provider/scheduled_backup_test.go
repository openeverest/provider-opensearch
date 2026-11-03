package provider

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

func instanceWithSchedule(storageName string, sched corev1alpha1.InstanceBackupSchedule) *corev1alpha1.Instance {
	return instanceWithBackup(corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: storageName},
		Schedules:  []corev1alpha1.InstanceBackupSchedule{sched},
	})
}

func TestSyncScheduledBackups(t *testing.T) {
	t.Run("backup not enabled: nothing created", func(t *testing.T) {
		c, cl := newTestContext(t, testInstance(validEngine()))

		require.NoError(t, syncScheduledBackups(c))

		list := &opensearchv1.OpensearchSnapshotPolicyList{}
		require.NoError(t, cl.List(context.Background(), list, client.InNamespace(testNamespace)))
		assert.Empty(t, list.Items)
	})

	t.Run("enabled schedule creates a policy", func(t *testing.T) {
		in := instanceWithSchedule("my-storage", corev1alpha1.InstanceBackupSchedule{
			Name: "daily", Enabled: true, Cron: "0 2 * * *",
		})
		c, cl := newTestContext(t, in)

		require.NoError(t, syncScheduledBackups(c))

		got := &opensearchv1.OpensearchSnapshotPolicy{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "my-search-daily"}, got))
		assert.Equal(t, testName, got.Spec.OpensearchRef.Name)
		assert.Equal(t, "my-search-daily", got.Spec.PolicyName)
		assert.Equal(t, "my-storage", got.Spec.SnapshotConfig.Repository)
		assert.Equal(t, "0 2 * * *", got.Spec.Creation.Schedule.Cron.Expression)
		assert.Nil(t, got.Spec.Deletion)
	})

	t.Run("disabled schedule creates nothing", func(t *testing.T) {
		in := instanceWithSchedule("my-storage", corev1alpha1.InstanceBackupSchedule{
			Name: "daily", Enabled: false, Cron: "0 2 * * *",
		})
		c, cl := newTestContext(t, in)

		require.NoError(t, syncScheduledBackups(c))

		list := &opensearchv1.OpensearchSnapshotPolicyList{}
		require.NoError(t, cl.List(context.Background(), list, client.InNamespace(testNamespace)))
		assert.Empty(t, list.Items)
	})

	t.Run("retention copies sets a deletion condition", func(t *testing.T) {
		in := instanceWithSchedule("my-storage", corev1alpha1.InstanceBackupSchedule{
			Name: "daily", Enabled: true, Cron: "0 2 * * *", RetentionCopies: 7,
		})
		c, cl := newTestContext(t, in)

		require.NoError(t, syncScheduledBackups(c))

		got := &opensearchv1.OpensearchSnapshotPolicy{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "my-search-daily"}, got))
		require.NotNil(t, got.Spec.Deletion)
		require.NotNil(t, got.Spec.Deletion.DeleteCondition.MaxCount)
		assert.Equal(t, 7, *got.Spec.Deletion.DeleteCondition.MaxCount)
	})

	t.Run("removing a schedule deletes its stale policy", func(t *testing.T) {
		existing := &opensearchv1.OpensearchSnapshotPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-search-old", Namespace: testNamespace,
				Labels: map[string]string{instanceLabel: testName},
			},
		}
		c, cl := newTestContext(t, testInstance(validEngine()), existing)

		require.NoError(t, syncScheduledBackups(c))

		err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "my-search-old"}, &opensearchv1.OpensearchSnapshotPolicy{})
		assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "stale policy must be deleted")
	})
}

func TestMirrorScheduledBackups(t *testing.T) {
	t.Run("no schedules: no-op", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		assert.NoError(t, mirrorScheduledBackups(c))
	})

	t.Run("matching successful snapshot is mirrored", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"snapshots":[
				{"snapshot":"my-search-daily-2026.11.03","state":"SUCCESS"},
				{"snapshot":"my-search-daily-2026.11.02","state":"IN_PROGRESS"},
				{"snapshot":"unrelated-snap","state":"SUCCESS"}
			]}`))
		})
		in := instanceWithSchedule("my-storage", corev1alpha1.InstanceBackupSchedule{
			Name: "daily", Enabled: true, Cron: "0 2 * * *",
		})
		c, cl := newTestContext(t, in)

		require.NoError(t, mirrorScheduledBackups(c))

		got := &backupv1alpha1.Backup{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "my-search-daily-2026.11.03"}, got))
		assert.Equal(t, "daily", got.Spec.ScheduleName)
		assert.Equal(t, "my-storage", got.Spec.StorageRef.Name)
		assert.Equal(t, backupClassName, got.Spec.ClassRef.Name)
		assert.Equal(t, testName, got.Spec.Origin.InstanceRef.Name)

		err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "my-search-daily-2026.11.02"}, &backupv1alpha1.Backup{})
		assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "in-progress snapshot must not be mirrored yet")

		err = cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "unrelated-snap"}, &backupv1alpha1.Backup{})
		assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "snapshot from another schedule/policy must not be mirrored")
	})

	t.Run("mirroring twice is idempotent", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-search-daily-2026.11.03","state":"SUCCESS"}]}`))
		})
		in := instanceWithSchedule("my-storage", corev1alpha1.InstanceBackupSchedule{
			Name: "daily", Enabled: true, Cron: "0 2 * * *",
		})
		c, _ := newTestContext(t, in)

		require.NoError(t, mirrorScheduledBackups(c))
		require.NoError(t, mirrorScheduledBackups(c))
	})
}

func TestValidateBackupSchedules(t *testing.T) {
	t.Run("nil backup config", func(t *testing.T) {
		assert.NoError(t, validateBackupSchedules(nil))
	})

	t.Run("valid schedule name", func(t *testing.T) {
		cfg := &corev1alpha1.InstanceBackupSpec{Storages: []corev1alpha1.InstanceBackupStorage{{
			StorageRef: commonv1alpha1.ObjectRef{Name: "s"},
			Schedules:  []corev1alpha1.InstanceBackupSchedule{{Name: "daily-full"}},
		}}}

		assert.NoError(t, validateBackupSchedules(cfg))
	})

	t.Run("invalid schedule name", func(t *testing.T) {
		cfg := &corev1alpha1.InstanceBackupSpec{Storages: []corev1alpha1.InstanceBackupStorage{{
			StorageRef: commonv1alpha1.ObjectRef{Name: "s"},
			Schedules:  []corev1alpha1.InstanceBackupSchedule{{Name: "Daily_Full"}},
		}}}

		assert.ErrorContains(t, validateBackupSchedules(cfg), "invalid")
	})
}
