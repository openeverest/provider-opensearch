package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// Compile-time interface check.
var _ controller.MonitoringSourcesProvider = (*Provider)(nil)

const (
	// exporterPluginBase is where the opensearch-project Prometheus exporter
	// plugin is released, one build per OpenSearch version. OpenSearch serves
	// no OpenMetrics without it.
	exporterPluginBase = "https://github.com/opensearch-project/opensearch-prometheus-exporter/releases/download/"
	// metricsPath is where the plugin serves the metrics, on the HTTP port.
	metricsPath = "/_prometheus/metrics"
	// metricsProfile names the credential profile scrapers log in with.
	metricsProfile = "metrics"
	// httpPortName is the container port the operator names for HTTP.
	httpPortName = "http"
)

// exporterPlugin is the Prometheus exporter plugin build for version.
func exporterPlugin(version string) string {
	return fmt.Sprintf("%s%[2]s.0/prometheus-exporter-%[2]s.0.zip", exporterPluginBase, version)
}

// hasExporter reports whether the cluster runs the exporter plugin.
func hasExporter(cluster *opensearchv1.OpenSearchCluster) bool {
	for _, plugin := range cluster.Spec.General.PluginsList {
		if strings.HasPrefix(plugin, exporterPluginBase) {
			return true
		}
	}
	return false
}

// exporterEnabled reports whether the cluster is to run the exporter: while
// a bound MonitoringClass needs metrics, or spec.monitoring.sources asks for
// them always. While that is unresolved (a destination or class is missing
// or not accepted yet), the live cluster keeps what it has, so its nodes do
// not restart while a class is reinstalled.
func exporterEnabled(c *controller.Context, live *opensearchv1.OpenSearchCluster) (bool, error) {
	req, err := c.RequiredSources()
	if err != nil {
		return false, fmt.Errorf("monitoring sources: %w", err)
	}
	return req.Metrics.Else(hasExporter(live)), nil
}

// Permissions of the monitoring role: what the exporter reads, the cluster's
// and every index's statistics, including system indices, and no documents.
// The built-in readall_and_monitor role is not enough: it lacks
// indices:monitor/stats on the system indices, so the exporter fails.
var (
	monitoringClusterPermissions = []string{"cluster_monitor"}
	monitoringIndexPermissions   = []opensearchv1.IndexPermissionSpec{{
		IndexPatterns:  []string{"*"},
		AllowedActions: []string{"indices_monitor"},
	}}
)

// monitoringUserName names the read-only user the exporter's scrapers log
// in as, its OpensearchUser, role and role binding, and the Secret holding
// its credentials. The operator names the user and the role after their
// resources.
func monitoringUserName(clusterName string) string {
	return clusterName + "-monitoring"
}

// syncMonitoringUser keeps the read-only monitoring user while the exporter
// runs, and removes it otherwise. Its password is generated once.
//
// The user and its role binding are rendered once the cluster is
// initialized: the operator creates them through the cluster's REST API, and
// retries one it could not create with a growing backoff, which would delay
// the metrics long after the cluster is up. The status change that marks the
// cluster initialized triggers the reconcile that renders them.
func syncMonitoringUser(c *controller.Context, enabled, initialized bool) error {
	name := monitoringUserName(c.Name())
	user := &opensearchv1.OpensearchUser{ObjectMeta: c.ObjectMeta(name)}
	role := &opensearchv1.OpensearchRole{ObjectMeta: c.ObjectMeta(name)}
	binding := &opensearchv1.OpensearchUserRoleBinding{ObjectMeta: c.ObjectMeta(name)}
	secret := &corev1.Secret{ObjectMeta: c.ObjectMeta(name)}
	if !enabled {
		if err := c.Delete(binding); err != nil {
			return err
		}
		if err := c.Delete(role); err != nil {
			return err
		}
		if err := c.Delete(user); err != nil {
			return err
		}
		return c.Delete(secret)
	}

	existing := &corev1.Secret{}
	password := ""
	if err := c.Get(existing, name); err == nil {
		password = string(existing.Data[corev1alpha1.CredentialSecretKeyPassword])
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get monitoring credentials: %w", err)
	}
	if password == "" {
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("generate monitoring password: %w", err)
		}
		password = hex.EncodeToString(buf)
	}
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{
		corev1alpha1.CredentialSecretKeyUsername: []byte(name),
		corev1alpha1.CredentialSecretKeyPassword: []byte(password),
	}
	if err := c.Apply(secret); err != nil {
		return fmt.Errorf("apply monitoring credentials: %w", err)
	}
	if !initialized {
		return nil
	}

	cluster := corev1.LocalObjectReference{Name: c.Name()}
	user.Spec = opensearchv1.OpensearchUserSpec{
		OpensearchRef: cluster,
		PasswordFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Key:                  corev1alpha1.CredentialSecretKeyPassword,
		},
	}
	if err := c.Apply(user); err != nil {
		return fmt.Errorf("apply monitoring user: %w", err)
	}
	role.Spec = opensearchv1.OpensearchRoleSpec{
		OpensearchRef:      cluster,
		ClusterPermissions: monitoringClusterPermissions,
		IndexPermissions:   monitoringIndexPermissions,
	}
	if err := c.Apply(role); err != nil {
		return fmt.Errorf("apply monitoring role: %w", err)
	}
	binding.Spec = opensearchv1.OpensearchUserRoleBindingSpec{
		OpensearchRef: cluster,
		Roles:         []string{name},
		Users:         []string{name},
	}
	if err := c.Apply(binding); err != nil {
		return fmt.Errorf("apply monitoring role binding: %w", err)
	}
	return nil
}

