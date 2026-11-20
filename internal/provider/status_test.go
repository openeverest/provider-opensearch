package provider

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
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
	want := clusterWant{version: "3.8.0", nodes: 3}
	running := func(nodes int32, health opensearchv1.OpenSearchHealth) opensearchv1.ClusterStatus {
		return opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true, Version: "3.8.0", AvailableNodes: nodes, Health: health}
	}
	tests := []struct {
		name      string
		status    opensearchv1.ClusterStatus
		want      clusterWant
		nodes     nodeRollout
		waiting   bool
		available controller.Availability
		operation string
		problem   string
	}{
		{
			name:    "not initialized",
			status:  opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning},
			waiting: true,
		},
		{
			name:      "green",
			status:    running(3, opensearchv1.OpenSearchGreenHealth),
			available: controller.Available,
		},
		{
			name:      "yellow serves reads and writes",
			status:    running(3, opensearchv1.OpenSearchYellowHealth),
			available: controller.Available,
		},
		{
			name:      "red",
			status:    running(3, opensearchv1.OpenSearchRedHealth),
			waiting:   true,
			available: controller.Unavailable,
			problem:   reasonPrimaryShardsUnassigned,
		},
		{
			name:      "health not reported",
			status:    running(3, ""),
			waiting:   true,
			available: controller.AvailabilityUnknown,
		},
		{
			name: "upgrading",
			status: opensearchv1.ClusterStatus{
				Phase: opensearchv1.PhaseUpgrading, Initialized: true, Version: "3.7.0", AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth,
			},
			waiting:   true,
			available: controller.Available,
			operation: corev1alpha1.ConditionUpgrading,
		},
		{
			// The operator has not picked up the new version yet: the cached
			// cluster can still be from before Sync applied it.
			name:      "version not reached yet",
			status:    running(3, opensearchv1.OpenSearchGreenHealth),
			want:      clusterWant{version: "3.9.0", nodes: 3},
			waiting:   true,
			available: controller.Available,
			operation: corev1alpha1.ConditionUpgrading,
		},
		{
			// Between two node restarts every node is up and the cluster green.
			name: "rolling restart in progress",
			status: func() opensearchv1.ClusterStatus {
				st := running(3, opensearchv1.OpenSearchGreenHealth)
				st.ComponentsStatus = []opensearchv1.ComponentStatus{{Component: "RollingRestart", Status: "InProgress"}}
				return st
			}(),
			waiting:   true,
			available: controller.Available,
		},
		{
			name: "rolling restart finished",
			status: func() opensearchv1.ClusterStatus {
				st := running(3, opensearchv1.OpenSearchGreenHealth)
				st.ComponentsStatus = []opensearchv1.ComponentStatus{{Component: "RollingRestart", Status: "Finished"}}
				return st
			}(),
			available: controller.Available,
		},
		{
			// A pinned image: the operator reports the new version at once and
			// restarts the nodes one by one.
			name:      "nodes still on another image",
			status:    running(3, opensearchv1.OpenSearchGreenHealth),
			nodes:     nodeRollout{staleImage: 2},
			waiting:   true,
			available: controller.Available,
			operation: corev1alpha1.ConditionUpgrading,
		},
		{
			name:      "nodes not on the StatefulSet's latest revision",
			status:    running(3, opensearchv1.OpenSearchGreenHealth),
			nodes:     nodeRollout{restarting: 1},
			waiting:   true,
			available: controller.Available,
		},
		{
			name:      "scaling up",
			status:    running(2, opensearchv1.OpenSearchGreenHealth),
			waiting:   true,
			available: controller.Available,
		},
		{
			name:      "scaling down",
			status:    running(4, opensearchv1.OpenSearchGreenHealth),
			waiting:   true,
			available: controller.Available,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d controller.ConnectionDetails
			if tt.status.Initialized {
				d = details
			}
			w := want
			if tt.want != (clusterWant{}) {
				w = tt.want
			}

			got := clusterStatus(testCluster(3, tt.status), w, tt.nodes, d)

			assert.Equal(t, tt.waiting, got.Message != "", "message %q", got.Message)
			assert.Equal(t, tt.available, got.Available)
			assert.Equal(t, tt.operation, operationTypes(got))
			if tt.problem == "" {
				assert.Empty(t, got.Problems)
			} else {
				require.Len(t, got.Problems, 1)
				assert.Equal(t, tt.problem, got.Problems[0].Reason)
				assert.False(t, got.Problems[0].Terminal)
			}
			assert.Equal(t, tt.status.Initialized, !got.ConnectionDetails.IsEmpty())
		})
	}
}

