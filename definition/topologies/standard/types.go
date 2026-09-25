// Package standard contains parameter types for the standard topology.
//
// Add fields to StandardTopologyParameters and reference it via parametersSchema in
// topology.yaml when this topology needs parameters.
//
// +k8s:openapi-gen=true
package standard

// StandardTopologyParameters defines the parameters for the standard topology.
// Add fields here when the standard topology needs parameters
// beyond what the base Instance spec provides.
//
// Example:
//   type StandardTopologyParameters struct {
//       NumShards int32 `json:"numShards,omitempty"`
//   }
//
// Then reference it in topology.yaml:
//   config:
//     parametersSchema: StandardTopologyParameters
type StandardTopologyParameters struct{}
