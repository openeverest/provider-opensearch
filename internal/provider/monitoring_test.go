package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// alwaysMetrics asks for the metrics endpoints with no destination bound.
func alwaysMetrics(in *corev1alpha1.Instance) *corev1alpha1.Instance {
	in.Spec.Monitoring = &corev1alpha1.InstanceMonitoringSpec{Sources: &corev1alpha1.InstanceMonitoringSources{
		Metrics: &corev1alpha1.InstanceMetricsSource{Policy: corev1alpha1.MonitoringSourcePolicyAlways},
	}}
	return in
}

func withExporter(cluster *opensearchv1.OpenSearchCluster) *opensearchv1.OpenSearchCluster {
	cluster.Spec.General.PluginsList = append(cluster.Spec.General.PluginsList, exporterPlugin("3.8.0"))
	return cluster
}

func TestExporterEnabled(t *testing.T) {
	t.Run("no destination needs metrics", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		enabled, err := exporterEnabled(c, &opensearchv1.OpenSearchCluster{})

		require.NoError(t, err)
		assert.False(t, enabled)
	})

	t.Run("metrics policy Always", func(t *testing.T) {
		c, _ := newTestContext(t, alwaysMetrics(testInstance(validEngine())))

		enabled, err := exporterEnabled(c, &opensearchv1.OpenSearchCluster{})

		require.NoError(t, err)
		assert.True(t, enabled)
	})

	t.Run("a destination not resolved yet keeps the live exporter", func(t *testing.T) {
		in := testInstance(validEngine())
		in.Spec.Monitoring = &corev1alpha1.InstanceMonitoringSpec{Destinations: []corev1alpha1.InstanceMonitoringDestination{{Name: "prom"}}}
		c, _ := newTestContext(t, in)

		enabled, err := exporterEnabled(c, withExporter(&opensearchv1.OpenSearchCluster{}))

		require.NoError(t, err)
		assert.True(t, enabled)
	})
}

func TestBuildClusterSpecExporter(t *testing.T) {
	spec := buildClusterSpec(clusterInputs{
		name: testName, namespace: testNamespace, topology: common.TopologyStandard,
		components:    map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: validEngine()},
		version:       "3.8.0",
		backupPlugins: []string{s3Plugin},
		exporter:      true,
	})

	assert.Equal(t, []string{
		s3Plugin,
		"https://github.com/opensearch-project/opensearch-prometheus-exporter/releases/download/3.8.0.0/prometheus-exporter-3.8.0.0.zip",
	}, spec.General.PluginsList)
	require.Len(t, spec.NodePools, 1)
	assert.Equal(t, map[string]string{controller.InstanceLabel: testName}, spec.NodePools[0].Labels)
	assert.True(t, hasExporter(&opensearchv1.OpenSearchCluster{Spec: spec}))
}

