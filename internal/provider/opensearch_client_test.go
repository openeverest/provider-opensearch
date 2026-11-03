package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testOpenSearchServer starts a TLS test server and returns a client wired
// to trust it, so request/response handling is tested without touching the
// real CA-secret plumbing (covered separately in TestNewOpenSearchClient).
func testOpenSearchServer(t *testing.T, handler http.HandlerFunc) (*openSearchClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return &openSearchClient{
		http:     server.Client(),
		baseURL:  server.URL,
		username: "admin",
		password: "secret",
	}, server
}

func TestOpenSearchClientCreateSnapshot(t *testing.T) {
	var gotPath, gotUser, gotPass string
	var gotBody map[string]any
	oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
	})

	err := oc.createSnapshot("my-repo", "my-snap")

	require.NoError(t, err)
	assert.Equal(t, "/_snapshot/my-repo/my-snap", gotPath)
	assert.Equal(t, "admin", gotUser)
	assert.Equal(t, "secret", gotPass)
	assert.Equal(t, "*", gotBody["indices"])
}

func TestOpenSearchClientGetSnapshot(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/_snapshot/my-repo/my-snap", r.URL.Path)
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"my-snap","state":"SUCCESS","start_time_in_millis":1000,"end_time_in_millis":2000}]}`))
		})

		info, err := oc.getSnapshot("my-repo", "my-snap")

		require.NoError(t, err)
		assert.Equal(t, snapshotStateSuccess, info.State)
		assert.Equal(t, int64(1000), info.StartTimeInMillis)
		assert.Equal(t, int64(2000), info.EndTimeInMillis)
	})

	t.Run("not found", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"snapshot_missing_exception"}}`))
		})

		_, err := oc.getSnapshot("my-repo", "my-snap")

		require.Error(t, err)
		assert.True(t, isNotFound(err))
	})

	t.Run("empty snapshots list is treated as not found", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"snapshots":[]}`))
		})

		_, err := oc.getSnapshot("my-repo", "my-snap")

		require.Error(t, err)
		assert.True(t, isNotFound(err))
	})
}

func TestOpenSearchClientListSnapshots(t *testing.T) {
	t.Run("lists all snapshots", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/_snapshot/my-repo/_all", r.URL.Path)
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":"a","state":"SUCCESS"},{"snapshot":"b","state":"IN_PROGRESS"}]}`))
		})

		snaps, err := oc.listSnapshots("my-repo")

		require.NoError(t, err)
		require.Len(t, snaps, 2)
		assert.Equal(t, "a", snaps[0].Snapshot)
		assert.Equal(t, "b", snaps[1].Snapshot)
	})

	t.Run("not found repository returns an empty slice", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})

		snaps, err := oc.listSnapshots("ghost-repo")

		require.NoError(t, err)
		assert.Empty(t, snaps)
	})
}

func TestOpenSearchClientGetSnapshotSize(t *testing.T) {
	oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/_snapshot/my-repo/my-snap/_status", r.URL.Path)
		_, _ = w.Write([]byte(`{"snapshots":[{"stats":{"total":{"size_in_bytes":12345}}}]}`))
	})

	size, err := oc.getSnapshotSize("my-repo", "my-snap")

	require.NoError(t, err)
	assert.Equal(t, int64(12345), size)
}

func TestOpenSearchClientDeleteSnapshot(t *testing.T) {
	t.Run("deletes", func(t *testing.T) {
		var gotMethod string
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			w.WriteHeader(http.StatusOK)
		})

		err := oc.deleteSnapshot("my-repo", "my-snap")

		require.NoError(t, err)
		assert.Equal(t, http.MethodDelete, gotMethod)
	})

	t.Run("already gone is not an error", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})

		err := oc.deleteSnapshot("my-repo", "my-snap")

		assert.NoError(t, err)
	})
}

func TestOpenSearchClientCloseIndices(t *testing.T) {
	t.Run("closes the given indices", func(t *testing.T) {
		var gotMethod, gotPath string
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path+"?"+r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
		})

		err := oc.closeIndices([]string{"idx-a", "idx-b"})

		require.NoError(t, err)
		assert.Equal(t, http.MethodPost, gotMethod)
		assert.Equal(t, "/idx-a,idx-b/_close?expand_wildcards=all&ignore_unavailable=true", gotPath)
	})

	t.Run("no indices is a no-op", func(t *testing.T) {
		called := false
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) { called = true })

		err := oc.closeIndices(nil)

		require.NoError(t, err)
		assert.False(t, called)
	})
}

