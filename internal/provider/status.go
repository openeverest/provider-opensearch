package provider

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// Reasons of the problems the provider reports, and why clients cannot
// connect. A reason becomes the status word users read.
const (
	// reasonPrimaryShardsUnassigned: the cluster health is red, so some
	// indices cannot be read or written.
	reasonPrimaryShardsUnassigned = "PrimaryShardsUnassigned"
	// reasonHealthUnknown: the operator has not reported the cluster health.
	reasonHealthUnknown = "HealthUnknown"
	// reasonRestoringSnapshot: a Restore closes the indices it overwrites.
	reasonRestoringSnapshot = "RestoringSnapshot"
)

// opensearchContainer is the container the operator runs OpenSearch in.
const opensearchContainer = "opensearch"

// Reasons of the operations the provider reports.
const (
	reasonUpgradingVersion = "UpgradingVersion"
	reasonResizingVolumes  = "ResizingVolumes"
)

// clusterWant is what the Instance asks of the cluster: its OpenSearch
// version and the number of OpenSearch nodes across the node pools. It comes
// from the Instance rather than from the OpenSearchCluster, which the cache
// may still hold from before Sync applied the latest spec.
type clusterWant struct {
	version string
	nodes   int32
}

// clusterStatus reports what the operator's view of the cluster says: whether
// clients can connect, what is wrong, the upgrade in progress, and what is
// still waited for. details is only set once the cluster has been
// initialized. The caller decides whether the Instance has converged.
func clusterStatus(cluster *opensearchv1.OpenSearchCluster, want clusterWant, nodes nodeRollout, details controller.ConnectionDetails) controller.Status {
	st := cluster.Status
	if !st.Initialized {
		// Availability is left unreported: the runtime reads an Instance
		// that has never been available as unavailable.
		return controller.Progressing("OpenSearch cluster is bootstrapping")
	}

	var s controller.Status
	switch st.Health {
	case opensearchv1.OpenSearchGreenHealth, opensearchv1.OpenSearchYellowHealth:
		// Yellow means all primaries are allocated, so the cluster serves
		// reads and writes; a single-node cluster with index replicas never
		// turns green.
		s = s.WithAvailable(controller.Available)
	case opensearchv1.OpenSearchRedHealth:
		const message = "Cluster health is red: some primary shards are unassigned, so their indices cannot be read or written"
		s = s.WithUnavailable(reasonPrimaryShardsUnassigned, message).
			WithProblem(reasonPrimaryShardsUnassigned, message)
	default:
		s = s.WithAvailable(controller.AvailabilityUnknown)
		s.AvailableReason = reasonHealthUnknown
		s.AvailableMessage = fmt.Sprintf("Cluster health is %q", healthOrUnknown(st.Health))
	}
	s.ConnectionDetails = details

	switch {
	case st.Phase == opensearchv1.PhaseUpgrading || (st.Version != "" && want.version != "" && st.Version != want.version):
		message := "Upgrading OpenSearch to " + want.version
		if st.Version != "" && st.Version != want.version {
			message = fmt.Sprintf("Upgrading OpenSearch from %s to %s", st.Version, want.version)
		}
		s = s.WithOperation(corev1alpha1.ConditionUpgrading, reasonUpgradingVersion, message)
		s.Message = message
	case nodes.staleImage > 0:
		// The provider pins the image, so the operator rolls a version change
		// out as a restart of the nodes, and reports the new version at once.
		message := fmt.Sprintf("Upgrading OpenSearch to %s: %d of %d nodes still run another image",
			want.version, nodes.staleImage, want.nodes)
		s = s.WithOperation(corev1alpha1.ConditionUpgrading, reasonUpgradingVersion, message)
		s.Message = message
	case st.AvailableNodes != want.nodes:
		s.Message = fmt.Sprintf("%d of %d nodes available", st.AvailableNodes, want.nodes)
	case nodes.restarting > 0 || rollingRestartInProgress(st):
		// Between two node restarts every node can be up and the cluster
		// green, while the operator still has nodes to restart.
		s.Message = "Restarting nodes to apply configuration changes"
	case st.Health != opensearchv1.OpenSearchGreenHealth && st.Health != opensearchv1.OpenSearchYellowHealth:
		s.Message = fmt.Sprintf("Cluster health is %q", healthOrUnknown(st.Health))
	}
	return s
}

// nodeRollout is how far the operator has rolled the OpenSearch pods out to
// the cluster's spec.
type nodeRollout struct {
	// staleImage counts the nodes running another image than the cluster's.
	staleImage int32
	// restarting counts the nodes a node pool's StatefulSet has not updated
	// to its latest revision yet.
	restarting int32
}

// observeNodes compares the OpenSearch pods and the node pools'
// StatefulSets with the cluster's spec. The image is the one the cluster
// carries, not the Instance's: a provider-initiated change held for
// maintenance is not rolling out.
func observeNodes(c *controller.Context, cluster *opensearchv1.OpenSearchCluster) (nodeRollout, error) {
	var out nodeRollout
	if image := cluster.Spec.General.ImageSpec; image != nil && image.Image != nil && *image.Image != "" {
		pods := &corev1.PodList{}
		if err := c.List(pods, client.MatchingLabels{clusterLabel: cluster.Name}, client.HasLabels{nodePoolLabel}); err != nil {
			return nodeRollout{}, fmt.Errorf("list OpenSearch pods: %w", err)
		}
		for _, pod := range pods.Items {
			for _, container := range pod.Spec.Containers {
				if container.Name == opensearchContainer && container.Image != *image.Image {
					out.staleImage++
				}
			}
		}
	}
	for _, pool := range cluster.Spec.NodePools {
		sts := &appsv1.StatefulSet{}
		if err := c.Get(sts, cluster.Name+"-"+pool.Component); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nodeRollout{}, fmt.Errorf("get StatefulSet of node pool %q: %w", pool.Component, err)
		}
		replicas := ptr.Deref(sts.Spec.Replicas, 1)
		switch {
		case sts.Status.ObservedGeneration < sts.Generation:
			out.restarting += replicas
		case sts.Status.UpdatedReplicas < replicas:
			out.restarting += replicas - sts.Status.UpdatedReplicas
		}
	}
	return out, nil
}

