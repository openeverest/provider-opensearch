package provider

import (
	"testing"

	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

func TestUISchemaIsReconciled(t *testing.T) {
	conformance.UISchemaIsReconciled(t, conformance.Config{
		Provider: New(),
		Unverifiable: map[string]string{
			// The field holds an opensearch.yml YAML fragment that Sync parses
			// and flattens into additionalConfig; the harness's generic string
			// probe is not valid YAML-mapping syntax, so Sync correctly rejects
			// it. Covered directly by TestResolveAdditionalConfig, TestFlattenConfig
			// and TestBuildNodePoolAdditionalConfig.
			"spec.components.engine.parameters.configuration":         "probe value is not valid YAML; see configuration_test.go",
			"spec.components.clusterManager.parameters.configuration": "probe value is not valid YAML; see configuration_test.go",
			// The kind only matters with an issuer name, which the harness
			// probes separately. Covered by TestValidateReferencesSecurity.
			"spec.parameters.tls.issuer.kind": "only applies together with spec.parameters.tls.issuer.name; see tls_test.go",
		},
	})
}

func TestSupportedFieldsAreReconciled(t *testing.T) {
	conformance.SupportedFieldsAreReconciled(t, conformance.Config{Provider: New()})
}
