package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-opensearch/definition/components"
)

// ownedConfigKeys are opensearch.yml keys the provider and operator own:
// TLS and the security plugin, cluster formation, node identity and roles,
// listen addresses and ports, and data paths. A user Configuration fragment
// cannot set them, or any key below them.
var ownedConfigKeys = []string{
	"plugins.security",
	"discovery",
	"cluster.initial_cluster_manager_nodes",
	"cluster.initial_master_nodes",
	"cluster.name",
	"node",
	"network",
	"http.port",
	"http.publish_port",
	"transport.port",
	"transport.publish_port",
	"path",
}

// engineConfiguration returns the engine's Configuration parameter, an
// opensearch.yml fragment, or "" when it is not set.
func engineConfiguration(engine corev1alpha1.ComponentSpec) (string, error) {
	if engine.Parameters == nil || len(engine.Parameters.Raw) == 0 {
		return "", nil
	}
	var params components.OpensearchParameters
	if err := json.Unmarshal(engine.Parameters.Raw, &params); err != nil {
		return "", fmt.Errorf("decode parameters: %w", err)
	}
	return params.Configuration, nil
}

// resolveAdditionalConfig parses the engine's Configuration parameter into
// the flat dotted-key map the operator's nodePools[].additionalConfig expects.
// Returns nil if no configuration is set.
func resolveAdditionalConfig(engine corev1alpha1.ComponentSpec) (map[string]string, error) {
	configuration, err := engineConfiguration(engine)
	if err != nil || configuration == "" {
		return nil, err
	}

	var tree map[string]any
	if err := yaml.Unmarshal([]byte(configuration), &tree); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}

	flat := map[string]string{}
	if err := flattenConfig("", tree, flat); err != nil {
		return nil, err
	}

	for key := range flat {
		for _, owned := range ownedConfigKeys {
			if key == owned || strings.HasPrefix(key, owned+".") {
				return nil, fmt.Errorf("%q is managed by the provider and cannot be set", key)
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
		case float64:
			// YAML numbers decode as float64; 'f' keeps large integers out of
			// exponent form, which OpenSearch would not parse as an integer.
			out[key] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			out[key] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}
