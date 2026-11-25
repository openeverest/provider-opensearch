package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// TestValidate runs the checks the runtime runs at admission and before every
// Sync: the schemas, supportedFields and catalog this provider publishes, then
// Validate.
func TestValidate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), "../../charts/provider-opensearch/generated/provider-spec.yaml")

	tests := []struct {
		fixture string
		// old is the previous Instance on an update; empty for a create.
		old string
		// want lists the field paths of the expected errors.
		want []string
		// warning is a substring of an expected admission warning.
		warning string
	}{
		{fixture: "minimal.yaml"},
		{fixture: "configuration.yaml"},
		{fixture: "load-balancer.yaml"},
		{fixture: "dashboards.yaml"},
		{fixture: "dashboards-older-bundle.yaml"},
		{fixture: "name-too-long.yaml", want: []string{"metadata.name"}},
		{fixture: "name-starts-with-digit.yaml", want: []string{"metadata.name"}},
		{fixture: "storage-too-small.yaml", want: []string{"spec.components[engine].storage.size"}},
		{fixture: "cpu-too-small.yaml", want: []string{"spec.components[engine].resources.requests.cpu"}},
		{fixture: "memory-too-small.yaml", want: []string{"spec.components[engine].resources.requests.memory"}},
		{fixture: "forbidden-configuration.yaml", want: []string{"spec.components[engine].parameters.configuration"}},
		{fixture: "external-name-service.yaml", want: []string{"spec.components[engine].service.serviceType"}},
		{fixture: "dashboards-version-mismatch.yaml", want: []string{"spec.components[dashboards].version"}},
		{fixture: "unknown-component.yaml", want: []string{"spec.components[monitoring]"}},
		{fixture: "invalid-schedule-name.yaml", want: []string{"spec.backup.storages[0].schedules[0].name"}},
		{fixture: "standard-explicit.yaml"},
		{fixture: "standard-with-manager.yaml", want: []string{"spec.components[clusterManager]"}},
		{fixture: "tls-secret.yaml"},
		{fixture: "tls-issuer.yaml"},
		{fixture: "tls-both.yaml", want: []string{"spec.parameters.tls.issuer"}},
		{fixture: "tls-bad-kind.yaml", want: []string{"spec.parameters.tls.issuer.kind"}},
		{fixture: "tls-issuer-external.yaml"},
		{fixture: "tls-issuer-names.yaml"},
		{fixture: "tls-names-without-issuer.yaml", want: []string{"spec.parameters.tls.additionalNames"}},
		{fixture: "tls-secret-names.yaml", want: []string{"spec.parameters.tls.additionalNames"}},
		{fixture: "tls-names-invalid.yaml", want: []string{"spec.parameters.tls.additionalNames[0]", "spec.parameters.tls.additionalNames[1]"}},
		{fixture: "user-secret.yaml"},

		// Dedicated topology.
		{fixture: "dedicated.yaml"},
		{fixture: "dedicated-coordinators.yaml"},
		{fixture: "dedicated-single-manager.yaml", warning: "single cluster manager"},
		{fixture: "dedicated-even-managers.yaml", want: []string{"spec.components[clusterManager].replicas"}},
		{fixture: "dedicated-manager-missing.yaml", want: []string{"spec.components[clusterManager]"}},
		{fixture: "dedicated-coordinator-storage.yaml", want: []string{"spec.components[coordinator].storage"}},
		{fixture: "dedicated-coordinator-memory-too-small.yaml", want: []string{"spec.components[coordinator].resources.requests.memory"}},

		// Transitions, only checked on an update at admission.
		{fixture: "version-3.8.0.yaml", old: "version-3.7.0.yaml"},
		{fixture: "version-3.7.0.yaml", old: "version-3.8.0.yaml", want: []string{"spec.version"}},
		// spec.version unset: the bundle frozen in status.version is in force.
		{fixture: "version-3.7.0.yaml", old: "status-3.8.0.yaml", want: []string{"spec.version"}},
		{fixture: "storage-20gi.yaml", old: "minimal.yaml"},
		{fixture: "minimal.yaml", old: "storage-20gi.yaml", want: []string{"spec.components[engine].storage.size"}},
		{fixture: "storage-class-slow.yaml", old: "storage-class-fast.yaml", want: []string{"spec.components[engine].storage.storageClass"}},
		{fixture: "storage-class-fast.yaml", old: "minimal.yaml", want: []string{"spec.components[engine].storage.storageClass"}},
		{fixture: "minimal.yaml", old: "standard-explicit.yaml"},
		{fixture: "dedicated.yaml", old: "minimal.yaml", want: []string{"spec.topology.type"}},
		{fixture: "minimal.yaml", old: "dedicated.yaml", want: []string{"spec.topology.type"}},
		{fixture: "dedicated-coordinators.yaml", old: "dedicated.yaml"},
		{fixture: "dedicated.yaml", old: "dedicated-coordinators.yaml"},
		{fixture: "dedicated-manager-storage-20gi.yaml", old: "dedicated.yaml"},
		{fixture: "dedicated.yaml", old: "dedicated-manager-storage-20gi.yaml", want: []string{"spec.components[clusterManager].storage.size"}},
		{fixture: "tls-secret.yaml", old: "tls-secret.yaml"},
		{fixture: "tls-secret.yaml", old: "minimal.yaml", want: []string{"spec.parameters.tls"}},
		{fixture: "minimal.yaml", old: "tls-issuer.yaml", want: []string{"spec.parameters.tls"}},
		{fixture: "tls-issuer.yaml", old: "tls-secret.yaml", want: []string{"spec.parameters.tls"}},
		{fixture: "tls-issuer-names.yaml", old: "tls-issuer.yaml"},
		{fixture: "tls-issuer.yaml", old: "tls-issuer-names.yaml"},
		{fixture: "tls-issuer-cert-manager-group.yaml", old: "tls-issuer.yaml"},
		{fixture: "tls-issuer-external.yaml", old: "tls-issuer.yaml", want: []string{"spec.parameters.tls"}},
		{fixture: "user-secret.yaml", old: "user-secret.yaml"},
		{fixture: "user-secret.yaml", old: "minimal.yaml", want: []string{"spec.userSecretRef"}},
	}

	for _, tt := range tests {
		name := tt.fixture
		if tt.old != "" {
			name = tt.old + " -> " + tt.fixture
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := validationtest.Load(t, "testdata/validate/"+tt.fixture)
			var old *corev1alpha1.Instance
			if tt.old != "" {
				old = validationtest.Load(t, "testdata/validate/"+tt.old)
			}

			res := v.Run(in, old)

			var got []string
			for _, err := range res.Errors {
				got = append(got, err.Field)
			}
			assert.Equal(t, tt.want, got, "errors: %v", res.Errors)
			if tt.warning != "" {
				assert.True(t, slices.ContainsFunc(res.Warnings, func(w string) bool {
					return strings.Contains(w, tt.warning)
				}), "warnings: %v", res.Warnings)
			}
		})
	}
}

