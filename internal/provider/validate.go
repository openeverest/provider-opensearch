package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

// Compile-time check that Provider validates referenced objects.
var _ controller.ReferenceValidator = (*Provider)(nil)

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

var (
	componentsPath = field.NewPath("spec", "components")
	enginePath     = componentsPath.Key(common.ComponentEngine)
	dashboardsPath = componentsPath.Key(common.ComponentDashboards)
)

// Validate checks the rules about the Instance alone and returns every error
// with its field path. It gets no client, so it cannot read the cluster.
//
// The runtime calls it at admission and before every Sync, once the Instance
// matches the topologies, components, versions, parameter schemas and
// supportedFields the Provider declares, so those are not repeated here.
// Rules about transitions (downgrades, storage changes) need in.Old, which is
// only set on an update at admission; ValidateReferences repeats them against
// the running cluster in case admission was skipped.
func (p *Provider) Validate(in controller.ValidationInput) controller.ValidationResult {
	inst := in.Instance
	var errs field.ErrorList

	errs = append(errs, validateName(inst.Name)...)

	engine, ok := inst.Spec.Components[common.ComponentEngine]
	if !ok {
		return controller.ValidationResult{Errors: append(errs, field.Required(enginePath, ""))}
	}
	errs = append(errs, validateEngine(engine)...)

	if dashboards, ok := inst.Spec.Components[common.ComponentDashboards]; ok {
		errs = append(errs, validateServiceType(dashboardsPath.Child("service", "serviceType"), dashboards.Service)...)
	}

	errs = append(errs, validateBackupSchedules(inst.Spec.Backup)...)

	if in.Provider != nil {
		spec := &in.Provider.Spec
		errs = append(errs, validateDashboardsVersion(spec, inst)...)
		if in.Old != nil {
			errs = append(errs, validateVersionChange(
				resolveComponentVersion(spec, in.Old, common.ComponentEngine, common.ComponentTypeOpensearch),
				resolveComponentVersion(spec, inst, common.ComponentEngine, common.ComponentTypeOpensearch),
			)...)
		}
	}

	if in.Old != nil {
		if oldEngine, ok := in.Old.Spec.Components[common.ComponentEngine]; ok && oldEngine.Storage != nil && engine.Storage != nil {
			errs = append(errs, validateStorageChange(
				oldEngine.Storage.Size, ptr.Deref(oldEngine.Storage.StorageClass, ""), engine.Storage)...)
		}
	}

	return controller.ValidationResult{Errors: errs}
}

func validateName(name string) field.ErrorList {
	path := field.NewPath("metadata", "name")
	var errs field.ErrorList
	if len(name) > maxInstanceNameLength {
		errs = append(errs, field.TooLong(path, name, maxInstanceNameLength))
	}
	for _, msg := range validation.IsDNS1035Label(name) {
		errs = append(errs, field.Invalid(path, name, msg))
	}
	return errs
}

func validateEngine(engine corev1alpha1.ComponentSpec) field.ErrorList {
	var errs field.ErrorList

	storagePath := enginePath.Child("storage", "size")
	switch {
	case engine.Storage == nil || engine.Storage.Size.IsZero():
		errs = append(errs, field.Required(storagePath, ""))
	case engine.Storage.Size.Cmp(minStorage) < 0:
		errs = append(errs, field.Invalid(storagePath, engine.Storage.Size.String(),
			fmt.Sprintf("must be at least %s", minStorage.String())))
	}

	if r := engine.Resources; r != nil {
		res := nodeResources(r)
		if cpu, ok := res.Requests[corev1.ResourceCPU]; ok && cpu.Cmp(minCPU) < 0 {
			errs = append(errs, field.Invalid(enginePath.Child("resources", "requests", "cpu"), cpu.String(),
				fmt.Sprintf("must be at least %s", minCPU.String())))
		}
		if mem, ok := res.Requests[corev1.ResourceMemory]; ok && mem.Cmp(minMemory) < 0 {
			errs = append(errs, field.Invalid(enginePath.Child("resources", "requests", "memory"), mem.String(),
				fmt.Sprintf("must be at least %s, half of it is the JVM heap", minMemory.String())))
		}
	}

	if _, err := resolveAdditionalConfig(engine); err != nil {
		errs = append(errs, field.Invalid(enginePath.Child("parameters", "configuration"), "", err.Error()))
	}

	errs = append(errs, validateServiceType(enginePath.Child("service", "serviceType"), engine.Service)...)
	return errs
}