func TestSyncMonitoringUser(t *testing.T) {
	c, _ := newTestContext(t, testInstance(validEngine()))
	name := monitoringUserName(testName)

	// The operator could not create the user before the cluster is up.
	require.NoError(t, syncMonitoringUser(c, true, false))
	exists, err := c.Exists(&opensearchv1.OpensearchUser{}, name)
	require.NoError(t, err)
	assert.False(t, exists)

	require.NoError(t, syncMonitoringUser(c, true, true))

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(secret, name))
	assert.Equal(t, name, string(secret.Data["username"]))
	password := secret.Data["password"]
	assert.Len(t, password, 48)
	user := &opensearchv1.OpensearchUser{}
	require.NoError(t, c.Get(user, name))
	assert.Equal(t, testName, user.Spec.OpensearchRef.Name)
	assert.Equal(t, name, user.Spec.PasswordFrom.Name)
	role := &opensearchv1.OpensearchRole{}
	require.NoError(t, c.Get(role, name))
	assert.Equal(t, []string{"cluster_monitor"}, role.Spec.ClusterPermissions)
	assert.Equal(t, []opensearchv1.IndexPermissionSpec{{IndexPatterns: []string{"*"}, AllowedActions: []string{"indices_monitor"}}},
		role.Spec.IndexPermissions)
	binding := &opensearchv1.OpensearchUserRoleBinding{}
	require.NoError(t, c.Get(binding, name))
	assert.Equal(t, []string{name}, binding.Spec.Roles)
	assert.Equal(t, []string{name}, binding.Spec.Users)

	require.NoError(t, syncMonitoringUser(c, true, true))
	require.NoError(t, c.Get(secret, name))
	assert.Equal(t, password, secret.Data["password"], "the password is generated once")

	require.NoError(t, syncMonitoringUser(c, false, true))
	exists, err = c.Exists(&opensearchv1.OpensearchUser{}, name)
	require.NoError(t, err)
	assert.False(t, exists)
	exists, err = c.Exists(&opensearchv1.OpensearchRole{}, name)
	require.NoError(t, err)
	assert.False(t, exists)
	exists, err = c.Exists(&corev1.Secret{}, name)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestMonitoringSources(t *testing.T) {
	serving := func() *opensearchv1.OpenSearchCluster {
		cluster := withExporter(testCluster(3, opensearchv1.ClusterStatus{
			Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth,
		}))
		cluster.Namespace = testNamespace
		return cluster
	}
	user := func(state opensearchv1.OpensearchUserState) *opensearchv1.OpensearchUser {
		return &opensearchv1.OpensearchUser{
			ObjectMeta: metav1.ObjectMeta{Name: monitoringUserName(testName), Namespace: testNamespace},
			Status:     opensearchv1.OpensearchUserStatus{State: state},
		}
	}

	t.Run("serving exporter", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()), serving(), user(opensearchv1.OpensearchUserStateCreated))

		got, err := New().MonitoringSources(c)

		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, []corev1alpha1.MonitoringCredential{{
			Profile: metricsProfile, SecretRef: commonv1alpha1.SecretRef{Name: monitoringUserName(testName)},
		}}, got.Credentials)
		require.Len(t, got.Metrics, 1)
		m := got.Metrics[0]
		assert.Equal(t, common.ComponentEngine, m.Component)
		assert.Equal(t, map[string]string{controller.InstanceLabel: testName, nodePoolLabel: nodePoolName}, m.PodSelector)
		assert.Equal(t, intstr.FromString("http"), m.Port)
		assert.Equal(t, "/_prometheus/metrics", m.Path)
		assert.Equal(t, "https", m.Scheme)
		require.NotNil(t, m.TLS)
		assert.Equal(t, caSecretName(testName), m.TLS.CA.SecretKeyRef.Name)
		assert.Equal(t, testName+"."+testNamespace+".svc", m.TLS.ServerName)
		assert.Equal(t, metricsProfile, m.Auth.CredentialProfile)
	})

	t.Run("nothing while the plugin is installed node by node", func(t *testing.T) {
		cluster := serving()
		cluster.Status.ComponentsStatus = []opensearchv1.ComponentStatus{{Component: "RollingRestart", Status: "InProgress"}}
		c, _ := newTestContext(t, testInstance(validEngine()), cluster, user(opensearchv1.OpensearchUserStateCreated))

		got, err := New().MonitoringSources(c)

		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("nothing until the monitoring user is created", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()), serving(), user(opensearchv1.OpensearchUserStatePending))

		got, err := New().MonitoringSources(c)

		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("nothing without the exporter", func(t *testing.T) {
		cluster := serving()
		cluster.Spec.General.PluginsList = nil
		c, _ := newTestContext(t, testInstance(validEngine()), cluster, user(opensearchv1.OpensearchUserStateCreated))

		got, err := New().MonitoringSources(c)

		require.NoError(t, err)
		assert.Nil(t, got)
	})
}
