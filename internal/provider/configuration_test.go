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
		c, _ := newTestContext(t, testInstance(validEngine()))

		got, err := resolveAdditionalConfig(c, validEngine())

		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("flattens nested keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster:\n  routing:\n    allocation:\n      disk:\n        threshold_enabled: false\nhttp:\n  port: 9201\n")
		c, _ := newTestContext(t, testInstance(engine))

		got, err := resolveAdditionalConfig(c, engine)

		require.NoError(t, err)
		assert.Equal(t, map[string]string{
			"cluster.routing.allocation.disk.threshold_enabled": "false",
			"http.port": "9201",
		}, got)
	})

	t.Run("accepts already-flat dotted keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster.routing.allocation.disk.threshold_enabled: false\n")
		c, _ := newTestContext(t, testInstance(engine))

		got, err := resolveAdditionalConfig(c, engine)

		require.NoError(t, err)
		assert.Equal(t, map[string]string{"cluster.routing.allocation.disk.threshold_enabled": "false"}, got)
	})

	t.Run("invalid YAML", func(t *testing.T) {
		engine := engineWithConfiguration(t, "cluster: [this is not: valid")
		c, _ := newTestContext(t, testInstance(engine))

		_, err := resolveAdditionalConfig(c, engine)

		assert.ErrorContains(t, err, "not valid YAML")
	})

	t.Run("list values are rejected", func(t *testing.T) {
		engine := engineWithConfiguration(t, "network.host:\n  - _local_\n  - _site_\n")
		c, _ := newTestContext(t, testInstance(engine))

		_, err := resolveAdditionalConfig(c, engine)

		assert.ErrorContains(t, err, "list values are not supported")
	})

	t.Run("blocks security plugin keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "plugins.security.ssl.http.enabled: false\n")
		c, _ := newTestContext(t, testInstance(engine))

		_, err := resolveAdditionalConfig(c, engine)

		assert.ErrorContains(t, err, `"plugins.security.ssl.http.enabled" is managed by the provider`)
	})

	t.Run("blocks nested discovery keys", func(t *testing.T) {
		engine := engineWithConfiguration(t, "discovery:\n  type: single-node\n")
		c, _ := newTestContext(t, testInstance(engine))

		_, err := resolveAdditionalConfig(c, engine)

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
