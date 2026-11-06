package provider

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// caSecretName is the operator-generated secret holding the cluster's CA
// certificate (key "ca.crt"), used to verify the HTTPS endpoint.
func caSecretName(clusterName string) string {
	return clusterName + "-ca"
}

// openSearchClient is an authenticated HTTPS client for the cluster's own
// REST API (not the Kubernetes API), used for actions the operator's CRD has
// no field for: on-demand snapshots. Shared by backup, restore and status.
type openSearchClient struct {
	http     *http.Client
	baseURL  string
	username string
	password string
}

// newOpenSearchClient builds a client for the cluster's REST API using the
// operator-generated admin credentials and CA certificate.
func newOpenSearchClient(c *controller.Context) (*openSearchClient, error) {
	adminSecret := &corev1.Secret{}
	if err := c.Get(adminSecret, adminSecretName(c.Name())); err != nil {
		return nil, fmt.Errorf("get admin credentials secret: %w", err)
	}

	caSecret := &corev1.Secret{}
	if err := c.Get(caSecret, caSecretName(c.Name())); err != nil {
		return nil, fmt.Errorf("get CA secret: %w", err)
	}
	caCert := caSecret.Data["ca.crt"]
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("CA secret %q: ca.crt does not contain a valid certificate", caSecretName(c.Name()))
	}

	return &openSearchClient{
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
		baseURL:  fmt.Sprintf("https://%s.%s.svc:%d", c.Name(), c.Namespace(), httpPort),
		username: string(adminSecret.Data["username"]),
		password: string(adminSecret.Data["password"]),
	}, nil
}

// errStatus carries a non-2xx HTTP response so callers can distinguish "not
// found" from other failures.
type errStatus struct {
	statusCode int
	body       string
}

func (e *errStatus) Error() string {
	return fmt.Sprintf("opensearch API: unexpected status %d: %s", e.statusCode, e.body)
}

// isNotFound reports whether err is an errStatus for HTTP 404, mirroring
// apierrors.IsNotFound for the Kubernetes API.
func isNotFound(err error) bool {
	var es *errStatus
	return errors.As(err, &es) && es.statusCode == http.StatusNotFound
}

