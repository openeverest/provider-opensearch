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

func existingCluster(runningVersion string, storageClass *string) *opensearchv1.OpenSearchCluster {
	return &opensearchv1.OpenSearchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
		Spec: opensearchv1.ClusterSpec{NodePools: []opensearchv1.NodePool{{
			Component: nodePoolName,
			DiskSize:  resource.MustParse("10Gi"),
			Persistence: &opensearchv1.PersistenceConfig{PersistenceSource: opensearchv1.PersistenceSource{
				PVC: &opensearchv1.PVCSource{StorageClassName: storageClass},
			}},
		}}},
		Status: opensearchv1.ClusterStatus{Version: runningVersion},
	}
}

func TestValidateInvalidVersion(t *testing.T) {
	engine := validEngine()
	engine.Version = "not-a-version"
	c, _ := newTestContext(t, testInstance(engine))

	assert.ErrorContains(t, New().Validate(c), "not a valid semantic version")
}

func TestValidateVersionChange(t *testing.T) {
	tests := []struct {
		name      string
		running   string
		requested string
		wantErr   string
	}{
		{name: "same version", running: "3.8.0", requested: "3.8.0"},
		{name: "minor upgrade", running: "3.7.0", requested: "3.8.0"},
		{name: "patch upgrade", running: "3.8.0", requested: "3.8.1"},
		{name: "next major", running: "3.8.0", requested: "4.0.0"},
		{name: "minor downgrade", running: "3.8.0", requested: "3.7.0", wantErr: "cannot be downgraded"},
		{name: "patch downgrade", running: "3.8.1", requested: "3.8.0", wantErr: "cannot be downgraded"},
		{name: "major downgrade", running: "3.8.0", requested: "2.19.2", wantErr: "cannot be downgraded"},
		{name: "major jump", running: "2.19.2", requested: "4.0.0", wantErr: "skips a major version"},
		{name: "cluster not started yet", running: "", requested: "3.7.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := validEngine()
			engine.Version = tt.requested
			c, _ := newTestContext(t, testInstance(engine), existingCluster(tt.running, nil))

			err := New().Validate(c)

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}

	t.Run("default version is compared when none is set", func(t *testing.T) {
		// The provider default is 3.8.0, so a running 3.9.0 makes it a downgrade.
		c, _ := newTestContext(t, testInstance(validEngine()), existingCluster("3.9.0", nil))

		assert.ErrorContains(t, New().Validate(c), "cannot be downgraded")
	})

	t.Run("error tells the user what is allowed", func(t *testing.T) {
		engine := validEngine()
		engine.Version = "3.7.0"
		c, _ := newTestContext(t, testInstance(engine), existingCluster("3.8.0", nil))

		assert.ErrorContains(t, New().Validate(c), "choose 3.8.0 or later")
	})
}

func TestValidateStorageClassChange(t *testing.T) {
	tests := []struct {
		name      string
		existing  *string
		requested *string
		wantErr   string
	}{
		{name: "unchanged", existing: ptr.To("fast"), requested: ptr.To("fast")},
		{name: "both default", existing: nil, requested: nil},
		{name: "changed", existing: ptr.To("fast"), requested: ptr.To("slow"), wantErr: "storage class cannot be changed"},
		{name: "default to explicit", existing: nil, requested: ptr.To("fast"), wantErr: "storage class cannot be changed"},
		{name: "explicit to default", existing: ptr.To("fast"), requested: nil, wantErr: "storage class cannot be changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := validEngine()
			engine.Storage.StorageClass = tt.requested
			c, _ := newTestContext(t, testInstance(engine), existingCluster("3.8.0", tt.existing))

			err := New().Validate(c)

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
			assert.ErrorContains(t, err, "keep")
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
