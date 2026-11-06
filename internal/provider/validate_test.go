package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"
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

		// Transitions, only checked on an update at admission.
		{fixture: "version-3.8.0.yaml", old: "version-3.7.0.yaml"},
		{fixture: "version-3.7.0.yaml", old: "version-3.8.0.yaml", want: []string{"spec.version"}},
		// spec.version unset: the bundle frozen in status.version is in force.
		{fixture: "version-3.7.0.yaml", old: "status-3.8.0.yaml", want: []string{"spec.version"}},
		{fixture: "storage-20gi.yaml", old: "minimal.yaml"},
		{fixture: "minimal.yaml", old: "storage-20gi.yaml", want: []string{"spec.components[engine].storage.size"}},
		{fixture: "storage-class-slow.yaml", old: "storage-class-fast.yaml", want: []string{"spec.components[engine].storage.storageClass"}},
		{fixture: "storage-class-fast.yaml", old: "minimal.yaml", want: []string{"spec.components[engine].storage.storageClass"}},
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

func TestValidateReferencesBackupStorage(t *testing.T) {
	entry := func(name string) corev1alpha1.InstanceBackupStorage {
		return corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: name}}
	}
	notS3 := testBackupStorage("azure", nil)
	notS3.Spec.Type = "azure"
	in := instanceWithBackup(entry("s3"), entry("azure"), entry("missing"))
	c, _ := newTestContext(t, in, testBackupStorage("s3", validS3Spec()), notS3)

	errs, err := New().ValidateReferences(c)

	require.NoError(t, err)
	// A missing storage is reported by the runtime, not here.
	assert.Equal(t, []string{"spec.backup.storages[1].storageRef.name"}, fieldsOf(errs))
}