func TestConnectionDetails(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("p@ss:w/rd!"),
	}}

	got := connectionDetails(testCluster(3, opensearchv1.ClusterStatus{}), secret, nil)

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

func TestConnectionDetailsDedicated(t *testing.T) {
	cluster := testCluster(3, opensearchv1.ClusterStatus{})
	cluster.Spec.Security = &opensearchv1.Security{Tls: &opensearchv1.TlsConfig{
		Http: &opensearchv1.TlsConfigHttp{CustomFQDN: ptr.To(clientServiceHost(testName, testNamespace))},
	}}

	got := connectionDetails(cluster, &corev1.Secret{}, nil)

	assert.Equal(t, "my-search-client.db.svc", got.Host)
}

func TestStatus(t *testing.T) {
	t.Run("cluster not created yet", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.False(t, got.Converged)
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
		assert.True(t, got.Converged)
		assert.Equal(t, "secret", got.ConnectionDetails.Password)
	})

	readyCluster := func() *opensearchv1.OpenSearchCluster {
		return testCluster(3, opensearchv1.ClusterStatus{
			Phase: opensearchv1.PhaseRunning, Initialized: true, AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth,
		})
	}
	adminSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminSecretName(testName), Namespace: testNamespace},
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("secret")},
		}
	}

	t.Run("load balancer address is published", func(t *testing.T) {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: exposedServiceName(testName), Namespace: testNamespace},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}},
			}},
		}
		c, _ := newTestContext(t, testInstance(validEngine()), readyCluster(), adminSecret(), svc)

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.Equal(t, "203.0.113.7", got.ConnectionDetails.Host)
		assert.Equal(t, "https://admin:secret@203.0.113.7:9200", got.ConnectionDetails.URI)
	})

	t.Run("load balancer without an address keeps the in-cluster host", func(t *testing.T) {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: exposedServiceName(testName), Namespace: testNamespace},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		}
		c, _ := newTestContext(t, testInstance(validEngine()), readyCluster(), adminSecret(), svc)

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.Equal(t, testName+"."+testNamespace+".svc", got.ConnectionDetails.Host)
	})

	t.Run("running restore reports restoring", func(t *testing.T) {
		restore := testRestore("my-backup")
		restore.Status.State = backupv1alpha1.RestoreStateRunning
		c, _ := newTestContext(t, testInstance(validEngine()), readyCluster(), adminSecret(), restore)

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.False(t, got.Converged)
		assert.Equal(t, corev1alpha1.ConditionRestoring, operationTypes(got))
		assert.Equal(t, controller.Unavailable, got.Available)
		assert.Equal(t, "secret", got.ConnectionDetails.Password)
	})

	t.Run("finished restore does not override", func(t *testing.T) {
		restore := testRestore("my-backup")
		restore.Status.State = backupv1alpha1.RestoreStateSucceeded
		c, _ := newTestContext(t, testInstance(validEngine()), readyCluster(), adminSecret(), restore)

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.True(t, got.Converged)
	})

	t.Run("initialized without admin secret requeues", func(t *testing.T) {
		cluster := testCluster(3, opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseRunning, Initialized: true})
		c, _ := newTestContext(t, testInstance(validEngine()), cluster)

		_, err := New().Status(c)

		assert.Error(t, err)
	})

	seeded := func() *corev1alpha1.Instance {
		in := testInstance(validEngine())
		in.Spec.DataSource = &backupv1alpha1.DataSource{
			Type:   backupv1alpha1.DataSourceTypeBackup,
			Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "my-backup"}},
		}
		return in
	}

	t.Run("seeding from the data source has not converged", func(t *testing.T) {
		c, _ := newTestContext(t, seeded(), readyCluster(), adminSecret())
		c.SetDataSourceStatus(controller.DataSourceStatus{Done: false, Message: "restoring from backup"})

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.False(t, got.Converged)
		// The runtime reports the seeding restore itself.
		assert.Empty(t, got.Operations)
	})

	t.Run("failed seeding is reported by the runtime", func(t *testing.T) {
		c, _ := newTestContext(t, seeded(), readyCluster(), adminSecret())
		c.SetDataSourceStatus(controller.DataSourceStatus{Done: true, State: controller.DataSourceStateFailed, Message: "source backup not found"})

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.True(t, got.Converged)
		assert.Empty(t, got.Problems)
	})

	t.Run("data source succeeded does not override", func(t *testing.T) {
		c, _ := newTestContext(t, seeded(), readyCluster(), adminSecret())
		c.SetDataSourceStatus(controller.DataSourceStatus{Done: true, State: controller.DataSourceStateSucceeded})

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.True(t, got.Converged)
	})

	t.Run("volumes not grown to the requested size report StorageResizing", func(t *testing.T) {
		engine := validEngine()
		engine.Storage.Size = resource.MustParse("20Gi")
		pvc := func(ordinal int, capacity string) *corev1.PersistentVolumeClaim {
			return &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("data-%s-%s-%d", testName, nodePoolName, ordinal), Namespace: testNamespace},
				Status: corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(capacity),
				}},
			}
		}
		c, _ := newTestContext(t, testInstance(engine), readyCluster(), adminSecret(), pvc(0, "20Gi"), pvc(1, "10Gi"))

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.False(t, got.Converged)
		assert.Equal(t, corev1alpha1.ConditionStorageResizing, operationTypes(got))
		assert.Contains(t, got.Operations[0].Message, pvc(1, "10Gi").Name)
		assert.Equal(t, controller.Available, got.Available)
	})

	t.Run("desired replicas per component", func(t *testing.T) {
		in := testInstance(validEngine())
		in.Spec.Components[common.ComponentDashboards] = corev1alpha1.ComponentSpec{}
		c, _ := newTestContext(t, in)

		got, err := New().Status(c)

		require.NoError(t, err)
		assert.ElementsMatch(t, []controller.ComponentReplicas{
			{Component: common.ComponentEngine, Desired: 3},
			{Component: common.ComponentDashboards, Desired: 1},
		}, got.DesiredReplicas)
	})
}

