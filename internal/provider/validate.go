package provider

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// maxInstanceNameLength keeps every name the operator derives from the
// cluster name within the 63-character label limit, e.g. the
// "<name>-securityconfig-update" Job and StatefulSet revision labels.
const maxInstanceNameLength = 36

var (
	minStorage = resource.MustParse("1Gi")
	minCPU     = resource.MustParse("500m")
	// The operator gives the JVM half of the memory request as heap.
	minMemory = resource.MustParse("2Gi")
)

func validate(c *controller.Context) error {
	in := c.Instance()

	if err := validateName(in.Name); err != nil {
		return err
	}

	if t := in.GetTopologyType(); t != "" && t != common.TopologyStandard {
		return fmt.Errorf("unsupported topology %q: only %q is supported", t, common.TopologyStandard)
	}

	for name := range in.Spec.Components {
		if name != common.ComponentEngine {
			return fmt.Errorf("unsupported component %q: only %q is supported", name, common.ComponentEngine)
		}
	}
	engine, ok := in.Spec.Components[common.ComponentEngine]
	if !ok {
		return fmt.Errorf("component %q is required", common.ComponentEngine)
	}

	if engine.Replicas != nil && *engine.Replicas < 1 {
		return fmt.Errorf("%s: replicas must be at least 1", common.ComponentEngine)
	}

	if engine.Storage == nil || engine.Storage.Size.IsZero() {
		return fmt.Errorf("%s: storage size is required", common.ComponentEngine)
	}
	if engine.Storage.Size.Cmp(minStorage) < 0 {
		return fmt.Errorf("%s: storage size must be at least %s", common.ComponentEngine, minStorage.String())
	}

	if r := engine.Resources; r != nil {
		res := nodeResources(r)
		if cpu, ok := res.Requests[corev1.ResourceCPU]; ok && cpu.Cmp(minCPU) < 0 {
			return fmt.Errorf("%s: cpu must be at least %s", common.ComponentEngine, minCPU.String())
		}
		if mem, ok := res.Requests[corev1.ResourceMemory]; ok && mem.Cmp(minMemory) < 0 {
			return fmt.Errorf("%s: memory must be at least %s", common.ComponentEngine, minMemory.String())
		}
	}

	return validateAgainstExisting(c, engine.Storage.Size)
}

func validateName(name string) error {
	if len(name) > maxInstanceNameLength {
		return fmt.Errorf("name %q must be at most %d characters", name, maxInstanceNameLength)
	}
	if errs := validation.IsDNS1035Label(name); len(errs) > 0 {
		return fmt.Errorf("name %q is invalid: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

// validateAgainstExisting rejects changes the running cluster cannot take.
func validateAgainstExisting(c *controller.Context, storageSize resource.Quantity) error {
	existing := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(existing, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get existing OpenSearchCluster: %w", err)
	}

	for _, pool := range existing.Spec.NodePools {
		if pool.Component == nodePoolName && storageSize.Cmp(pool.DiskSize) < 0 {
			return fmt.Errorf("%s: storage size cannot be decreased from %s to %s",
				common.ComponentEngine, pool.DiskSize.String(), storageSize.String())
		}
	}
	return nil
}
