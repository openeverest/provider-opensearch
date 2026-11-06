package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// withTestOpenSearchClient points newOpenSearchClientFunc at a client wired
// to a test server for the duration of the test, then restores it.
func withTestOpenSearchClient(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	oc, server := testOpenSearchServer(t, handler)
	original := newOpenSearchClientFunc
	newOpenSearchClientFunc = func(*controller.Context) (*openSearchClient, error) { return oc, nil }
	t.Cleanup(func() { newOpenSearchClientFunc = original })
	return server
}

func testBackup(storageName string, deletionPolicy backupv1alpha1.BackupDeletionPolicy) *backupv1alpha1.Backup {
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "my-backup", Namespace: testNamespace},
		Spec: backupv1alpha1.BackupSpec{
			Origin:         backupv1alpha1.BackupOrigin{Type: backupv1alpha1.BackupOriginTypeInstance},
			ClassRef:       commonv1alpha1.ObjectRef{Name: "opensearch-snapshot"},
			StorageRef:     commonv1alpha1.ObjectRef{Name: "my-storage"},
			DeletionPolicy: deletionPolicy,
		},
	}
}

// runningCluster is an OpenSearchCluster the operator has bootstrapped.
func runningCluster() *opensearchv1.OpenSearchCluster {
	cluster := existingCluster("3.8.0", nil)
	cluster.Status.Initialized = true
	cluster.Status.Phase = opensearchv1.PhaseRunning
	return cluster
}

func TestSyncBackup(t *testing.T) {
	t.Run("cluster not running yet: pending without calling the API", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not call the API")
		})
		c, _ := newTestContext(t, testInstance(validEngine()))

		got, err := New().SyncBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStatePending, got.State)
	})

	t.Run("snapshot does not exist yet: starts it and reports running", func(t *testing.T) {
		var putCalled bool
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPut:
				putCalled = true
				w.WriteHeader(http.StatusOK)
			}
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		got, err := New().SyncBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.True(t, putCalled)
		assert.Equal(t, backupv1alpha1.BackupStateRunning, got.State)
		assert.NotNil(t, got.StartedAt)
	})

	t.Run("snapshot in progress", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"IN_PROGRESS","start_time_in_millis":1000}]}`))
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		got, err := New().SyncBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateRunning, got.State)
		require.NotNil(t, got.StartedAt)
	})

	t.Run("snapshot succeeded: reports size", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/_snapshot/my-storage/my-backup":
				_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"SUCCESS","start_time_in_millis":1000,"end_time_in_millis":5000}]}`))
			case "/_snapshot/my-storage/my-backup/_status":
				_, _ = w.Write([]byte(`{"snapshots":[{"stats":{"total":{"size_in_bytes":999}}}]}`))
			}
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		got, err := New().SyncBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateSucceeded, got.State)
		require.NotNil(t, got.CompletedAt)
		require.NotNil(t, got.Size)
		assert.Equal(t, "999", *got.Size)
	})

	t.Run("snapshot failed", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-backup","state":"FAILED"}]}`))
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		got, err := New().SyncBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, got.State)
		assert.Contains(t, got.Message, "FAILED")
	})

	t.Run("unexpected API error is returned as an error", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		_, err := New().SyncBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		assert.Error(t, err)
	})
}

// startedBackup is a Backup whose snapshot was started.
func startedBackup(policy backupv1alpha1.BackupDeletionPolicy) *backupv1alpha1.Backup {
	b := testBackup("my-storage", policy)
	b.Status.StartedAt = ptr.To(metav1.Now())
	return b
}

func TestCleanupBackup(t *testing.T) {
	t.Run("never started: nothing to delete", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not call the API")
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		done, err := New().CleanupBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.True(t, done)
	})

	t.Run("deletes the snapshot when policy is Delete", func(t *testing.T) {
		var gotMethod, gotPath string
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusOK)
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		done, err := New().CleanupBackup(c, startedBackup(backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.True(t, done)
		assert.Equal(t, http.MethodDelete, gotMethod)
		assert.Equal(t, "/_snapshot/my-storage/my-backup", gotPath)
	})

	t.Run("cluster gone: leaves the snapshot and releases the finalizer", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not call the API")
		})
		c, _ := newTestContext(t, testInstance(validEngine()))

		done, err := New().CleanupBackup(c, startedBackup(backupv1alpha1.BackupDeletionPolicyDelete))

		require.NoError(t, err)
		assert.True(t, done)
	})

	t.Run("retain policy does not call the API", func(t *testing.T) {
		called := false
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		done, err := New().CleanupBackup(c, testBackup("my-storage", backupv1alpha1.BackupDeletionPolicyRetain))

		require.NoError(t, err)
		assert.True(t, done)
		assert.False(t, called)
	})
}
