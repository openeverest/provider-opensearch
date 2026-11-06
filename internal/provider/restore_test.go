package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
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

// sourceBackup is a Backup of origin, stored in my-storage.
func sourceBackup(origin string) *backupv1alpha1.Backup {
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "my-backup", Namespace: testNamespace},
		Spec: backupv1alpha1.BackupSpec{
			Origin:     backupv1alpha1.BackupOrigin{Type: backupv1alpha1.BackupOriginTypeInstance, InstanceRef: &commonv1alpha1.ObjectRef{Name: origin}},
			StorageRef: commonv1alpha1.ObjectRef{Name: "my-storage"},
		},
	}
}

// restoreAPI fakes the endpoints SyncRestore calls and records what it did.
type restoreAPI struct {
	snapshotState string
	indices       string
	recovery      string
	calls         []string
	registered    map[string]any
	restoreBody   map[string]any
}

func (a *restoreAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_recovery"):
			_, _ = w.Write([]byte(a.recovery))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/_snapshot/"):
			_, _ = fmt.Fprintf(w, `{"snapshots":[{"snapshot":"my-backup","state":%q,"indices":%s}]}`, a.snapshotState, a.indices)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/_snapshot/"):
			require.NoError(t, json.NewDecoder(r.Body).Decode(&a.registered))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_restore"):
			require.NoError(t, json.NewDecoder(r.Body).Decode(&a.restoreBody))
		}
	}
}

func TestSyncRestore(t *testing.T) {
	t.Run("rejects point-in-time data source", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))
		restore := testRestore("my-backup")
		restore.Spec.DataSource = backupv1alpha1.DataSource{Type: backupv1alpha1.DataSourceTypePointInTime}

		got, err := New().SyncRestore(c, restore)

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateFailed, got.State)
		assert.Contains(t, got.Message, "point-in-time")
	})

	t.Run("source backup does not exist yet", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster())

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, got.State)
	})

	t.Run("cluster not running yet", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not call the API")
		})
		c, _ := newTestContext(t, testInstance(validEngine()), sourceBackup(testName))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, got.State)
	})

	t.Run("source snapshot missing from repository", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup(testName))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateFailed, got.State)
	})

	t.Run("source snapshot not finished yet", func(t *testing.T) {
		api := &restoreAPI{snapshotState: "IN_PROGRESS", indices: `[]`}
		withTestOpenSearchClient(t, api.handler(t))
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup(testName))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, got.State)
	})

	t.Run("first sync closes and restores user indices only", func(t *testing.T) {
		api := &restoreAPI{snapshotState: "SUCCESS", indices: `["books",".opendistro_security","security-auditlog-2026.11.05"]`}
		withTestOpenSearchClient(t, api.handler(t))
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup(testName))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateRunning, got.State)
		require.NotNil(t, got.StartedAt)
		assert.Contains(t, api.calls, "POST /books/_close")
		assert.Contains(t, api.calls, "POST /_snapshot/my-storage/my-backup/_restore")
		assert.Equal(t, "books", api.restoreBody["indices"])
	})

	t.Run("open indices with peer replicas do not count as restored before the restore starts", func(t *testing.T) {
		// Before the fix this reported Running forever without restoring.
		api := &restoreAPI{
			snapshotState: "SUCCESS", indices: `["books"]`,
			recovery: `{"books":{"shards":[{"type":"EXISTING_STORE","stage":"DONE","primary":true},{"type":"PEER","stage":"DONE","primary":false}]}}`,
		}
		withTestOpenSearchClient(t, api.handler(t))
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup(testName))

		_, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Contains(t, api.calls, "POST /_snapshot/my-storage/my-backup/_restore")
	})

	t.Run("started: running while a primary is recovering", func(t *testing.T) {
		api := &restoreAPI{
			snapshotState: "SUCCESS", indices: `["books"]`,
			recovery: `{"books":{"shards":[{"type":"SNAPSHOT","stage":"INDEX","primary":true}]}}`,
		}
		withTestOpenSearchClient(t, api.handler(t))
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup(testName))
		restore := testRestore("my-backup")
		restore.Status.StartedAt = ptr.To(metav1.Now())

		got, err := New().SyncRestore(c, restore)

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateRunning, got.State)
		assert.NotContains(t, api.calls, "POST /_snapshot/my-storage/my-backup/_restore", "must not restore twice")
	})

	t.Run("started: succeeded once primaries are restored", func(t *testing.T) {
		api := &restoreAPI{
			snapshotState: "SUCCESS", indices: `["books"]`,
			recovery: `{"books":{"shards":[{"type":"SNAPSHOT","stage":"DONE","primary":true},{"type":"PEER","stage":"DONE","primary":false}]}}`,
		}
		withTestOpenSearchClient(t, api.handler(t))
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup(testName))
		restore := testRestore("my-backup")
		restore.Status.StartedAt = ptr.To(metav1.Now())

		got, err := New().SyncRestore(c, restore)

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateSucceeded, got.State)
		require.NotNil(t, got.CompletedAt)
	})

	t.Run("backup of another Instance is read through a read-only repository", func(t *testing.T) {
		api := &restoreAPI{snapshotState: "SUCCESS", indices: `["books"]`}
		withTestOpenSearchClient(t, api.handler(t))
		in := instanceWithBackup(corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: "my-storage"}})
		c, _ := newTestContext(t, in, runningCluster(), sourceBackup("source"), testBackupStorage("my-storage", validS3Spec()))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateRunning, got.State)
		assert.Contains(t, api.calls, "PUT /_snapshot/my-storage-from-source")
		settings := api.registered["settings"].(map[string]any)
		assert.Equal(t, testNamespace+"/source", settings["base_path"])
		assert.Equal(t, "true", settings["readonly"])
		assert.Contains(t, api.calls, "POST /_snapshot/my-storage-from-source/my-backup/_restore")
	})

	t.Run("failing to register the read-only repository is retried", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"repository_verification_exception"}}`))
		})
		in := instanceWithBackup(corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: "my-storage"}})
		c, _ := newTestContext(t, in, runningCluster(), sourceBackup("source"), testBackupStorage("my-storage", validS3Spec()))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, got.State)
	})

	t.Run("backup of another Instance needs its storage on this Instance", func(t *testing.T) {
		withTestOpenSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not call the API")
		})
		c, _ := newTestContext(t, testInstance(validEngine()), runningCluster(), sourceBackup("source"))

		got, err := New().SyncRestore(c, testRestore("my-backup"))

		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateFailed, got.State)
		assert.Contains(t, got.Message, "spec.backup.storages")
	})
}
