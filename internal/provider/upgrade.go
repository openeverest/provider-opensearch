package provider

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

var _ controller.UpgradeProvider = (*Provider)(nil)

// Reasons reported on upgrade preflight issues.
const (
	reasonVersionUpgradeInProgress = "VersionUpgradeInProgress"
	reasonRollingRestartInProgress = "RollingRestartInProgress"
	reasonClusterNotRunning        = "ClusterNotRunning"
	reasonClusterNotGreen          = "ClusterNotGreen"
	reasonClusterUnverified        = "ClusterUnverified"
)

// upgraderComponent is the componentsStatus entry the operator keeps per node
// pool while it upgrades OpenSearch to a new version.
const upgraderComponent = "Upgrader"

// CheckUpgrade blocks a provider upgrade while any cluster is in the middle
// of a version upgrade or a rolling restart: the new operator would take over
// a half-done node-by-node operation. A cluster that is not running or not
// green only warns, since the upgrade itself does not make it worse.
func (p *Provider) CheckUpgrade(h *controller.HookContext, _ *corev1alpha1.ProviderSpec, instances []corev1alpha1.Instance) []controller.UpgradeIssue {
	var issues []controller.UpgradeIssue
	for i := range instances {
		in := &instances[i]
		if in.DeletionTimestamp != nil {
			continue
		}
		cluster := &opensearchv1.OpenSearchCluster{}
		err := h.Client().Get(h.Context(), client.ObjectKey{Namespace: in.Namespace, Name: in.Name}, cluster)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			issues = append(issues, controller.UpgradeIssue{
				Severity:     controller.UpgradeWarning,
				InstanceName: in.Name,
				Namespace:    in.Namespace,
				Reason:       reasonClusterUnverified,
				Message:      fmt.Sprintf("cannot check the cluster state: %v", err),
			})
			continue
		}
		for _, issue := range clusterUpgradeIssues(cluster) {
			issue.InstanceName = in.Name
			issue.Namespace = in.Namespace
			issues = append(issues, issue)
		}
	}
	return issues
}

func clusterUpgradeIssues(cluster *opensearchv1.OpenSearchCluster) []controller.UpgradeIssue {
	st := cluster.Status
	if versionUpgradeInProgress(cluster) {
		return []controller.UpgradeIssue{{
			Severity: controller.UpgradeError,
			Reason:   reasonVersionUpgradeInProgress,
			Message: fmt.Sprintf("OpenSearch is being upgraded to %s; wait for the upgrade to finish before upgrading the provider",
				cluster.Spec.General.Version),
		}}
	}
	if rollingRestartInProgress(st) {
		return []controller.UpgradeIssue{{
			Severity: controller.UpgradeError,
			Reason:   reasonRollingRestartInProgress,
			Message:  "nodes are being restarted to apply a configuration change; wait for the restart to finish before upgrading the provider",
		}}
	}
	if !st.Initialized || st.Phase != opensearchv1.PhaseRunning {
		return []controller.UpgradeIssue{{
			Severity: controller.UpgradeWarning,
			Reason:   reasonClusterNotRunning,
			Message:  "the cluster is not running yet; the upgrade can proceed, but check the cluster afterwards",
		}}
	}
	if st.Health != opensearchv1.OpenSearchGreenHealth {
		return []controller.UpgradeIssue{{
			Severity: controller.UpgradeWarning,
			Reason:   reasonClusterNotGreen,
			Message: fmt.Sprintf("cluster health is %q; if the upgrade restarts nodes, data on unassigned shards may be unavailable until they recover",
				healthOrUnknown(st.Health)),
		}}
	}
	return nil
}

// versionUpgradeInProgress reports whether the operator has an OpenSearch
// version upgrade underway or still to start.
func versionUpgradeInProgress(cluster *opensearchv1.OpenSearchCluster) bool {
	st := cluster.Status
	if st.Phase == opensearchv1.PhaseUpgrading {
		return true
	}
	if st.Initialized && st.Version != "" && st.Version != cluster.Spec.General.Version {
		return true
	}
	for _, cs := range st.ComponentsStatus {
		if cs.Component == upgraderComponent && cs.Status != "Upgraded" && cs.Status != "Finished" {
			return true
		}
	}
	return false
}
