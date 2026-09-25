package provider

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(in *corev1alpha1.Instance)
		wantErr string
	}{
		{name: "valid", mutate: func(*corev1alpha1.Instance) {}},
		{
			name:    "name too long",
			mutate:  func(in *corev1alpha1.Instance) { in.Name = strings.Repeat("a", maxInstanceNameLength+1) },
			wantErr: "at most",
		},
		{
			name:    "name starts with a digit",
			mutate:  func(in *corev1alpha1.Instance) { in.Name = "1search" },
			wantErr: "invalid",
		},
		{
			name:    "unknown topology",
			mutate:  func(in *corev1alpha1.Instance) { in.Spec.Topology = &corev1alpha1.TopologySpec{Type: "sharded"} },
			wantErr: "unsupported topology",
		},
		{
			name: "standard topology",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Topology = &corev1alpha1.TopologySpec{Type: "standard"}
			},
		},
		{
			name: "unknown component",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Components["dashboards"] = corev1alpha1.ComponentSpec{}
			},
			wantErr: "unsupported component",
		},
		{
			name:    "missing engine",
			mutate:  func(in *corev1alpha1.Instance) { delete(in.Spec.Components, "engine") },
			wantErr: "is required",
		},
		{
			name: "zero replicas",
			mutate: func(in *corev1alpha1.Instance) {
				e := in.Spec.Components["engine"]
				e.Replicas = ptr.To[int32](0)
				in.Spec.Components["engine"] = e
			},
			wantErr: "replicas",
		},
		{
			name: "missing storage",
			mutate: func(in *corev1alpha1.Instance) {
				e := in.Spec.Components["engine"]
				e.Storage = nil
				in.Spec.Components["engine"] = e
			},
			wantErr: "storage size is required",
		},
		{
			name: "storage too small",
			mutate: func(in *corev1alpha1.Instance) {
				e := in.Spec.Components["engine"]
				e.Storage.Size = resource.MustParse("512Mi")
				in.Spec.Components["engine"] = e
			},
			wantErr: "at least 1Gi",
		},
		{
			name: "memory too small",
			mutate: func(in *corev1alpha1.Instance) {
				e := in.Spec.Components["engine"]
				e.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
				in.Spec.Components["engine"] = e
			},
			wantErr: "memory",
		},
		{
			name: "cpu too small",
			mutate: func(in *corev1alpha1.Instance) {
				e := in.Spec.Components["engine"]
				e.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("100m")
				in.Spec.Components["engine"] = e
			},
			wantErr: "cpu",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance(validEngine())
			tt.mutate(in)
			c, _ := newTestContext(t, in)

			err := New().Validate(c)

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestValidateStorageShrink(t *testing.T) {
	existing := &opensearchv1.OpenSearchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
		Spec: opensearchv1.ClusterSpec{NodePools: []opensearchv1.NodePool{
			{Component: nodePoolName, DiskSize: resource.MustParse("20Gi")},
		}},
	}
	c, _ := newTestContext(t, testInstance(validEngine()), existing)

	assert.ErrorContains(t, New().Validate(c), "cannot be decreased")
}
