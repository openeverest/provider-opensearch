// Package components contains parameter types for provider component types.
//
// Each struct here corresponds to a component type defined in versions.yaml
// and is converted to an OpenAPI schema during generation.
// Add fields when a component type accepts parameters beyond
// what the base Instance spec provides.
//
// +k8s:openapi-gen=true
package components

// OpensearchParameters defines the parameters for opensearch components.
// Add fields here when the opensearch component type needs parameters
// beyond what the base Instance spec provides.
type OpensearchParameters struct{}