func TestOpenSearchClientRestoreSnapshot(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
	})

	err := oc.restoreSnapshot("my-repo", "my-snap", []string{"idx-a", "idx-b"})

	require.NoError(t, err)
	assert.Equal(t, "/_snapshot/my-repo/my-snap/_restore", gotPath)
	assert.Equal(t, "idx-a,idx-b", gotBody["indices"])
	assert.Equal(t, false, gotBody["include_global_state"],
		"restoring global state is rejected by the security plugin for basic-auth users")
}

func TestOpenSearchClientRestoreRecoveryStatus(t *testing.T) {
	t.Run("no target indices is done", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not call the API")
		})

		progress, err := oc.restoreRecoveryStatus(nil)

		require.NoError(t, err)
		assert.Equal(t, restoreDone, progress)
	})

	t.Run("not started: indices missing from the response", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})

		progress, err := oc.restoreRecoveryStatus([]string{"idx-a"})

		require.NoError(t, err)
		assert.Equal(t, restoreNotStarted, progress)
	})

	t.Run("not started: 404", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})

		progress, err := oc.restoreRecoveryStatus([]string{"idx-a"})

		require.NoError(t, err)
		assert.Equal(t, restoreNotStarted, progress)
	})

	t.Run("running: shard not done yet", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"INDEX"}]}}`))
		})

		progress, err := oc.restoreRecoveryStatus([]string{"idx-a"})

		require.NoError(t, err)
		assert.Equal(t, restoreRunning, progress)
	})

	t.Run("running: some indices still missing", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"DONE"}]}}`))
		})

		progress, err := oc.restoreRecoveryStatus([]string{"idx-a", "idx-b"})

		require.NoError(t, err)
		assert.Equal(t, restoreRunning, progress)
	})

	t.Run("done: all indices, all shards DONE from SNAPSHOT", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"DONE"}]},"idx-b":{"shards":[{"type":"SNAPSHOT","stage":"DONE"}]}}`))
		})

		progress, err := oc.restoreRecoveryStatus([]string{"idx-a", "idx-b"})

		require.NoError(t, err)
		assert.Equal(t, restoreDone, progress)
	})

	t.Run("not from our restore: shard type is not SNAPSHOT", func(t *testing.T) {
		oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"idx-a":{"shards":[{"type":"PEER","stage":"DONE"}]}}`))
		})

		progress, err := oc.restoreRecoveryStatus([]string{"idx-a"})

		require.NoError(t, err)
		assert.Equal(t, restoreRunning, progress)
	})
}

func TestOpenSearchClientUnexpectedStatus(t *testing.T) {
	oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})

	err := oc.createSnapshot("my-repo", "my-snap")

	require.Error(t, err)
	assert.False(t, isNotFound(err))
	assert.ErrorContains(t, err, "500")
}

func TestNewOpenSearchClient(t *testing.T) {
	t.Run("missing admin secret", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		_, err := newOpenSearchClient(c)

		assert.ErrorContains(t, err, "admin credentials secret")
	})

	t.Run("missing CA secret", func(t *testing.T) {
		admin := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminSecretName(testName), Namespace: testNamespace},
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pw")},
		}
		c, _ := newTestContext(t, testInstance(validEngine()), admin)

		_, err := newOpenSearchClient(c)

		assert.ErrorContains(t, err, "CA secret")
	})

	t.Run("invalid CA certificate", func(t *testing.T) {
		admin := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminSecretName(testName), Namespace: testNamespace},
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pw")},
		}
		ca := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: caSecretName(testName), Namespace: testNamespace},
			Data:       map[string][]byte{"ca.crt": []byte("not a certificate")},
		}
		c, _ := newTestContext(t, testInstance(validEngine()), admin, ca)

		_, err := newOpenSearchClient(c)

		assert.ErrorContains(t, err, "does not contain a valid certificate")
	})
}
