package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

func TestRolloutGate(t *testing.T) {
	ctx := context.Background()
	key := client.ObjectKey{Namespace: testNamespace, Name: testName}

	getCluster := func(t *testing.T, cl client.Client) *opensearchv1.OpenSearchCluster {
		t.Helper()
		got := &opensearchv1.OpenSearchCluster{}
		require.NoError(t, cl.Get(ctx, key, got))
		return got
	}
	// syncAgain runs Sync on the stored Instance, as the runtime would.
	syncAgain := func(t *testing.T, cl client.Client) *controller.Context {
		t.Helper()
		in := &corev1alpha1.Instance{}
		require.NoError(t, cl.Get(ctx, key, in))
		c := controller.NewContext(ctx, cl, in, common.ProviderName)
		require.NoError(t, New().Sync(c))
		return c
	}
	// releaseNewDefault simulates a provider upgrade moving the default
	// OpenSearch version, which the Instance does not pin.
	releaseNewDefault := func(t *testing.T, cl client.Client) {
		t.Helper()
		p := &corev1alpha1.Provider{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: common.ProviderName}, p))
		ct := p.Spec.ComponentTypes[common.ComponentTypeOpensearch]
		ct.DefaultVersion = "3.7.0"
		p.Spec.ComponentTypes[common.ComponentTypeOpensearch] = ct
		p.Spec.Release = &corev1alpha1.Release{Version: "0.4.0"}
		require.NoError(t, cl.Update(ctx, p))
	}
	updateInstance := func(t *testing.T, cl client.Client, mutate func(*corev1alpha1.Instance)) {
		t.Helper()
		in := &corev1alpha1.Instance{}
		require.NoError(t, cl.Get(ctx, key, in))
		mutate(in)
		require.NoError(t, cl.Update(ctx, in))
	}
	created := func(t *testing.T) client.Client {
		t.Helper()
		c, cl := newTestContext(t, testInstance(validEngine()))
		require.NoError(t, New().Sync(c))
		require.Equal(t, "3.8.0", getCluster(t, cl).Spec.General.Version)
		return cl
	}

	t.Run("records what it applies", func(t *testing.T) {
		cl := created(t)
		state := &corev1.Secret{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: rolloutStateName(testName)}, state))
		assert.NotEmpty(t, state.Data[rolloutSpecKey])
		assert.NotEmpty(t, state.Data[rolloutInputsKey])
		require.Len(t, state.OwnerReferences, 1)

		c := syncAgain(t, cl)
		assert.Empty(t, c.GetPendingMaintenance())
	})

	t.Run("holds a provider-initiated change", func(t *testing.T) {
		cl := created(t)
		releaseNewDefault(t, cl)

		c := syncAgain(t, cl)

		assert.Equal(t, "3.8.0", getCluster(t, cl).Spec.General.Version)
		pending := c.GetPendingMaintenance()
		require.Len(t, pending, 1)
		assert.Equal(t, controller.MaintenanceRollingRestart, pending[0].Severity)
		assert.Regexp(t, `^provider-0\.4\.0-[0-9a-f]{8}$`, pending[0].ApprovalToken)

		// The token is stable while the change is pending.
		again := syncAgain(t, cl).GetPendingMaintenance()
		require.Len(t, again, 1)
		assert.Equal(t, pending[0].ApprovalToken, again[0].ApprovalToken)
	})

	t.Run("applies a provider-initiated change once approved", func(t *testing.T) {
		cl := created(t)
		releaseNewDefault(t, cl)
		token := syncAgain(t, cl).GetPendingMaintenance()[0].ApprovalToken

		updateInstance(t, cl, func(in *corev1alpha1.Instance) {
			in.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: token}
		})
		c := syncAgain(t, cl)

		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, "3.7.0", getCluster(t, cl).Spec.General.Version)

		// Once applied, nothing is pending even without the approval.
		updateInstance(t, cl, func(in *corev1alpha1.Instance) { in.Spec.Maintenance = nil })
		assert.Empty(t, syncAgain(t, cl).GetPendingMaintenance())
	})

	t.Run("applies a provider-initiated change within the tolerance", func(t *testing.T) {
		cl := created(t)
		releaseNewDefault(t, cl)
		updateInstance(t, cl, func(in *corev1alpha1.Instance) {
			in.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{AutoApproveUpTo: corev1alpha1.MaintenanceRollingRestart}
		})

		c := syncAgain(t, cl)

		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, "3.7.0", getCluster(t, cl).Spec.General.Version)
	})

	t.Run("applies a user change right away", func(t *testing.T) {
		cl := created(t)
		updateInstance(t, cl, func(in *corev1alpha1.Instance) {
			engine := in.Spec.Components[common.ComponentEngine]
			engine.Replicas = ptr.To[int32](5)
			in.Spec.Components[common.ComponentEngine] = engine
		})

		c := syncAgain(t, cl)

		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, int32(5), getCluster(t, cl).Spec.NodePools[0].Replicas)
	})

	t.Run("a change unrelated to the cluster does not release a held one", func(t *testing.T) {
		cl := created(t)
		releaseNewDefault(t, cl)
		updateInstance(t, cl, func(in *corev1alpha1.Instance) {
			in.Spec.DeletionPolicy = corev1alpha1.InstanceDeletionPolicyOrphan
		})

		c := syncAgain(t, cl)

		assert.Len(t, c.GetPendingMaintenance(), 1)
		assert.Equal(t, "3.8.0", getCluster(t, cl).Spec.General.Version)
	})

	t.Run("takes a cluster without a record as the baseline", func(t *testing.T) {
		existing := &opensearchv1.OpenSearchCluster{}
		existing.Name, existing.Namespace = testName, testNamespace
		existing.Spec.General.Version = "3.7.0"
		c, cl := newTestContext(t, testInstance(validEngine()), existing)

		require.NoError(t, New().Sync(c))

		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, "3.8.0", getCluster(t, cl).Spec.General.Version)
	})
}
