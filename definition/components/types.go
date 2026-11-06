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
type OpensearchParameters struct {
	// Configuration is an opensearch.yml fragment (YAML), merged into the
	// node pool's additionalConfig. `configuration` is the conventional
	// property name for the engine configuration file inside a component's
	// parameters. Keys under the TLS, security, and discovery settings the
	// provider and operator manage are rejected.
	Configuration string `json:"configuration,omitempty"`
}
