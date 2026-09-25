package provider

import (
	"fmt"
	"net"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// clusterStatus maps the operator's view of the cluster onto an Instance
// phase. details is only set once the cluster has been initialized.
func clusterStatus(cluster *opensearchv1.OpenSearchCluster, details controller.ConnectionDetails) controller.Status {
	st := cluster.Status
	if !st.Initialized {
		return controller.Provisioning("OpenSearch cluster is bootstrapping")
	}

	var desired int32
	for _, pool := range cluster.Spec.NodePools {
		desired += pool.Replicas
	}

	var s controller.Status
	switch {
	case st.Phase == opensearchv1.PhaseUpgrading:
		s = controller.Updating("Upgrading OpenSearch to " + cluster.Spec.General.Version)
	case st.AvailableNodes < desired:
		s = controller.Updating(fmt.Sprintf("%d of %d nodes available", st.AvailableNodes, desired))
	case st.Health == opensearchv1.OpenSearchGreenHealth:
		s = controller.Ready()
	case st.Health == opensearchv1.OpenSearchYellowHealth:
		// Yellow means all primaries are allocated, so the cluster serves reads
		// and writes; a single-node cluster with index replicas never turns green.
		s = controller.Ready()
		s.Message = "Cluster health is yellow: some replica shards are unassigned"
	default:
		s = controller.Updating(fmt.Sprintf("Cluster health is %q", healthOrUnknown(st.Health)))
	}
	s.ConnectionDetails = details
	return s
}

func healthOrUnknown(h opensearchv1.OpenSearchHealth) opensearchv1.OpenSearchHealth {
	if h == "" {
		return opensearchv1.OpenSearchUnknownHealth
	}
	return h
}

// connectionDetails builds the HTTPS endpoint of the cluster's client
// service together with the admin credentials.
func connectionDetails(cluster *opensearchv1.OpenSearchCluster, adminSecret *corev1.Secret) controller.ConnectionDetails {
	host := fmt.Sprintf("%s.%s.svc", cluster.Spec.General.ServiceName, cluster.Namespace)
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

	return controller.ConnectionDetails{
		Type:     "opensearch",
		Provider: common.ProviderName,
		Host:     host,
		Port:     portStr,
		Username: username,
		Password: password,
		URI:      uri.String(),
	}
}