// MonitoringSources publishes the exporter's endpoint on every OpenSearch
// node pool once it serves: the plugin runs on every node, and by default
// reports the statistics of the node it runs on. The endpoint is HTTPS,
// verified with the CA of the HTTP certificate, and authenticates with the
// monitoring user.
func (p *Provider) MonitoringSources(c *controller.Context) (*corev1alpha1.MonitoringSources, error) {
	cluster := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(cluster, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !hasExporter(cluster) || !exporterServing(cluster) {
		return nil, nil
	}
	user := &opensearchv1.OpensearchUser{}
	if err := c.Get(user, monitoringUserName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if user.Status.State != opensearchv1.OpensearchUserStateCreated {
		return nil, nil
	}

	ca, err := httpCASecretRef(c)
	if err != nil {
		return nil, err
	}
	serverName := cluster.Spec.General.ServiceName + "." + c.Namespace() + ".svc"
	if sec := cluster.Spec.Security; sec != nil && sec.Tls != nil && sec.Tls.Http != nil &&
		sec.Tls.Http.CustomFQDN != nil && *sec.Tls.Http.CustomFQDN != "" {
		serverName = *sec.Tls.Http.CustomFQDN
	}

	sources := &corev1alpha1.MonitoringSources{
		Credentials: []corev1alpha1.MonitoringCredential{{
			Profile:   metricsProfile,
			SecretRef: commonv1alpha1.SecretRef{Name: monitoringUserName(c.Name())},
		}},
	}
	in := c.Instance()
	for _, def := range nodePoolDefs(topologyOf(in)) {
		if _, ok := in.Spec.Components[def.component]; !ok {
			continue
		}
		sources.Metrics = append(sources.Metrics, corev1alpha1.MetricsEndpoint{
			Component: def.component,
			PodSelector: map[string]string{
				controller.InstanceLabel: c.Name(),
				nodePoolLabel:            def.pool,
			},
			Port:   intstr.FromString(httpPortName),
			Path:   metricsPath,
			Scheme: "https",
			TLS: &corev1alpha1.MetricsEndpointTLS{
				TLSConfig:  commonv1alpha1.TLSConfig{CA: &commonv1alpha1.CABundleSource{SecretKeyRef: ca}},
				ServerName: serverName,
			},
			Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: metricsProfile},
		})
	}
	return sources, nil
}

// exporterServing reports whether every node runs the plugin: the cluster
// is running, all its nodes are up, and no rolling restart, which installs
// the plugin node by node, is left.
func exporterServing(cluster *opensearchv1.OpenSearchCluster) bool {
	st := cluster.Status
	if !st.Initialized || st.Phase != opensearchv1.PhaseRunning || rollingRestartInProgress(st) {
		return false
	}
	var desired int32
	for _, pool := range cluster.Spec.NodePools {
		desired += pool.Replicas
	}
	return st.AvailableNodes == desired
}

// httpCASecretRef references the CA that verifies the HTTP certificate.
func httpCASecretRef(c *controller.Context) (*commonv1alpha1.SecretKeyRef, error) {
	t, err := instanceTLSParameters(c.Instance())
	if err != nil {
		return nil, err
	}
	name := caSecretName(c.Name())
	if customCertificate(t) {
		name = httpCertificateSecretName(c.Name(), t)
	}
	return &commonv1alpha1.SecretKeyRef{Name: name, Key: caKey}, nil
}