// validateBackupSchedules rejects a schedule name that cannot form a valid
// OpensearchSnapshotPolicy object name (snapshotPolicyName combines it with
// the Instance name).
func validateBackupSchedules(backupCfg *corev1alpha1.InstanceBackupSpec) field.ErrorList {
	if backupCfg == nil {
		return nil
	}
	var errs field.ErrorList
	storagesPath := field.NewPath("spec", "backup", "storages")
	for i, storage := range backupCfg.Storages {
		for j, sched := range storage.Schedules {
			for _, msg := range validation.IsDNS1123Label(sched.Name) {
				errs = append(errs, field.Invalid(storagesPath.Index(i).Child("schedules").Index(j).Child("name"), sched.Name, msg))
			}
		}
	}
	return errs
}

// validateDashboardsVersion requires Dashboards to run the engine's version:
// OpenSearch Dashboards refuses to connect to a cluster of another version.
func validateDashboardsVersion(spec *corev1alpha1.ProviderSpec, inst *corev1alpha1.Instance) field.ErrorList {
	if _, ok := inst.Spec.Components[common.ComponentDashboards]; !ok {
		return nil
	}
	engineVersion := resolveComponentVersion(spec, inst, common.ComponentEngine, common.ComponentTypeOpensearch)
	dashboardsVersion := resolveComponentVersion(spec, inst, common.ComponentDashboards, common.ComponentTypeDashboards)
	if engineVersion == "" || dashboardsVersion == "" || engineVersion == dashboardsVersion {
		return nil
	}
	return field.ErrorList{field.Invalid(dashboardsPath.Child("version"), dashboardsVersion,
		fmt.Sprintf("must match the engine version %s", engineVersion))}
}

// resolveComponentVersion returns the version a component runs: its pinned
// version, else the one from the version bundle in force, else its type's
// default. This is the order the runtime applies before Sync; Validate and
// ValidateReferences get the Instance before that resolution.
func resolveComponentVersion(spec *corev1alpha1.ProviderSpec, inst *corev1alpha1.Instance, component, componentType string) string {
	if c, ok := inst.Spec.Components[component]; ok && c.Version != "" {
		return c.Version
	}
	if name := controller.EffectiveVersionBundleName(spec, inst); name != "" {
		if bundle, err := controller.ResolveVersionBundle(spec, name); err == nil {
			if v := bundle.Components[component]; v != "" {
				return v
			}
		}
	}
	if v := controller.GetDefaultVersion(spec, componentType); v != nil {
		return v.Version
	}
	return ""
}

// validateVersionChange rejects downgrades and upgrades that skip a major
// version. Either side being empty or unparsable means there is nothing to
// compare.
func validateVersionChange(from, to string) field.ErrorList {
	if from == "" || to == "" {
		return nil
	}
	fromV, err := utilversion.ParseSemantic(from)
	if err != nil {
		return nil
	}
	toV, err := utilversion.ParseSemantic(to)
	if err != nil {
		return nil
	}
	path := field.NewPath("spec", "version")
	if toV.LessThan(fromV) {
		return field.ErrorList{field.Forbidden(path,
			fmt.Sprintf("OpenSearch cannot be downgraded from %s to %s; choose %s or later", from, to, from))}
	}
	if toV.Major() > fromV.Major()+1 {
		return field.ErrorList{field.Forbidden(path,
			fmt.Sprintf("upgrading from %s to %s skips a major version; upgrade to a %d.x release first", from, to, fromV.Major()+1))}
	}
	return nil
}

// validateStorageChange rejects shrinking the volumes and changing their
// StorageClass, neither of which a StatefulSet can do in place.
func validateStorageChange(currentSize resource.Quantity, currentClass string, requested *corev1alpha1.Storage) field.ErrorList {
	var errs field.ErrorList
	if requested.Size.Cmp(currentSize) < 0 {
		errs = append(errs, field.Forbidden(enginePath.Child("storage", "size"),
			fmt.Sprintf("cannot be decreased from %s to %s", currentSize.String(), requested.Size.String())))
	}
	if requestedClass := ptr.Deref(requested.StorageClass, ""); requestedClass != currentClass {
		errs = append(errs, field.Forbidden(enginePath.Child("storage", "storageClass"),
			fmt.Sprintf("cannot be changed after creation; keep %q", currentClass)))
	}
	return errs
}

