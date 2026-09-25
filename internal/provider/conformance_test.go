package provider

import (
	"testing"

	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

func TestUISchemaIsReconciled(t *testing.T) {
	conformance.UISchemaIsReconciled(t, conformance.Config{Provider: New()})
}

func TestSupportedFieldsAreReconciled(t *testing.T) {
	conformance.SupportedFieldsAreReconciled(t, conformance.Config{Provider: New()})
}
