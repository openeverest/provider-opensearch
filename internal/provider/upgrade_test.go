package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

func TestClusterUpgradeIssues(t *testing.T) {
	running := opensearchv1.ClusterStatus{
		Phase: opensearchv1.PhaseRunning, Initialized: true, Version: "3.8.0",
		AvailableNodes: 3, Health: opensearchv1.OpenSearchGreenHealth,
	}
	with := func(f func(*opensearchv1.ClusterStatus)) opensearchv1.ClusterStatus {
		st := running
		f(&st)
		return st
	}
	tests := []struct {
		name     string
		status   opensearchv1.ClusterStatus
		severity controller.UpgradeSeverity
		reason   string
	}{
		{name: "green and running", status: running},
		{
			name:     "upgrading phase",
			status:   with(func(st *opensearchv1.ClusterStatus) { st.Phase = opensearchv1.PhaseUpgrading }),
			severity: controller.UpgradeError, reason: reasonVersionUpgradeInProgress,
		},
		{
			name:     "upgrade requested but not started",
			status:   with(func(st *opensearchv1.ClusterStatus) { st.Version = "3.7.0" }),
			severity: controller.UpgradeError, reason: reasonVersionUpgradeInProgress,
		},
		{
			name: "upgrader still has a pool to do",
			status: with(func(st *opensearchv1.ClusterStatus) {
				st.ComponentsStatus = []opensearchv1.ComponentStatus{{Component: upgraderComponent, Status: "Pending"}}
			}),
			severity: controller.UpgradeError, reason: reasonVersionUpgradeInProgress,
		},
		{
			name: "upgrader finished",
			status: with(func(st *opensearchv1.ClusterStatus) {
				st.ComponentsStatus = []opensearchv1.ComponentStatus{{Component: upgraderComponent, Status: "Finished"}}
			}),
		},
		{
			name: "rolling restart",
			status: with(func(st *opensearchv1.ClusterStatus) {
				st.ComponentsStatus = []opensearchv1.ComponentStatus{{Component: rollingRestartComponent, Status: "InProgress"}}
			}),
			severity: controller.UpgradeError, reason: reasonRollingRestartInProgress,
		},
		{
			name:     "bootstrapping",
			status:   opensearchv1.ClusterStatus{Phase: opensearchv1.PhasePending},
			severity: controller.UpgradeWarning, reason: reasonClusterNotRunning,
		},
		{
			name:     "yellow",
			status:   with(func(st *opensearchv1.ClusterStatus) { st.Health = opensearchv1.OpenSearchYellowHealth }),
			severity: controller.UpgradeWarning, reason: reasonClusterNotGreen,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := clusterUpgradeIssues(testCluster(3, tt.status))
			if tt.reason == "" {
				assert.Empty(t, issues)
				return
			}
			if assert.Len(t, issues, 1) {
				assert.Equal(t, tt.severity, issues[0].Severity)
				assert.Equal(t, tt.reason, issues[0].Reason)
			}
		})
	}
}

func TestCheckUpgrade(t *testing.T) {
	restarting := testCluster(3, opensearchv1.ClusterStatus{
		Phase: opensearchv1.PhaseRunning, Initialized: true, Version: "3.8.0", Health: opensearchv1.OpenSearchGreenHealth,
		ComponentsStatus: []opensearchv1.ComponentStatus{{Component: rollingRestartComponent, Status: "InProgress"}},
	})
	deleting := testCluster(3, opensearchv1.ClusterStatus{Phase: opensearchv1.PhaseUpgrading})
	deleting.Name = "deleting"

	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(restarting, deleting).Build()
	h := controller.NewHookContext(context.Background(), cl, "provider-opensearch")

	now := metav1.Now()
	instances := []corev1alpha1.Instance{
		{ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace}},
		// Deleting Instances and Instances without a cluster yet are skipped.
		{ObjectMeta: metav1.ObjectMeta{Name: "deleting", Namespace: testNamespace, DeletionTimestamp: &now, Finalizers: []string{"x"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: testNamespace}},
	}

	issues := New().CheckUpgrade(h, nil, instances)
	if assert.Len(t, issues, 1) {
		assert.Equal(t, controller.UpgradeError, issues[0].Severity)
		assert.Equal(t, reasonRollingRestartInProgress, issues[0].Reason)
		assert.Equal(t, testName, issues[0].InstanceName)
		assert.Equal(t, testNamespace, issues[0].Namespace)
	}
}
