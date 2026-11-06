package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

func engineWithConfiguration(t *testing.T, configuration string) corev1alpha1.ComponentSpec {
	t.Helper()
	engine := validEngine()
	raw, err := json.Marshal(map[string]string{"configuration": configuration})
	require.NoError(t, err)
	engine.Parameters = &runtime.RawExtension{Raw: raw}
	return engine
}

func TestResolveAdditionalConfig(t *testing.T) {
	t.Run("no parameters set", func(t *testing.T) {
		got, err := resolveAdditionalConfig(validEngine())

		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("flattens nested keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster:\n  routing:\n    allocation:\n      disk:\n        threshold_enabled: false\nindices:\n  query:\n    bool:\n      max_clause_count: 10000000\n")
		got, err := resolveAdditionalConfig(engine)

		require.NoError(t, err)
		assert.Equal(t, map[string]string{
			"cluster.routing.allocation.disk.threshold_enabled": "false",
			// Large integers must not turn into 1e+07.
			"indices.query.bool.max_clause_count": "10000000",
		}, got)
	})

	t.Run("accepts already-flat dotted keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster.routing.allocation.disk.threshold_enabled: false\n")
		got, err := resolveAdditionalConfig(engine)

		require.NoError(t, err)
		assert.Equal(t, map[string]string{"cluster.routing.allocation.disk.threshold_enabled": "false"}, got)
	})

	t.Run("invalid YAML", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster: [this is not: valid")
		_, err := resolveAdditionalConfig(engine)

		assert.ErrorContains(t, err, "not valid YAML")
	})

	t.Run("list values are rejected", func(t *testing.T) {
		engine := engineWithConfiguration(t, "network.host:\n  - _local_\n  - _site_\n")
		_, err := resolveAdditionalConfig(engine)

		assert.ErrorContains(t, err, "list values are not supported")
	})

	t.Run("blocks security plugin keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "plugins.security.ssl.http.enabled: false\n")
		_, err := resolveAdditionalConfig(engine)

		assert.ErrorContains(t, err, `"plugins.security.ssl.http.enabled" is managed by the provider`)
	})

	for _, key := range []string{"cluster.name", "node.roles", "network.host", "http.port", "transport.port", "path.data"} {
		t.Run("blocks "+key, func(t *testing.T) {
			engine := engineWithConfiguration(t, key+": x\n")

			_, err := resolveAdditionalConfig(engine)

			assert.ErrorContains(t, err, `"`+key+`" is managed by the provider`)
		})
	}

	t.Run("allows keys that only share a prefix with an owned key", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster.routing.allocation.enable: all\nnodes_extra: x\n")

		got, err := resolveAdditionalConfig(engine)

		require.NoError(t, err)
		assert.Len(t, got, 2)
	})

	t.Run("blocks nested discovery keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "discovery:\n  type: single-node\n")
		_, err := resolveAdditionalConfig(engine)

		assert.ErrorContains(t, err, `"discovery.type" is managed by the provider`)
	})
}

func TestFlattenConfig(t *testing.T) {
	tests := []struct {
		name    string
		in      map[string]any
		want    map[string]string
		wantErr string
	}{
		{
			name: "scalars",
			in:   map[string]any{"a": "x", "b": true, "c": 5, "d": 1.5},
			want: map[string]string{"a": "x", "b": "true", "c": "5", "d": "1.5"},
		},
		{
			name: "nil value becomes empty string",
			in:   map[string]any{"a": nil},
			want: map[string]string{"a": ""},
		},
		{
			name: "nested maps flatten with dots",
			in:   map[string]any{"a": map[string]any{"b": map[string]any{"c": "x"}}},
			want: map[string]string{"a.b.c": "x"},
		},
		{
			name:    "list values error",
			in:      map[string]any{"a": []any{"x", "y"}},
			wantErr: `"a": list values are not supported`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := map[string]string{}
			err := flattenConfig("", tt.in, out)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}
