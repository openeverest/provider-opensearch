package provider

import (
	"errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

const (
	// Node pool names. The operator uses them in StatefulSet, Service and PVC
	// names, so they must never change for existing clusters.
	//
	// nodePoolName is the single pool of the standard topology.
	nodePoolName        = "nodes"
	managerPoolName     = "managers"
	dataPoolName        = "data"
	coordinatorPoolName = "coordinators"

	httpPort int32 = 9200

	defaultReplicas            int32 = 3
	defaultCoordinatorReplicas int32 = 2

	// defaultDashboardsReplicas matches the operator's own CRD default
	// (kubebuilder:default=1 on DashboardsConfig.Replicas); set explicitly
	// here so buildDashboardsConfig's output does not depend on it.
	defaultDashboardsReplicas int32 = 1

	// Pod labels set by the operator on every OpenSearch node.
	clusterLabel  = "opensearch.org/opensearch-cluster"
	nodePoolLabel = "opensearch.org/opensearch-nodepool"
)

// nodePoolDef ties an Instance component to the OpenSearchCluster node pool
// it renders.
type nodePoolDef struct {
	component       string
	pool            string
	roles           []string
	defaultReplicas int32
	// ephemeral pools hold no data and get an emptyDir instead of a PVC.
	ephemeral bool
}

var (
	// In the standard topology every node is cluster-manager eligible, holds
	// data and runs ingest pipelines.
	standardPools = []nodePoolDef{
		{component: common.ComponentEngine, pool: nodePoolName, roles: []string{"cluster_manager", "data", "ingest"}, defaultReplicas: defaultReplicas},
	}
	// The dedicated topology keeps the cluster-manager quorum off the data
	// nodes. Coordinating-only nodes have no roles (the CRD requires the
	// field, so the list is empty rather than nil).
	dedicatedPools = []nodePoolDef{
		{component: common.ComponentClusterManager, pool: managerPoolName, roles: []string{"cluster_manager"}, defaultReplicas: defaultReplicas},
		{component: common.ComponentEngine, pool: dataPoolName, roles: []string{"data", "ingest"}, defaultReplicas: defaultReplicas},
		{component: common.ComponentCoordinator, pool: coordinatorPoolName, roles: []string{}, defaultReplicas: defaultCoordinatorReplicas, ephemeral: true},
	}
)

// topologyOf returns the Instance's topology; an unset type is standard.
func topologyOf(in *corev1alpha1.Instance) string {
	if t := in.GetTopologyType(); t != "" {
		return t
	}
	return common.TopologyStandard
}

// nodePoolDefs returns the node pools of a topology, including the ones of
// optional components the Instance may leave out.
func nodePoolDefs(topology string) []nodePoolDef {
	if topology == common.TopologyDedicated {
		return dedicatedPools
	}
	return standardPools
}

// clientServiceName is the provider-managed ClusterIP Service clients of a
// dedicated Instance connect to: it selects the coordinators when there are
// any, else the data nodes, so cluster managers do not serve client traffic.
func clientServiceName(clusterName string) string {
	return clusterName + "-client"
}

// clientServiceHost is the client Service's DNS name. The operator only puts
// its own service names in the HTTP certificate and never reissues it when
// they change, so a dedicated cluster adds this name from creation on.
func clientServiceHost(clusterName, namespace string) string {
	return clientServiceName(clusterName) + "." + namespace + ".svc"
}

// clientPodSelector selects the pods that serve client requests.
func clientPodSelector(clusterName, topology string, components map[string]corev1alpha1.ComponentSpec) map[string]string {
	selector := map[string]string{clusterLabel: clusterName}
	if topology != common.TopologyDedicated {
		return selector
	}
	selector[nodePoolLabel] = dataPoolName
	if _, ok := components[common.ComponentCoordinator]; ok {
		selector[nodePoolLabel] = coordinatorPoolName
	}
	return selector
}

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

// clusterInputs is everything an OpenSearchCluster spec is rendered from.
type clusterInputs struct {
	name       string
	namespace  string
	topology   string
	components map[string]corev1alpha1.ComponentSpec
	// configs holds each OpenSearch component's parsed opensearch.yml
	// fragment, keyed by component name.
	configs        map[string]map[string]string
	version, image string
	snapshotRepos  []opensearchv1.SnapshotRepoConfig
	keystore       []opensearchv1.KeystoreValue
	backupPlugins  []string
}

// buildClusterSpec maps the OpenSearch components of an Instance onto an
// OpenSearchCluster spec, one node pool per component.
func buildClusterSpec(in clusterInputs) opensearchv1.ClusterSpec {
	general := opensearchv1.GeneralConfig{
		Version:              in.version,
		ServiceName:          in.name,
		HttpPort:             httpPort,
		SnapshotRepositories: in.snapshotRepos,
		Keystore:             in.keystore,
		PluginsList:          in.backupPlugins,
		// The PVCs hold the only copy of the data; drop them with the cluster
		// but keep them on scale-down so scaling back up reuses them.
		PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
	}
	if in.image != "" {
		image := in.image
		general.ImageSpec = &opensearchv1.ImageSpec{Image: &image}
	}
	if svc := in.components[common.ComponentEngine].Service; svc != nil {
		general.Annotations = svc.Annotations
	}

	httpTLS := &opensearchv1.TlsConfigHttp{Generate: true}
	if in.topology == common.TopologyDedicated {
		host := clientServiceHost(in.name, in.namespace)
		httpTLS.CustomFQDN = &host
	}

	var pools []opensearchv1.NodePool
	for _, def := range nodePoolDefs(in.topology) {
		component, ok := in.components[def.component]
		if !ok && def.component != common.ComponentEngine {
			continue
		}
		pools = append(pools, buildNodePool(in.name, def, component, in.configs[def.component]))
	}

	return opensearchv1.ClusterSpec{
		General: general,
		// Sent explicitly: the CRD default does not apply to a serialized false.
		ConfMgmt: opensearchv1.ConfMgmt{SmartScaler: true},
		Security: &opensearchv1.Security{
			Tls: &opensearchv1.TlsConfig{
				Transport: &opensearchv1.TlsConfigTransport{Generate: true, PerNode: true},
				Http:      httpTLS,
			},
		},
		NodePools: pools,
	}
}

func buildNodePool(clusterName string, def nodePoolDef, component corev1alpha1.ComponentSpec, additionalConfig map[string]string) opensearchv1.NodePool {
	pool := opensearchv1.NodePool{
		Component:        def.pool,
		AdditionalConfig: additionalConfig,
		Replicas:         def.defaultReplicas,
		Roles:            def.roles,
		Resources:        nodeResources(component.Resources),
		Pdb:              nodePoolPDB(),
	}
	if component.Replicas != nil {
		pool.Replicas = *component.Replicas
	}
	if def.ephemeral {
		pool.Persistence = &opensearchv1.PersistenceConfig{
			PersistenceSource: opensearchv1.PersistenceSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}
	} else {
		pool.Persistence = &opensearchv1.PersistenceConfig{
			PersistenceSource: opensearchv1.PersistenceSource{
				PVC: &opensearchv1.PVCSource{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			},
		}
		if component.Storage != nil {
			pool.DiskSize = component.Storage.Size
			pool.Persistence.PVC.StorageClassName = component.Storage.StorageClass
		}
	}

	var policy commonv1alpha1.SchedulingPolicy
	if component.SchedulingPolicy != nil {
		policy = *component.SchedulingPolicy
	}
	pool.NodeSelector = policy.NodeSelector
	pool.Tolerations = policy.Tolerations
	if policy.TopologySpreadConstraints != nil {
		pool.TopologySpreadConstraints = *policy.TopologySpreadConstraints
	}
	pool.Affinity = policy.Affinity
	if pool.Affinity == nil {
		pool.Affinity = defaultAffinity(clusterName, def.pool)
	}

	return pool
}

// nodePoolPDB lets voluntary disruptions (node drains, cluster autoscaler)
// evict at most one OpenSearch node at a time, so a 3-node cluster keeps its
// cluster-manager quorum. maxUnavailable rather than minAvailable keeps a
// 1-node Instance drainable. The operator's own restarts and upgrades delete
// pods directly and are not limited by it.
func nodePoolPDB() *opensearchv1.PdbConfig {
	maxUnavailable := intstr.FromInt32(1)
	return &opensearchv1.PdbConfig{Enable: true, MaxUnavailable: &maxUnavailable}
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

// defaultAffinity prefers spreading the nodes of a pool across Kubernetes
// nodes.
func defaultAffinity(clusterName, poolName string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey: corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							clusterLabel:  clusterName,
							nodePoolLabel: poolName,
						},
					},
				},
			}},
		},
	}
}
