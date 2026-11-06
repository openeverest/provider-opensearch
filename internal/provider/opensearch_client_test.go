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
	tests := []struct {
		name     string
		response string
		status   int
		indices  []string
		want     restoreProgress
	}{
		{
			name:    "no target indices is done",
			indices: nil,
			want:    restoreDone,
		},
		{
			name:     "index not created yet",
			response: `{}`,
			indices:  []string{"idx-a"},
			want:     restoreRunning,
		},
		{
			name:    "404 while indices are recreated",
			status:  http.StatusNotFound,
			indices: []string{"idx-a"},
			want:    restoreRunning,
		},
		{
			name:     "primary still recovering from the snapshot",
			response: `{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"INDEX","primary":true}]}}`,
			indices:  []string{"idx-a"},
			want:     restoreRunning,
		},
		{
			name:     "one index restored, another not created yet",
			response: `{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"DONE","primary":true}]}}`,
			indices:  []string{"idx-a", "idx-b"},
			want:     restoreRunning,
		},
		{
			// Replicas of a restored index recover from a peer: they must
			// not keep the restore running.
			name: "primaries restored, replicas from peers",
			response: `{"idx-a":{"shards":[{"type":"SNAPSHOT","stage":"DONE","primary":true},{"type":"PEER","stage":"DONE","primary":false}]},` +
				`"idx-b":{"shards":[{"type":"SNAPSHOT","stage":"DONE","primary":true},{"type":"PEER","stage":"INDEX","primary":false}]}}`,
			indices: []string{"idx-a", "idx-b"},
			want:    restoreDone,
		},
		{
			// A node restarted after the restore recovers its primaries from
			// local disk.
			name:     "primary recovered from local disk after the restore",
			response: `{"idx-a":{"shards":[{"type":"EXISTING_STORE","stage":"DONE","primary":true}]}}`,
			indices:  []string{"idx-a"},
			want:     restoreDone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.indices == nil {
					t.Fatal("should not call the API")
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					return
				}
				_, _ = w.Write([]byte(tt.response))
			})

			progress, err := oc.restoreRecoveryStatus(tt.indices)

			require.NoError(t, err)
			assert.Equal(t, tt.want, progress)
		})
	}
}

func TestRestorableIndices(t *testing.T) {
	got := restorableIndices([]string{
		"books", ".opendistro_security", ".kibana_1", ".plugins-ml-config",
		"security-auditlog-2026.11.05", "top_queries-2026.11.05-89255", "logs-2026",
	})

	assert.Equal(t, []string{"books", "logs-2026"}, got)
}

func TestOpenSearchClientRegisterRepository(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	oc, _ := testOpenSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
	})

	err := oc.registerRepository("s3-from-src", "s3", map[string]string{"bucket": "b", "readonly": "true"})

	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/_snapshot/s3-from-src", gotPath)
	assert.Equal(t, "s3", gotBody["type"])
	assert.Equal(t, map[string]any{"bucket": "b", "readonly": "true"}, gotBody["settings"])
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
