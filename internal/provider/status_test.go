package provider

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

func testCluster(replicas int32, st opensearchv1.ClusterStatus) *opensearchv1.OpenSearchCluster {
	return &opensearchv1.OpenSearchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
		Spec: opensearchv1.ClusterSpec{
			General:   opensearchv1.GeneralConfig{ServiceName: testName, Version: "3.8.0", HttpPort: httpPort},
			NodePools: []opensearchv1.NodePool{{Component: nodePoolName, Replicas: replicas}},
		},
		Status: st,
	}
}

func TestClusterStatus(t *testing.T) {
	details := controller.ConnectionDetails{Host: "h"}
	tests := []struct {
		name    string
		status  opensearchv1.ClusterStatus
		phase   corev1alpha1.InstancePhase
		details bool
	}{
		{
			name:   "not initialized",
			status: opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning},
			phase:  corev1alpha1.InstancePhaseProvisioning,
		},
		{
			name:    "green",
			status:  opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth},
			phase:   corev1alpha1.InstancePhaseReady,
			details: true,
		},
		{
			name:    "yellow",
			status:  opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchYellowHealth},
			phase:   corev1alpha1.InstancePhaseReady,
			details: true,
		},
		{
			name:    "red",
			status:  opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchRedHealth},
			phase:   corev1alpha1.InstancePhaseUpdating,
			details: true,
		},
		{
			name:    "upgrading",
			status:  opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseUpgrading, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth},
			phase:   corev1alpha1.InstancePhaseUpdating,
			details: true,
		},
		{
			name:    "scaling",
			status:  opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 2, Health: opensearchv1.OpenSearchGreenHealth},
			phase:   corev1alpha1.InstancePhaseUpdating,
			details: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d controller.ConnectionDetails
			if tt.status.Initialized {
				d = details
			}

			got := clusterStatus(testCluster(3, tt.status), d)

			assert.Equal(t, tt.phase, got.Phase)
			assert.Equal(t, tt.details, !got.ConnectionDetails.IsEmpty())
		})
	}
}

func TestConnectionDetails(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("p@ss:w/rd!"),
	}}

	got := connectionDetails(testCluster(3, opensearchv1.ClusterStatus{}), secret)

	assert.Equal(t, "opensearch", got.Type)
	assert.Equal(t, "my-search.db.svc", got.Host)
	assert.Equal(t, "9200", got.Port)
	assert.Equal(t, "admin", got.Username)
	assert.Equal(t, "p@ss:w/rd!", got.Password)

	u, err := url.Parse(got.URI)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "my-search.db.svc:9200", u.Host)
	pass, _ := u.User.Password()
	assert.Equal(t, "p@ss:w/rd!", pass, "the password must survive URI encoding")
}

func TestStatus(t *testing.T) {
	t.Run("cluster not created yet", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.Equal(t, corev1alpha1.InstancePhaseProvisioning, got.Phase)
	})

	t.Run("ready with credentials", func(t *testing.T) {
		cluster := testCluster(3, opensearchv1.ClusterStatus{
			Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth,
		})
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminSecretName(testName), Namespace: testNamespace},
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("secret")},
		}
		c, _ := newTestContext(t, testInstance(validEngine()), cluster, secret)

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.Equal(t, corev1alpha1.InstancePhaseReady, got.Phase)
		assert.Equal(t, "secret", got.ConnectionDetails.Password)
	})

	t.Run("initialized without admin secret requeues", func(t *testing.T) {
		cluster := testCluster(3, opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true})
		c, _ := newTestContext(t, testInstance(validEngine()), cluster)

		_, err := New().Status(c)

		assert.Error(t, err)
	})
}