func TestValidateVersionChange(t *testing.T) {
	tests := []struct {
		name      string
		from, to  string
		wantError string
	}{
		{name: "same version", from: "3.8.0", to: "3.8.0"},
		{name: "minor upgrade", from: "3.7.0", to: "3.8.0"},
		{name: "patch upgrade", from: "3.8.0", to: "3.8.1"},
		{name: "next major", from: "3.8.0", to: "4.0.0"},
		{name: "minor downgrade", from: "3.8.0", to: "3.7.0", wantError: "cannot be downgraded from 3.8.0 to 3.7.0; choose 3.8.0 or later"},
		{name: "patch downgrade", from: "3.8.1", to: "3.8.0", wantError: "cannot be downgraded"},
		{name: "major downgrade", from: "3.8.0", to: "2.19.2", wantError: "cannot be downgraded"},
		{name: "major jump", from: "2.19.2", to: "4.0.0", wantError: "upgrade to a 3.x release first"},
		{name: "cluster not started yet", from: "", to: "3.7.0"},
		{name: "unparsable running version", from: "garbage", to: "3.7.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateVersionChange(tt.from, tt.to)

			if tt.wantError == "" {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, "spec.version", errs[0].Field)
			assert.Contains(t, errs[0].Detail, tt.wantError)
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

func fieldsOf(errs field.ErrorList) []string {
	var out []string
	for _, e := range errs {
		out = append(out, e.Field)
	}
	return out
}

func TestValidateReferencesAgainstRunningCluster(t *testing.T) {
	storageClass := func(name string, allowExpansion *bool, isDefault bool) *storagev1.StorageClass {
		sc := &storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: name},
			AllowVolumeExpansion: allowExpansion,
		}
		if isDefault {
			sc.Annotations = map[string]string{defaultStorageClassAnnotation: "true"}
		}
		return sc
	}

	tests := []struct {
		name             string
		runningVersion   string
		requestedVersion string
		requestedSize    string
		existingClass    *string
		requestedClass   *string
		objs             []client.Object
		want             []string
	}{
		{name: "no change", requestedSize: "10Gi", existingClass: ptr.To("fast"), requestedClass: ptr.To("fast")},
		{
			name:          "expansion allowed",
			requestedSize: "20Gi", existingClass: ptr.To("fast"), requestedClass: ptr.To("fast"),
			objs: []client.Object{storageClass("fast", ptr.To(true), false)},
		},
		{
			name:          "expansion not allowed",
			requestedSize: "20Gi", existingClass: ptr.To("fast"), requestedClass: ptr.To("fast"),
			objs: []client.Object{storageClass("fast", ptr.To(false), false)},
			want: []string{"spec.components[engine].storage.size"},
		},
		{
			name:          "AllowVolumeExpansion unset defaults to false",
			requestedSize: "20Gi", existingClass: ptr.To("fast"), requestedClass: ptr.To("fast"),
			objs: []client.Object{storageClass("fast", nil, false)},
			want: []string{"spec.components[engine].storage.size"},
		},
		{
			name:          "storage class not found",
			requestedSize: "20Gi", existingClass: ptr.To("ghost"), requestedClass: ptr.To("ghost"),
			want: []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:          "default storage class allows expansion",
			requestedSize: "20Gi",
			objs:          []client.Object{storageClass("standard", ptr.To(true), true)},
		},
		{
			name:          "default storage class disallows expansion",
			requestedSize: "20Gi",
			objs:          []client.Object{storageClass("standard", ptr.To(false), true)},
			want:          []string{"spec.components[engine].storage.size"},
		},
		{name: "no default storage class resolvable does not block", requestedSize: "20Gi"},
		{
			name:          "shrink",
			requestedSize: "5Gi",
			want:          []string{"spec.components[engine].storage.size"},
		},
		{
			name:          "storage class changed",
			requestedSize: "10Gi", existingClass: ptr.To("fast"), requestedClass: ptr.To("slow"),
			want: []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:           "older than the running version",
			runningVersion: "3.8.0", requestedVersion: "3.7.0", requestedSize: "10Gi",
			want: []string{"spec.version"},
		},
		{
			name:           "newer than the running version",
			runningVersion: "3.7.0", requestedVersion: "3.8.0", requestedSize: "10Gi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := validEngine()
			engine.Version = tt.requestedVersion
			engine.Storage.Size = resource.MustParse(tt.requestedSize)
			engine.Storage.StorageClass = tt.requestedClass
			objs := append([]client.Object{existingCluster(tt.runningVersion, tt.existingClass)}, tt.objs...)
			c, _ := newTestContext(t, testInstance(engine), objs...)

			errs, err := New().ValidateReferences(c)

			require.NoError(t, err)
			assert.Equal(t, tt.want, fieldsOf(errs), "errors: %v", errs)
		})
	}

	t.Run("no cluster yet", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		errs, err := New().ValidateReferences(c)

		require.NoError(t, err)
		assert.Empty(t, errs)
	})
}

