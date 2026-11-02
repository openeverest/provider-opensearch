package provider

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// supportedServiceTypes are the engine.service.serviceType values the
// provider accepts. ExternalName makes no sense for exposing a cluster this
// way, so it is not offered.
var supportedServiceTypes = []corev1.ServiceType{
	corev1.ServiceTypeClusterIP,
	corev1.ServiceTypeNodePort,
	corev1.ServiceTypeLoadBalancer,
}

// exposedServiceName is the Service the provider manages directly to expose
// the cluster as LoadBalancer or NodePort. The operator's own <name> service
// is always ClusterIP; it has no field to change that, and a NodePort service
// it can build (pkg/builders.NewNodePortService) is never wired into its
// reconciler. See https://github.com/openeverest/provider-opensearch/issues/8
func exposedServiceName(instanceName string) string {
	return instanceName + "-exposed"
}

// syncExposedService creates or updates the provider-managed exposed Service
// when engine.Service asks for LoadBalancer or NodePort access, and removes
// it otherwise (e.g. the user reverted to the default ClusterIP).
func syncExposedService(c *controller.Context, engine corev1alpha1.ComponentSpec) error {
	svc := engine.Service
	if svc == nil || svc.ServiceType == "" || svc.ServiceType == corev1.ServiceTypeClusterIP {
		return deleteExposedService(c)
	}

	meta := c.ObjectMeta(exposedServiceName(c.Name()))
	meta.Annotations = svc.Annotations

	exposed := &corev1.Service{
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			Type: svc.ServiceType,
			// The main service already covers transport/metrics/rca for
			// in-cluster use; only the REST API should be reachable from
			// outside the cluster.
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Protocol:   corev1.ProtocolTCP,
				Port:       httpPort,
				TargetPort: intstr.FromInt32(httpPort),
			}},
			Selector: map[string]string{clusterLabel: c.Name()},
		},
	}

	if svc.ServiceType == corev1.ServiceTypeLoadBalancer && svc.LoadBalancerService != nil {
		ranges := svc.LoadBalancerService.SourceRanges.NormalizedSourceRanges()
		if len(ranges) > 0 {
			exposed.Spec.LoadBalancerSourceRanges = []string(ranges)
		}
	}

	// Service objects have no finalizer, so the owner reference c.Apply sets
	// is enough for Kubernetes to garbage-collect this on Instance deletion;
	// Cleanup does not need to handle it.
	return c.Apply(exposed)
}

func deleteExposedService(c *controller.Context) error {
	svc := &corev1.Service{ObjectMeta: c.ObjectMeta(exposedServiceName(c.Name()))}
	return c.Delete(svc)
}

// validateServiceType rejects a service type the provider does not support.
func validateServiceType(svc *corev1alpha1.Service) error {
	if svc == nil || svc.ServiceType == "" {
		return nil
	}
	if !slices.Contains(supportedServiceTypes, svc.ServiceType) {
		return fmt.Errorf("%s: unsupported service type %q: supported types are %v",
			common.ComponentEngine, svc.ServiceType, supportedServiceTypes)
	}
	return nil
}
