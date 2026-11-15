// Package dedicated contains parameter types for the dedicated topology.
//
// Add fields to DedicatedTopologyParameters and reference it via parametersSchema in
// topology.yaml when this topology needs parameters.
//
// +k8s:openapi-gen=true
package dedicated

// DedicatedTopologyParameters defines the parameters for the dedicated topology.
// Add fields here when the dedicated topology needs parameters
// beyond what the base Instance spec provides.
//
// Example:
//   type DedicatedTopologyParameters struct {
//       NumShards int32 `json:"numShards,omitempty"`
//   }
//
// Then reference it in topology.yaml:
//   config:
//     parametersSchema: DedicatedTopologyParameters
type DedicatedTopologyParameters struct{}
