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
		return controller.Progressing("OpenSearch cluster is bootstrapping")
	}

	var desired int32
	for _, pool := range cluster.Spec.NodePools {
		desired += pool.Replicas
	}

	var s controller.Status
	switch {
	case st.Phase == opensearchv1.PhaseUpgrading:
		s = controller.Progressing("Upgrading OpenSearch to " + cluster.Spec.General.Version)
	case st.AvailableNodes < desired:
		s = controller.Progressing(fmt.Sprintf("%d of %d nodes available", st.AvailableNodes, desired))
	case rollingRestartInProgress(st):
		// Between two node restarts every node can be up and the cluster
		// green, while the operator still has nodes to restart.
		s = controller.Progressing("Restarting nodes to apply configuration changes")
	case st.Health == opensearchv1.OpenSearchGreenHealth:
		s = controller.Ready()
	case st.Health == opensearchv1.OpenSearchYellowHealth:
		// Yellow means all primaries are allocated, so the cluster serves reads
		// and writes; a single-node cluster with index replicas never turns green.
		s = controller.Ready()
		s.Message = "Cluster health is yellow: some replica shards are unassigned"
	default:
		s = controller.Progressing(fmt.Sprintf("Cluster health is %q", healthOrUnknown(st.Health)))
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