// ValidateReferences checks the rules that need other objects: the backup
// storages, and the running OpenSearchCluster with its StorageClass. It
// repeats the transition rules against the running cluster, since Validate
// only sees the previous Instance at admission.
func (p *Provider) ValidateReferences(c *controller.Context) (field.ErrorList, error) {
	errs, err := validateBackupStorages(c)
	if err != nil {
		return nil, err
	}

	existing := &opensearchv1.OpenSearchCluster{}
	if err := c.Get(existing, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return errs, nil
		}
		return nil, fmt.Errorf("get existing OpenSearchCluster: %w", err)
	}

	spec, err := c.ProviderSpec()
	if err != nil {
		return nil, err
	}
	requested := resolveComponentVersion(spec, c.Instance(), common.ComponentEngine, common.ComponentTypeOpensearch)
	errs = append(errs, validateVersionChange(existing.Status.Version, requested)...)

	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if engine.Storage == nil {
		return errs, nil
	}
	for _, pool := range existing.Spec.NodePools {
		if pool.Component != nodePoolName {
			continue
		}
		var currentClass *string
		if pool.Persistence != nil && pool.Persistence.PVC != nil {
			currentClass = pool.Persistence.PVC.StorageClassName
		}
		changeErrs := validateStorageChange(pool.DiskSize, ptr.Deref(currentClass, ""), engine.Storage)
		errs = append(errs, changeErrs...)
		if len(changeErrs) == 0 && engine.Storage.Size.Cmp(pool.DiskSize) > 0 {
			expandErrs, err := validateStorageExpansion(c, currentClass)
			if err != nil {
				return nil, err
			}
			errs = append(errs, expandErrs...)
		}
	}
	return errs, nil
}

// validateBackupStorages rejects a backup storage the snapshot repository
// cannot use: only S3-compatible storage is supported.
func validateBackupStorages(c *controller.Context) (field.ErrorList, error) {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled {
		return nil, nil
	}
	var errs field.ErrorList
	for i, entry := range backupCfg.Storages {
		bs, err := c.BackupStorage(entry.StorageRef.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Whether a referent exists is the runtime's check.
				continue
			}
			return nil, fmt.Errorf("get backup storage %q: %w", entry.StorageRef.Name, err)
		}
		if bs.Spec.Type != backupv1alpha1.BackupStorageTypeS3 || bs.Spec.S3 == nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "backup", "storages").Index(i).Child("storageRef", "name"),
				entry.StorageRef.Name, "only S3-compatible backup storage is supported"))
		}
	}
	return errs, nil
}

// validateStorageExpansion rejects a storage increase when the StorageClass
// does not support it. className is nil when the PVC uses the cluster's
// default StorageClass.
//
// StorageClass is cluster-scoped, so this goes through c.Client() directly:
// c.Get/c.List always scope to the Instance's namespace, which would never
// match a cluster-scoped object.
func validateStorageExpansion(c *controller.Context, className *string) (field.ErrorList, error) {
	var sc *storagev1.StorageClass
	if name := ptr.Deref(className, ""); name != "" {
		sc = &storagev1.StorageClass{}
		if err := c.Client().Get(c.Context(), client.ObjectKey{Name: name}, sc); err != nil {
			if apierrors.IsNotFound(err) {
				return field.ErrorList{field.NotFound(enginePath.Child("storage", "storageClass"), name)}, nil
			}
			return nil, fmt.Errorf("get storage class %q: %w", name, err)
		}
	} else {
		var err error
		if sc, err = defaultStorageClass(c); err != nil {
			return nil, err
		}
		if sc == nil {
			// The default StorageClass could not be resolved; don't block an
			// increase over something we can't verify.
			return nil, nil
		}
	}

	if !ptr.Deref(sc.AllowVolumeExpansion, false) {
		return field.ErrorList{field.Forbidden(enginePath.Child("storage", "size"),
			fmt.Sprintf("storage class %q does not allow volume expansion", sc.Name))}, nil
	}
	return nil, nil
}

// defaultStorageClassAnnotation marks the cluster's default StorageClass.
// See https://kubernetes.io/docs/concepts/storage/storage-classes/#default-storageclass
const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

func defaultStorageClass(c *controller.Context) (*storagev1.StorageClass, error) {
	list := &storagev1.StorageClassList{}
	if err := c.Client().List(c.Context(), list); err != nil {
		return nil, fmt.Errorf("list storage classes: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Annotations[defaultStorageClassAnnotation] == "true" {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}