// wantedCluster returns what the Instance asks of the cluster.
func wantedCluster(c *controller.Context) (clusterWant, error) {
	spec, err := c.ProviderSpec()
	if err != nil {
		return clusterWant{}, err
	}
	in := c.Instance()
	want := clusterWant{
		version: resolveVersion(spec, in.Spec.Components[common.ComponentEngine], common.ComponentTypeOpensearch),
	}
	for _, def := range nodePoolDefs(topologyOf(in)) {
		if component, ok := in.Spec.Components[def.component]; ok {
			want.nodes += ptr.Deref(component.Replicas, def.defaultReplicas)
		}
	}
	return want, nil
}

// withDesiredReplicas reports how many pods each component will have once
// the current spec has converged.
func withDesiredReplicas(in *corev1alpha1.Instance, s controller.Status) controller.Status {
	for _, def := range nodePoolDefs(topologyOf(in)) {
		if component, ok := in.Spec.Components[def.component]; ok {
			s = s.WithDesiredReplicas(def.component, ptr.Deref(component.Replicas, def.defaultReplicas))
		}
	}
	if dashboards, ok := in.Spec.Components[common.ComponentDashboards]; ok {
		s = s.WithDesiredReplicas(common.ComponentDashboards, ptr.Deref(dashboards.Replicas, defaultDashboardsReplicas))
	}
	return s
}

// resizingVolumes says which data volumes have not grown yet to the size
// the Instance asks for, or "" when none. The operator grows a node pool's
// volumes by patching the PersistentVolumeClaims of its StatefulSet,
// data-<cluster>-<pool>-<ordinal>, and the storage driver then expands them.
func resizingVolumes(c *controller.Context) (string, error) {
	in := c.Instance()
	var resizing []string
	for _, def := range nodePoolDefs(topologyOf(in)) {
		component, ok := in.Spec.Components[def.component]
		if !ok || def.ephemeral || component.Storage == nil {
			continue
		}
		replicas := ptr.Deref(component.Replicas, def.defaultReplicas)
		for i := range replicas {
			name := fmt.Sprintf("data-%s-%s-%d", c.Name(), def.pool, i)
			pvc := &corev1.PersistentVolumeClaim{}
			if err := c.Get(pvc, name); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return "", fmt.Errorf("get PersistentVolumeClaim %q: %w", name, err)
			}
			capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]
			if ok && capacity.Cmp(component.Storage.Size) < 0 {
				resizing = append(resizing, name)
			}
		}
	}
	if len(resizing) == 0 {
		return "", nil
	}
	return "Resizing volumes " + strings.Join(resizing, ", "), nil
}

func healthOrUnknown(h opensearchv1.OpenSearchHealth) opensearchv1.OpenSearchHealth {
	if h == "" {
		return opensearchv1.OpenSearchUnknownHealth
	}
	return h
}

// rollingRestartComponent is the componentsStatus entry the operator keeps
// while it restarts nodes one by one.
const rollingRestartComponent = "RollingRestart"

func rollingRestartInProgress(st opensearchv1.ClusterStatus) bool {
	for _, cs := range st.ComponentsStatus {
		if cs.Component == rollingRestartComponent && cs.Status == "InProgress" {
			return true
		}
	}
	return false
}

// withHost points the connection details at host, keeping port and
// credentials.
func withHost(details controller.ConnectionDetails, host string) controller.ConnectionDetails {
	details.Host = host
	if u, err := url.Parse(details.URI); err == nil {
		u.Host = net.JoinHostPort(host, details.Port)
		details.URI = u.String()
	}
	return details
}

// connectionDetails builds the HTTPS endpoint of the cluster's client
// service together with the admin credentials and the CA that verifies the
// endpoint. A dedicated cluster's client service is the extra name in its
// HTTP certificate.
func connectionDetails(cluster *opensearchv1.OpenSearchCluster, adminSecret *corev1.Secret, ca []byte) controller.ConnectionDetails {
	host := fmt.Sprintf("%s.%s.svc", cluster.Spec.General.ServiceName, cluster.Namespace)
	if sec := cluster.Spec.Security; sec != nil && sec.Tls != nil && sec.Tls.Http != nil &&
		sec.Tls.Http.CustomFQDN != nil && *sec.Tls.Http.CustomFQDN != "" {
		host = *sec.Tls.Http.CustomFQDN
	}
	port := cluster.Spec.General.HttpPort
	if port == 0 {
		port = httpPort
	}
	portStr := strconv.Itoa(int(port))
	username := string(adminSecret.Data["username"])
	password := string(adminSecret.Data["password"])

	uri := url.URL{
		Scheme: "https",
		User:   url.UserPassword(username, password),
		Host:   net.JoinHostPort(host, portStr),
	}

	details := controller.ConnectionDetails{
		Type:     "opensearch",
		Provider: common.ProviderName,
		Host:     host,
		Port:     portStr,
		Username: username,
		Password: password,
		URI:      uri.String(),
	}
	if len(ca) > 0 {
		details.AdditionalProperties = map[string]string{caKey: string(ca)}
	}
	return details
}