// operationTypes returns the condition type of the single operation s reports.
func operationTypes(s controller.Status) string {
	if len(s.Operations) != 1 {
		return ""
	}
	return s.Operations[0].Type
}

func TestObserveNodes(t *testing.T) {
	cluster := testCluster(3, opensearchv1.ClusterStatus{})
	cluster.Spec.General.ImageSpec = &opensearchv1.ImageSpec{Image: ptr.To("opensearchproject/opensearch:3.8.0")}
	pod := func(name, image string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{
				clusterLabel: testName, nodePoolLabel: nodePoolName,
			}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: opensearchContainer, Image: image}}},
		}
	}
	bootstrap := pod("bootstrap", "opensearchproject/opensearch:3.7.0")
	delete(bootstrap.Labels, nodePoolLabel)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: testName + "-" + nodePoolName, Namespace: testNamespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](3)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1},
	}
	c, _ := newTestContext(t, testInstance(validEngine()), cluster,
		pod("n0", "opensearchproject/opensearch:3.8.0"),
		pod("n1", "opensearchproject/opensearch:3.7.0"),
		pod("n2", "opensearchproject/opensearch:3.7.0"),
		bootstrap, sts)

	got, err := observeNodes(c, cluster)

	require.NoError(t, err)
	assert.Equal(t, nodeRollout{staleImage: 2, restarting: 2}, got)
}
