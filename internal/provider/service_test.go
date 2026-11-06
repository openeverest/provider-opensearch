package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

func getService(t *testing.T, cl client.Client, name string) (*corev1.Service, error) {
	t.Helper()
	svc := &corev1.Service{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, svc)
	return svc, err
}

func TestSyncExposedService(t *testing.T) {
	t.Run("no service set: nothing is created", func(t *testing.T) {
		c, cl := newTestContext(t, testInstance(validEngine()))

		require.NoError(t, syncExposedService(c, validEngine()))

		_, err := getService(t, cl, exposedServiceName(testName))
		assert.True(t, errors.IsNotFound(err))
	})

	t.Run("ClusterIP: nothing is created", func(t *testing.T) {
		engine := validEngine()
		engine.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP}
		c, cl := newTestContext(t, testInstance(engine))

		require.NoError(t, syncExposedService(c, engine))

		_, err := getService(t, cl, exposedServiceName(testName))
		assert.True(t, errors.IsNotFound(err))
	})

	t.Run("LoadBalancer: creates the exposed service", func(t *testing.T) {
		engine := validEngine()
		engine.Service = &corev1alpha1.Service{
			ServiceType: corev1.ServiceTypeLoadBalancer,
			Annotations: map[string]string{"example.com/lb": "internal"},
			LoadBalancerService: &corev1alpha1.LoadBalancerService{
				SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
			},
		}
		c, cl := newTestContext(t, testInstance(engine))

		require.NoError(t, syncExposedService(c, engine))

		svc, err := getService(t, cl, exposedServiceName(testName))
		require.NoError(t, err)
		assert.Equal(t, corev1.ServiceTypeLoadBalancer, svc.Spec.Type)
		assert.Equal(t, map[string]string{"example.com/lb": "internal"}, svc.Annotations)
		assert.Equal(t, map[string]string{clusterLabel: testName}, svc.Spec.Selector)
		assert.Equal(t, []string{"10.0.0.0/8"}, svc.Spec.LoadBalancerSourceRanges)
		require.Len(t, svc.Spec.Ports, 1)
		assert.Equal(t, httpPort, svc.Spec.Ports[0].Port)
		assert.Equal(t, intstr.FromInt32(httpPort), svc.Spec.Ports[0].TargetPort)
		require.Len(t, svc.OwnerReferences, 1)
		assert.Equal(t, testName, svc.OwnerReferences[0].Name)
	})

	t.Run("NodePort: no source ranges applied", func(t *testing.T) {
		engine := validEngine()
		engine.Service = &corev1alpha1.Service{
			ServiceType: corev1.ServiceTypeNodePort,
			LoadBalancerService: &corev1alpha1.LoadBalancerService{
				SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
			},
		}
		c, cl := newTestContext(t, testInstance(engine))

		require.NoError(t, syncExposedService(c, engine))

		svc, err := getService(t, cl, exposedServiceName(testName))
		require.NoError(t, err)
		assert.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)
		assert.Empty(t, svc.Spec.LoadBalancerSourceRanges)
	})

	t.Run("reverting to ClusterIP removes a previously created service", func(t *testing.T) {
		engine := validEngine()
		engine.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer}
		c, cl := newTestContext(t, testInstance(engine))
		require.NoError(t, syncExposedService(c, engine))
		_, err := getService(t, cl, exposedServiceName(testName))
		require.NoError(t, err, "precondition: the service was created")

		engine.Service.ServiceType = corev1.ServiceTypeClusterIP
		require.NoError(t, syncExposedService(c, engine))

		_, err = getService(t, cl, exposedServiceName(testName))
		assert.True(t, errors.IsNotFound(err))
	})
}

func TestValidateServiceType(t *testing.T) {
	tests := []struct {
		name    string
		svc     *corev1alpha1.Service
		wantErr bool
	}{
		{name: "nil", svc: nil},
		{name: "empty", svc: &corev1alpha1.Service{}},
		{name: "ClusterIP", svc: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP}},
		{name: "LoadBalancer", svc: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer}},
		{name: "NodePort", svc: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort}},
		{
			name:    "ExternalName is unsupported",
			svc:     &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateServiceType(field.NewPath("spec", "components").Key("engine").Child("service", "serviceType"), tt.svc)

			if !tt.wantErr {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, "spec.components[engine].service.serviceType", errs[0].Field)
		})
	}
}

func TestBuildClusterSpecServiceAnnotations(t *testing.T) {
	engine := validEngine()
	engine.Service = &corev1alpha1.Service{Annotations: map[string]string{"example.com/scrape": "true"}}

	spec := buildClusterSpec(testName, engine, "3.8.0", "", nil, nil, nil, nil)

	assert.Equal(t, map[string]string{"example.com/scrape": "true"}, spec.General.Annotations)
}