func TestValidateReferencesDedicated(t *testing.T) {
	dedicatedCluster := func() *opensearchv1.OpenSearchCluster {
		cluster := existingCluster("", nil)
		pvc := cluster.Spec.NodePools[0].Persistence
		cluster.Spec.NodePools = []opensearchv1.NodePool{
			{Component: managerPoolName, DiskSize: resource.MustParse("5Gi"), Persistence: pvc},
			{Component: dataPoolName, DiskSize: resource.MustParse("10Gi"), Persistence: pvc},
			{Component: coordinatorPoolName, Persistence: &opensearchv1.PersistenceConfig{
				PersistenceSource: opensearchv1.PersistenceSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		}
		return cluster
	}
	manager := func(size string) corev1alpha1.ComponentSpec {
		return corev1alpha1.ComponentSpec{Storage: &corev1alpha1.Storage{Size: resource.MustParse(size)}}
	}

	tests := []struct {
		name     string
		topology string
		manager  *corev1alpha1.ComponentSpec
		existing *opensearchv1.OpenSearchCluster
		want     []string
	}{
		{name: "unchanged", topology: common.TopologyDedicated, manager: ptr.To(manager("5Gi")), existing: dedicatedCluster()},
		{
			name: "manager storage shrunk", topology: common.TopologyDedicated, manager: ptr.To(manager("2Gi")),
			existing: dedicatedCluster(), want: []string{"spec.components[clusterManager].storage.size"},
		},
		{
			name: "standard cluster switched to dedicated", topology: common.TopologyDedicated, manager: ptr.To(manager("5Gi")),
			existing: existingCluster("", nil), want: []string{"spec.topology.type"},
		},
		{
			name: "dedicated cluster switched to standard", existing: dedicatedCluster(),
			want: []string{"spec.topology.type"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance(validEngine())
			if tt.topology != "" {
				in.Spec.Topology = &corev1alpha1.TopologySpec{Type: tt.topology}
			}
			if tt.manager != nil {
				in.Spec.Components[common.ComponentClusterManager] = *tt.manager
			}
			c, _ := newTestContext(t, in, tt.existing)

			errs, err := New().ValidateReferences(c)

			require.NoError(t, err)
			assert.Equal(t, tt.want, fieldsOf(errs), "errors: %v", errs)
		})
	}
}
