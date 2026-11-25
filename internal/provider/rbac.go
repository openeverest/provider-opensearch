package provider

// Run `make manifests` to regenerate config/rbac/role.yaml from these markers.
// This file contains kubebuilder RBAC markers for controller-gen.
// See: https://book.kubebuilder.io/reference/markers/rbac

// Base RBAC (required by all providers):
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.openeverest.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get
// The runtime writes the connection details returned by Status() into a Secret.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// A new build stays unready until the installed OpenEverest supports it: the
// runtime reads the versions OpenEverest supports on the Provider CRD, then
// records the version it was built for on its Provider's status. Keep
// resourceNames in sync with common.ProviderName.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=providers.core.openeverest.io,verbs=get
// +kubebuilder:rbac:groups=core.openeverest.io,resources=providers/status,resourceNames=opensearch,verbs=patch

// =============================================================================
// PROVIDER-SPECIFIC RBAC
// =============================================================================
// +kubebuilder:rbac:groups=opensearch.org,resources=opensearchclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=opensearch.org,resources=opensearchclusters/status,verbs=get
// The read-only user the Prometheus exporter's scrapers log in as.
// +kubebuilder:rbac:groups=opensearch.org,resources=opensearchusers;opensearchroles;opensearchuserrolebindings,verbs=get;list;watch;create;update;patch;delete
// One OpensearchSnapshotPolicy per enabled Instance.spec.backup.storages[].schedules[] entry.
// +kubebuilder:rbac:groups=opensearch.org,resources=opensearchsnapshotpolicies,verbs=get;list;watch;create;update;patch;delete
// Validate reads the StorageClass to check allowVolumeExpansion before accepting a storage increase.
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// Status compares the OpenSearch pods' image and the node pools' StatefulSet
// revisions with the cluster's spec.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// Status compares the data volumes' capacity with the requested size to report StorageResizing.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// The operator's own service is always ClusterIP; the provider manages a separate
// Service directly for LoadBalancer/NodePort access.
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// spec.parameters.tls.issuer: a cert-manager Certificate signed by the user's issuer.
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers;clusterissuers,verbs=get;list;watch

// =============================================================================
// OPENEVEREST MONITORING RESOURCES
// =============================================================================
// +kubebuilder:rbac:groups=monitoring.openeverest.io,resources=monitoringclasses;monitoringdestinations;monitoringbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.openeverest.io,resources=monitoringbindings/status,verbs=patch

// =============================================================================
// OPENEVEREST BACKUP RESOURCES
// =============================================================================
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupclasses;backupstorageclasses,verbs=get;list;watch
// The runtime claims the shipped BackupClass by applying its Accepted condition.
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupclasses/status,verbs=patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupstorages,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/status,verbs=get;update;patch
// The operator never removes Dashboards once enabled; the provider deletes
// these when the dashboards component is dropped.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete
