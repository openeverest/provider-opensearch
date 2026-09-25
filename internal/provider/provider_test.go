package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

const (
	testName      = "my-search"
	testNamespace = "db"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, opensearchv1.AddToScheme(scheme))
	return scheme
}

func testProvider() *corev1alpha1.Provider {
	return &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: common.ProviderName},
		Spec: corev1alpha1.ProviderSpec{
			ComponentTypes: map[string]corev1alpha1.ComponentType{
				common.ComponentTypeOpensearch: {Versions: []corev1alpha1.ComponentVersion{
					{Version: "3.7.0", Image: "opensearchproject/opensearch:3.7.0"},
					{Version: "3.8.0", Image: "opensearchproject/opensearch:3.8.0", Default: true},
				}},
			},
			Components: map[string]corev1alpha1.Component{
				common.ComponentEngine: {Type: common.ComponentTypeOpensearch},
			},
		},
	}
}

func testInstance(engine corev1alpha1.ComponentSpec) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace, UID: "uid"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Components:  map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
}

func validEngine() corev1alpha1.ComponentSpec {
	return corev1alpha1.ComponentSpec{
		Replicas: ptr.To[int32](3),
		Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
		Storage: &corev1alpha1.Storage{Size: resource.MustParse("10Gi")},
	}
}

func newTestContext(t *testing.T, in *corev1alpha1.Instance, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	objs = append(objs, in, testProvider())
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&opensearchv1.OpenSearchCluster{}).
		Build()
	return controller.NewContext(context.Background(), cl, in, common.ProviderName), cl
}

func TestBuildClusterSpec(t *testing.T) {
	engine := validEngine()
	engine.Storage.StorageClass = ptr.To("fast")

	spec := buildClusterSpec(testName, engine, "3.8.0", "opensearchproject/opensearch:3.8.0")

	assert.Equal(t, "3.8.0", spec.General.Version)
	assert.Equal(t, testName, spec.General.ServiceName)
	assert.Equal(t, httpPort, spec.General.HttpPort)
	require.NotNil(t, spec.General.ImageSpec)
	assert.Equal(t, "opensearchproject/opensearch:3.8.0", *spec.General.Image)
	assert.True(t, spec.ConfMgmt.SmartScaler)

	require.NotNil(t, spec.Security)
	assert.True(t, spec.Security.Tls.Transport.Generate)
	assert.True(t, spec.Security.Tls.Http.Generate)

	require.Len(t, spec.NodePools, 1)
	pool := spec.NodePools[0]
	assert.Equal(t, nodePoolName, pool.Component)
	assert.Equal(t, int32(3), pool.Replicas)
	assert.Equal(t, []string{"cluster_manager", "data", "ingest"}, pool.Roles)
	assert.Equal(t, resource.MustParse("10Gi"), pool.DiskSize)
	require.NotNil(t, pool.Persistence.PVC)
	assert.Equal(t, ptr.To("fast"), pool.Persistence.PVC.StorageClassName)
	assert.Equal(t, defaultAffinity(testName), pool.Affinity)
}

func TestBuildClusterSpecDefaults(t *testing.T) {
	spec := buildClusterSpec(testName, corev1alpha1.ComponentSpec{}, "3.8.0", "")

	assert.Nil(t, spec.General.ImageSpec, "an empty image lets the operator derive it from the version")
	assert.Equal(t, defaultReplicas, spec.NodePools[0].Replicas)
	assert.True(t, spec.NodePools[0].DiskSize.IsZero())
}

func TestBuildNodePoolSchedulingPolicy(t *testing.T) {
	affinity := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	engine := validEngine()
	engine.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{
		Affinity:     affinity,
		NodeSelector: map[string]string{"disk": "ssd"},
		Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone},
		},
	}

	pool := buildNodePool(testName, engine)

	assert.Same(t, affinity, pool.Affinity)
	assert.Equal(t, map[string]string{"disk": "ssd"}, pool.NodeSelector)
	assert.Len(t, pool.Tolerations, 1)
	assert.Len(t, pool.TopologySpreadConstraints, 1)
}

func TestNodeResources(t *testing.T) {
	t.Run("requests default to limits", func(t *testing.T) {
		got := nodeResources(&corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
		})
		assert.Equal(t, resource.MustParse("4Gi"), got.Requests[corev1.ResourceMemory])
	})

	t.Run("explicit requests are kept", func(t *testing.T) {
		got := nodeResources(&corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
		})
		assert.Equal(t, resource.MustParse("3Gi"), got.Requests[corev1.ResourceMemory])
	})

	t.Run("input is not mutated", func(t *testing.T) {
		in := &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		}
		nodeResources(in)
		assert.Nil(t, in.Requests)
	})

	t.Run("nil", func(t *testing.T) {
		assert.Equal(t, corev1.ResourceRequirements{}, nodeResources(nil))
	})
}

func TestSync(t *testing.T) {
	t.Run("creates the cluster with the resolved version", func(t *testing.T) {
		engine := validEngine()
		engine.Version = "3.7.0"
		c, cl := newTestContext(t, testInstance(engine))

		require.NoError(t, New().Sync(c))

		got := &opensearchv1.OpenSearchCluster{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, got))
		assert.Equal(t, "3.7.0", got.Spec.General.Version)
		assert.Equal(t, "opensearchproject/opensearch:3.7.0", *got.Spec.General.Image)
		require.Len(t, got.OwnerReferences, 1)
		assert.Equal(t, testName, got.OwnerReferences[0].Name)
	})

	t.Run("falls back to the default version", func(t *testing.T) {
		c, cl := newTestContext(t, testInstance(validEngine()))

		require.NoError(t, New().Sync(c))

		got := &opensearchv1.OpenSearchCluster{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, got))
		assert.Equal(t, "3.8.0", got.Spec.General.Version)
	})

	t.Run("image override wins", func(t *testing.T) {
		engine := validEngine()
		engine.Image = "registry.local/opensearch:3.8.0"
		c, cl := newTestContext(t, testInstance(engine))

		require.NoError(t, New().Sync(c))

		got := &opensearchv1.OpenSearchCluster{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, got))
		assert.Equal(t, "registry.local/opensearch:3.8.0", *got.Spec.General.Image)
	})

	t.Run("keeps the operator finalizer", func(t *testing.T) {
		existing := &opensearchv1.OpenSearchCluster{ObjectMeta: metav1.ObjectMeta{
			Name: testName, Namespace: testNamespace, Finalizers: []string{"Opensearch"},
		}}
		c, cl := newTestContext(t, testInstance(validEngine()), existing)

		require.NoError(t, New().Sync(c))

		got := &opensearchv1.OpenSearchCluster{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, got))
		assert.Equal(t, []string{"Opensearch"}, got.Finalizers)
	})
}

func TestCleanup(t *testing.T) {
	t.Run("deletes the cluster and unowned secrets", func(t *testing.T) {
		cluster := &opensearchv1.OpenSearchCluster{ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace}}
		secrets := []client.Object{cluster}
		for _, name := range unownedSecretNames(testName) {
			secrets = append(secrets, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
		}
		c, cl := newTestContext(t, testInstance(validEngine()), secrets...)

		require.NoError(t, New().Cleanup(c))

		for _, name := range unownedSecretNames(testName) {
			err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &corev1.Secret{})
			assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "secret %s must be deleted", name)
		}
	})

	t.Run("waits while the operator finalizer holds the cluster", func(t *testing.T) {
		cluster := &opensearchv1.OpenSearchCluster{ObjectMeta: metav1.ObjectMeta{
			Name: testName, Namespace: testNamespace, Finalizers: []string{"Opensearch"},
		}}
		admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: adminSecretName(testName), Namespace: testNamespace}}
		c, cl := newTestContext(t, testInstance(validEngine()), cluster, admin)

		err := New().Cleanup(c)

		assert.True(t, controller.IsWaitError(err), "expected a wait error, got %v", err)
		require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(admin), &corev1.Secret{}),
			"secrets must survive until the cluster is gone")
	})
}
