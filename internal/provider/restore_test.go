package provider

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
)

func testRestore(backupName string) *backupv1alpha1.Restore {
	return &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "my-restore", Namespace: testNamespace},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: testName},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: backupName}},
			},
		},
	}
}

func TestSyncRestore(t *testing.T) {
	t.Run("rejects point-in-time data source", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))
		restore := testRestore("my-backup")
		restore.Spec.DataSource = backupv1alpha1.DataSource{Type: backupv1alpha1.DataSourceTypePointInTime}

		_, err := New().SyncRestore(c, restore)

		assert.ErrorContains(t, err, "point-in-time")
	})

	t.Run("source backup does not exist yet", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		got, err := New().SyncRestore(c, testRestore("ghost"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, got.State)
	})

	t.Run("source snapshot missing from repository", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		backup := testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete)
		c, _ := newTestContext(t, testInstance(validEngine()), backup)

		got, err := New().SyncRestore(c, testRestore(backup.Name))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateFailed, got.State)
	})

	t.Run("source snapshot not finished yet", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"IN_PROGRESS"}]}`))
		})
		backup := testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete)
		c, _ := newTestContext(t, testInstance(validEngine()), backup)

		got, err := New().SyncRestore(c, testRestore(backup.Name))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, got.State)
	})

	t.Run("not started: closes indices and starts the restore", func(t *testing.T) {
		var closedPath, restoredPath string
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/_snapshot/my-storage/my-backup":
				_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"SUCCESS","indices":["idx-a"]}]}`))
			case r.Method == http.MethodGet && r.URL.Path == "/idx-a/_recovery":
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodPost && r.URL.Path == "/idx-a/_close":
				closedPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodPost && r.URL.Path == "/_snapshot/my-storage/my-backup/_restore":
				restoredPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
			}
		})
		backup := testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete)
		c, _ := newTestContext(t, testInstance(validEngine()), backup)

		got, err := New().SyncRestore(c, testRestore(backup.Name))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateRunning, got.State)
		require.NotNil(t, got.StartedAt)
		assert.Equal(t, "/idx-a/_close", closedPath)
		assert.Equal(t, "/_snapshot/my-storage/my-backup/_restore", restoredPath)
	})

	t.Run("running: does not re-trigger", func(t *testing.T) {
		var closeOrRestoreCalled bool
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/_snapshot/my-storage/my-backup":
				_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"SUCCESS","indices":["idx-a"]}]}`))
			case "/idx-a/_recovery":
				_, _ = w.Write([]byte(`{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"INDEX"}]}}`))
			default:
				closeOrRestoreCalled = true
			}
		})
		backup := testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete)
		c, _ := newTestContext(t, testInstance(validEngine()), backup)

		got, err := New().SyncRestore(c, testRestore(backup.Name))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateRunning, got.State)
		assert.False(t, closeOrRestoreCalled)
	})

	t.Run("done: all indices restored", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/_snapshot/my-storage/my-backup":
				_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"SUCCESS","indices":["idx-a"]}]}`))
			case "/idx-a/_recovery":
				_, _ = w.Write([]byte(`{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"DONE"}]}}`))
			}
		})
		backup := testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete)
		c, _ := newTestContext(t, testInstance(validEngine()), backup)

		got, err := New().SyncRestore(c, testRestore(backup.Name))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateSucceeded, got.State)
		require.NotNil(t, got.CompletedAt)
	})
}

func TestCleanupRestore(t *testing.T) {
	done, err := New().CleanupRestore(nil, nil)

	require.NoError(t, err)
	assert.True(t, done)
}
