package provider

import (
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/definition/components"
	"github.com/openeverest/provider-opensearch/internal/common"
)

// ownedConfigPrefixes are opensearch.yml dotted-key prefixes the provider and
// operator own: TLS and the security plugin, and cluster discovery. A user
// Configuration fragment cannot set keys under these.
var ownedConfigPrefixes = []string{
	"plugins.security.",
	"discovery.",
}

// resolveAdditionalConfig parses the engine's Configuration parameter (an
// opensearch.yml fragment) into the flat dotted-key map the operator's
// nodePools[].additionalConfig expects. Returns nil if no configuration is
// set.
func resolveAdditionalConfig(c *controller.Context, engine corev1alpha1.ComponentSpec) (map[string]string, error) {
	var params components.OpensearchParameters
	if !c.TryDecodeComponentParameters(engine, &params) || params.Configuration == "" {
		return nil, nil
	}

	var tree map[string]any
	if err := yaml.Unmarshal([]byte(params.Configuration), &tree); err != nil {
		return nil, fmt.Errorf("%s: configuration is not valid YAML: %w", common.ComponentEngine, err)
	}

	flat := map[string]string{}
	if err := flattenConfig("", tree, flat); err != nil {
		return nil, fmt.Errorf("%s: configuration: %w", common.ComponentEngine, err)
	}

	for key := range flat {
		for _, prefix := range ownedConfigPrefixes {
			if key == strings.TrimSuffix(prefix, ".") || strings.HasPrefix(key, prefix) {
				return nil, fmt.Errorf("%s: configuration: %q is managed by the provider and cannot be set", common.ComponentEngine, key)
			}
		}
	}

	return flat, nil
}

// flattenConfig turns a parsed YAML mapping into opensearch.yml's flat
// dotted-key form, e.g. {cluster: {routing: {allocation: {...}}}} becomes
// "cluster.routing.allocation...". List values are not supported: opensearch.yml
// has no single flat-key representation for them that this provider can
// produce reliably.
func flattenConfig(prefix string, in map[string]any, out map[string]string) error {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch v := in[k].(type) {
		case map[string]any:
			if err := flattenConfig(key, v, out); err != nil {
				return err
			}
		case []any:
			return fmt.Errorf("%q: list values are not supported", key)
		case nil:
			out[key] = ""
		default:
			out[key] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}
