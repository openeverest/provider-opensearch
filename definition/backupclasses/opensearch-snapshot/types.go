// Package opensearchsnapshot contains the schema-bearing Go types for the
// "opensearch-snapshot" BackupClass. Each struct here is converted to an OpenAPI
// v3 schema by `provider-sdk generate` and inlined into the generated
// BackupClass manifest.
//
// +k8s:openapi-gen=true
package opensearchsnapshot

// OpensearchSnapshotBackupParameters describes the parameters accepted by Backup CRs that
// target this class (spec.parameters). Add fields the user can set per backup.
type OpensearchSnapshotBackupParameters struct{}

// OpensearchSnapshotRestoreParameters describes the parameters accepted by Restore CRs that
// target this class (spec.parameters). Add fields the user can set per restore.
type OpensearchSnapshotRestoreParameters struct{}

// OpensearchSnapshotPITRParameters describes the per-storage PITR parameters exposed to
// Instance.spec.backup.storages[].pitr.parameters. Add fields a provider needs
// to fine-tune its PITR pipeline (oplog span, compression, retention, etc.).
type OpensearchSnapshotPITRParameters struct{}