// do sends an authenticated request and decodes a JSON response into out
// (when non-nil). A non-2xx response is returned as *errStatus.
func (oc *openSearchClient) do(method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, oc.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(oc.username, oc.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := oc.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &errStatus{statusCode: resp.StatusCode, body: string(respBody)}
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// snapshotState mirrors the "state" field OpenSearch reports for a snapshot.
type snapshotState string

const (
	snapshotStateInProgress   snapshotState = "IN_PROGRESS"
	snapshotStateSuccess      snapshotState = "SUCCESS"
	snapshotStatePartial      snapshotState = "PARTIAL"
	snapshotStateFailed       snapshotState = "FAILED"
	snapshotStateIncompatible snapshotState = "INCOMPATIBLE"
)

type snapshotInfo struct {
	Snapshot          string        `json:"snapshot"`
	State             snapshotState `json:"state"`
	StartTimeInMillis int64         `json:"start_time_in_millis"`
	EndTimeInMillis   int64         `json:"end_time_in_millis"`
	Indices           []string      `json:"indices"`
}

type getSnapshotResponse struct {
	Snapshots []snapshotInfo `json:"snapshots"`
}

// createSnapshot starts a new snapshot (PUT _snapshot/<repo>/<name>). The
// call returns once the operation is accepted; the snapshot itself proceeds
// asynchronously and is polled via getSnapshot.
func (oc *openSearchClient) createSnapshot(repo, name string) error {
	return oc.do(http.MethodPut, fmt.Sprintf("/_snapshot/%s/%s", repo, name), map[string]any{
		"indices":              "*",
		"ignore_unavailable":   true,
		"include_global_state": true,
	}, nil)
}

// getSnapshot returns the snapshot's current state. Returns an error
// satisfying isNotFound if the snapshot does not exist (not yet created, or
// already deleted).
func (oc *openSearchClient) getSnapshot(repo, name string) (*snapshotInfo, error) {
	var resp getSnapshotResponse
	if err := oc.do(http.MethodGet, fmt.Sprintf("/_snapshot/%s/%s", repo, name), nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Snapshots) == 0 {
		return nil, &errStatus{statusCode: http.StatusNotFound, body: "no snapshots returned"}
	}
	return &resp.Snapshots[0], nil
}

// listSnapshots returns every snapshot in a repository
// (GET _snapshot/<repo>/_all). An empty or not-yet-registered repository
// returns an empty slice rather than an error.
func (oc *openSearchClient) listSnapshots(repo string) ([]snapshotInfo, error) {
	var resp getSnapshotResponse
	if err := oc.do(http.MethodGet, fmt.Sprintf("/_snapshot/%s/_all", repo), nil, &resp); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return resp.Snapshots, nil
}

// snapshotStatusResponse is the richer payload from
// GET _snapshot/<repo>/<name>/_status, used only to report size once a
// snapshot has finished, since getSnapshot does not carry it.
type snapshotStatusResponse struct {
	Snapshots []struct {
		Stats struct {
			Total struct {
				SizeInBytes int64 `json:"size_in_bytes"`
			} `json:"total"`
		} `json:"stats"`
	} `json:"snapshots"`
}

// getSnapshotSize returns the total snapshot size in bytes. Only meaningful
// once the snapshot has finished (successfully or not); calling it on a
// running snapshot is more expensive than getSnapshot and is not needed
// while polling.
func (oc *openSearchClient) getSnapshotSize(repo, name string) (int64, error) {
	var resp snapshotStatusResponse
	if err := oc.do(http.MethodGet, fmt.Sprintf("/_snapshot/%s/%s/_status", repo, name), nil, &resp); err != nil {
		return 0, err
	}
	if len(resp.Snapshots) == 0 {
		return 0, nil
	}
	return resp.Snapshots[0].Stats.Total.SizeInBytes, nil
}

// deleteSnapshot deletes a snapshot (DELETE _snapshot/<repo>/<name>). Not
// finding it is treated as success, matching apierrors.IsNotFound idioms
// used elsewhere in this provider.
func (oc *openSearchClient) deleteSnapshot(repo, name string) error {
	err := oc.do(http.MethodDelete, fmt.Sprintf("/_snapshot/%s/%s", repo, name), nil, nil)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// closeIndices closes the given indices so a snapshot restore can recreate
// them. Restoring over an open index of the same name is rejected by
// OpenSearch; a closed (or missing) index is not. Missing indices are
// ignored, so this is safe to call before a first-ever restore.
func (oc *openSearchClient) closeIndices(indices []string) error {
	if len(indices) == 0 {
		return nil
	}
	path := fmt.Sprintf("/%s/_close?expand_wildcards=all&ignore_unavailable=true", strings.Join(indices, ","))
	return oc.do(http.MethodPost, path, nil, nil)
}

// restoreSnapshot starts restoring the given indices from a snapshot
// (POST _snapshot/<repo>/<name>/_restore). Indices must already be closed
// (or not exist). The call returns once accepted; restore proceeds
// asynchronously and is tracked via restoreRecoveryStatus.
//
// include_global_state is always false: restoring it is rejected by the
// security plugin for basic-auth users regardless of role (confirmed live —
// even the admin user's all_access role gets a security_exception on
// cluster:admin/snapshot/restore with include_global_state: true; only a
// certificate-authenticated superadmin can do it). This is also the safer
// default for an in-place restore: global state includes cluster-wide
// settings, templates and the security config, which restoring could
// silently overwrite with their snapshot-time values, including the
// cluster's current users and roles.
func (oc *openSearchClient) restoreSnapshot(repo, name string, indices []string) error {
	idx := "*"
	if len(indices) > 0 {
		idx = strings.Join(indices, ",")
	}
	return oc.do(http.MethodPost, fmt.Sprintf("/_snapshot/%s/%s/_restore", repo, name), map[string]any{
		"indices":              idx,
		"include_global_state": false,
	}, nil)
}

// registerRepository creates or updates a snapshot repository
// (PUT _snapshot/<name>). Used for the read-only repositories that expose
// another Instance's snapshots; the Instance's own repositories are managed by
// the operator from spec.general.snapshotRepositories.
func (oc *openSearchClient) registerRepository(name, repoType string, settings map[string]string) error {
	return oc.do(http.MethodPut, "/_snapshot/"+name, map[string]any{
		"type":     repoType,
		"settings": settings,
	}, nil)
}

// restoreProgress classifies how far a snapshot restore of a set of indices
// has progressed.
type restoreProgress int

const (
	// restoreRunning means at least one target index is not restored yet.
	restoreRunning restoreProgress = iota
	// restoreDone means every primary shard of every target index is
	// recovered: from the snapshot, or from local disk after a node restart
	// that followed the restore.
	restoreDone
)

type recoveryShard struct {
	Type    string `json:"type"`
	Stage   string `json:"stage"`
	Primary bool   `json:"primary"`
}

type recoveryIndex struct {
	Shards []recoveryShard `json:"shards"`
}

// restoreRecoveryStatus reports the progress of a restore that was already
// started, using the shard recovery API. Only primaries are looked at:
// replicas of a restored index always recover from a peer. An index missing
// from the response is closed or not created yet, so still restoring.
func (oc *openSearchClient) restoreRecoveryStatus(indices []string) (restoreProgress, error) {
	if len(indices) == 0 {
		return restoreDone, nil
	}

	var resp map[string]recoveryIndex
	path := fmt.Sprintf("/%s/_recovery?expand_wildcards=all&ignore_unavailable=true", strings.Join(indices, ","))
	if err := oc.do(http.MethodGet, path, nil, &resp); err != nil {
		if isNotFound(err) {
			return restoreRunning, nil
		}
		return restoreRunning, err
	}

	for _, name := range indices {
		entry, ok := resp[name]
		if !ok {
			return restoreRunning, nil
		}
		primaries := 0
		for _, shard := range entry.Shards {
			if !shard.Primary {
				continue
			}
			primaries++
			if shard.Stage != "DONE" {
				return restoreRunning, nil
			}
		}
		if primaries == 0 {
			return restoreRunning, nil
		}
	}
	return restoreDone, nil
}

// restorableIndices returns the snapshot's indices a restore brings back:
// user data only. System indices (a leading dot, e.g. .opendistro_security)
// and the indices plugins write on their own are left alone, so the restored
// cluster keeps its own users, roles and plugin state; the security plugin
// rejects restoring its index for a non-certificate admin anyway.
func restorableIndices(indices []string) []string {
	out := make([]string, 0, len(indices))
	for _, name := range indices {
		if strings.HasPrefix(name, ".") || slices.ContainsFunc(pluginIndexPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// pluginIndexPrefixes are indices OpenSearch plugins create in every cluster
// (audit log, query insights).
var pluginIndexPrefixes = []string{
	"security-auditlog-",
	"top_queries-",
}
