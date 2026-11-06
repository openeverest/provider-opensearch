package provider

import (
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

const (
	// nodePoolName is the OpenSearchCluster node pool backing the engine
	// component. The operator uses it in StatefulSet and Service names, so it
	// must never change for existing clusters.
	nodePoolName = "nodes"

	httpPort int32 = 9200

	defaultReplicas int32 = 3

	// defaultDashboardsReplicas matches the operator's own CRD default
	// (kubebuilder:default=1 on DashboardsConfig.Replicas); set explicitly
	// here so buildDashboardsConfig's output does not depend on it.
	defaultDashboardsReplicas int32 = 1

	// Pod labels set by the operator on every OpenSearch node.
	clusterLabel  = "opensearch.org/opensearch-cluster"
	nodePoolLabel = "opensearch.org/opensearch-nodepool"
)

// standardRoles are the node roles of the standard topology: every node is
// cluster-manager eligible, holds data and runs ingest pipelines.
var standardRoles = []string{"cluster_manager", "data", "ingest"}

// adminSecretName is the operator-generated secret with the admin user's
// credentials (keys "username" and "password").
func adminSecretName(clusterName string) string {
	return clusterName + "-admin-password"
}

// unownedSecretNames lists the secrets the operator generates without an
// owner reference, so they outlive the OpenSearchCluster unless deleted.
func unownedSecretNames(clusterName string) []string {
	return []string{
		adminSecretName(clusterName),
		clusterName + "-dashboards-password",
	}
}

// buildClusterSpec maps the engine component of an Instance onto an
// OpenSearchCluster spec for the standard topology.
func buildClusterSpec(
	name string,
	engine corev1alpha1.ComponentSpec,
	version, image string,
	additionalConfig map[string]string,
	snapshotRepos []opensearchv1.SnapshotRepoConfig,
	keystore []opensearchv1.KeystoreValue,
	backupPlugins []string,
) opensearchv1.ClusterSpec {
	general := opensearchv1.GeneralConfig{
		Version:              version,
		ServiceName:          name,
		HttpPort:             httpPort,
		SnapshotRepositories: snapshotRepos,
		Keystore:             keystore,
		PluginsList:          backupPlugins,
		// The PVCs hold the only copy of the data; drop them with the cluster
		// but keep them on scale-down so scaling back up reuses them.
		PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
	}
	if image != "" {
		general.ImageSpec = &opensearchv1.ImageSpec{Image: &image}
	}
	if engine.Service != nil {
		general.Annotations = engine.Service.Annotations
	}

	return opensearchv1.ClusterSpec{
		General: general,
		// Sent explicitly: the CRD default does not apply to a serialized false.
		ConfMgmt: opensearchv1.ConfMgmt{SmartScaler: true},
		Security: &opensearchv1.Security{
			Tls: &opensearchv1.TlsConfig{
				Transport: &opensearchv1.TlsConfigTransport{Generate: true, PerNode: true},
				Http:      &opensearchv1.TlsConfigHttp{Generate: true},
			},
		},
		NodePools: []opensearchv1.NodePool{buildNodePool(name, engine, additionalConfig)},
	}
}

func buildNodePool(clusterName string, engine corev1alpha1.ComponentSpec, additionalConfig map[string]string) opensearchv1.NodePool {
	pool := opensearchv1.NodePool{
		Component:        nodePoolName,
		AdditionalConfig: additionalConfig,
		Replicas:         defaultReplicas,
		Roles:            standardRoles,
		Resources:        nodeResources(engine.Resources),
		Persistence: &opensearchv1.PersistenceConfig{
			PersistenceSource: opensearchv1.PersistenceSource{
				PVC: &opensearchv1.PVCSource{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			},
		},
	}
	if engine.Replicas != nil {
		pool.Replicas = *engine.Replicas
	}
	if engine.Storage != nil {
		pool.DiskSize = engine.Storage.Size
		pool.Persistence.PVC.StorageClassName = engine.Storage.StorageClass
	}

	var policy commonv1alpha1.SchedulingPolicy
	if engine.SchedulingPolicy != nil {
		policy = *engine.SchedulingPolicy
	}
	pool.NodeSelector = policy.NodeSelector
	pool.Tolerations = policy.Tolerations
	if policy.TopologySpreadConstraints != nil {
		pool.TopologySpreadConstraints = *policy.TopologySpreadConstraints
	}
	pool.Affinity = policy.Affinity
	if pool.Affinity == nil {
		pool.Affinity = defaultAffinity(clusterName)
	}

	return pool
}

// buildDashboardsConfig maps the optional dashboards component onto the
// OpenSearchCluster's embedded DashboardsConfig. TLS is always generated,
// consistent with the main cluster's always-on TLS.
func buildDashboardsConfig(dashboards corev1alpha1.ComponentSpec, version, image string) opensearchv1.DashboardsConfig {
	cfg := opensearchv1.DashboardsConfig{
		Enable:   true,
		Version:  version,
		Replicas: defaultDashboardsReplicas,
		Tls: &opensearchv1.DashboardsTlsConfig{
			Enable:   true,
			Generate: true,
		},
		Resources: nodeResources(dashboards.Resources),
	}
	if image != "" {
		cfg.ImageSpec = &opensearchv1.ImageSpec{Image: &image}
	}
	if dashboards.Replicas != nil {
		cfg.Replicas = *dashboards.Replicas
	}
	if svc := dashboards.Service; svc != nil {
		cfg.Annotations = svc.Annotations
		cfg.Service.Type = svc.ServiceType
		if svc.LoadBalancerService != nil {
			ranges := svc.LoadBalancerService.SourceRanges.NormalizedSourceRanges()
			if len(ranges) > 0 {
				cfg.Service.LoadBalancerSourceRanges = []string(ranges)
			}
		}
	}
	return cfg
}

// disabledDashboardsConfig turns Dashboards off by applying the running
// block with enable false. Dropping the block from the apply instead would
// remove the fields the provider owns, leaving dashboards.resources null,
// which the OpenSearchCluster CRD rejects.
func disabledDashboardsConfig(current opensearchv1.DashboardsConfig) opensearchv1.DashboardsConfig {
	disabled := *current.DeepCopy()
	disabled.Enable = false
	return disabled
}

// dashboardsObjectNames are the objects the operator creates for Dashboards.
// With dashboards.enable false it stops reconciling them but never removes
// them, so the provider does when the component is dropped.
func dashboardsObjectNames(clusterName string) (deployment, service, certSecret, configMap string) {
	base := clusterName + "-dashboards"
	return base, base, base + "-cert", base + "-config"
}

func deleteDashboards(c *controller.Context) error {
	deployment, service, certSecret, configMap := dashboardsObjectNames(c.Name())
	return errors.Join(
		c.Delete(&appsv1.Deployment{ObjectMeta: c.ObjectMeta(deployment)}),
		c.Delete(&corev1.Service{ObjectMeta: c.ObjectMeta(service)}),
		c.Delete(&corev1.Secret{ObjectMeta: c.ObjectMeta(certSecret)}),
		c.Delete(&corev1.ConfigMap{ObjectMeta: c.ObjectMeta(configMap)}),
	)
}

// nodeResources returns the node pool resources. The operator sizes the JVM
// heap from the memory request, so requests default to the limits when unset.
func nodeResources(in *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if in == nil {
		return corev1.ResourceRequirements{}
	}
	out := *in.DeepCopy()
	for name, limit := range out.Limits {
		if _, ok := out.Requests[name]; ok {
			continue
		}
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[name] = limit
	}
	return out
}

// defaultAffinity prefers spreading OpenSearch nodes across Kubernetes nodes.
func defaultAffinity(clusterName string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey: corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							clusterLabel:  clusterName,
							nodePoolLabel: nodePoolName,
						},
					},
				},
			}},
		},
	}
}
