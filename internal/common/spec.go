// Package common defines shared constants used across the provider.
package common

const (
	// ProviderName is this provider's identity: the name of the Provider CR it
	// ships and the value Instances put in spec.providerRef.name. It must match
	// `name` in definition/provider.yaml — the runtime uses it to fetch the
	// Provider CR, so a mismatch means nothing ever reconciles.
	ProviderName = "opensearch"

	// ComponentEngine is the OpenSearch node component.
	ComponentEngine = "engine"

	// ComponentClusterManager is the cluster-manager node component of the
	// dedicated topology.
	ComponentClusterManager = "clusterManager"

	// ComponentCoordinator is the optional coordinating-only node component
	// of the dedicated topology.
	ComponentCoordinator = "coordinator"

	// ComponentDashboards is the optional OpenSearch Dashboards component.
	ComponentDashboards = "dashboards"

	ComponentTypeOpensearch = "opensearch"
	ComponentTypeDashboards = "dashboards"

	// TopologyStandard is the default topology: a single node pool of
	// cluster-manager eligible data nodes.
	TopologyStandard = "standard"

	// TopologyDedicated separates the cluster-manager quorum from the data
	// nodes, with optional coordinating-only nodes in front.
	TopologyDedicated = "dedicated"
)
