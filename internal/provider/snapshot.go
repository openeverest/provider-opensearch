package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// s3RepoType is the repository type name the repository-s3 plugin registers
// under. Only S3-compatible BackupStorages are supported.
const s3RepoType = "s3"

// s3Plugin is added to general.pluginsList when backups are enabled, so the
// operator downloads it at pod start.
const s3Plugin = "repository-s3"

// snapshotClientName is the name of the repository-s3 named client whose
// settings (access_key, secret_key via keystore; region, endpoint via repo
// settings) back a given repository. One client per BackupStorage, named
// after it, since each may point at a different S3-compatible endpoint.
func snapshotClientName(storageName string) string {
	return storageName
}

// buildBackupConfig resolves the Instance's registered backup storages into
// the operator's snapshotRepositories, keystore and pluginsList. Returns
// zero values when backup is not enabled or no storages are registered.
func buildBackupConfig(c *controller.Context) ([]opensearchv1.SnapshotRepoConfig, []opensearchv1.KeystoreValue, []string, error) {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled || len(backupCfg.Storages) == 0 {
		return nil, nil, nil, nil
	}

	repos := make([]opensearchv1.SnapshotRepoConfig, 0, len(backupCfg.Storages))
	keystore := make([]opensearchv1.KeystoreValue, 0, len(backupCfg.Storages))
	for _, entry := range backupCfg.Storages {
		repo, kv, err := buildSnapshotRepository(c, entry)
		if err != nil {
			return nil, nil, nil, err
		}
		repos = append(repos, repo)
		keystore = append(keystore, kv)
	}

	return repos, keystore, []string{s3Plugin}, nil
}

func buildSnapshotRepository(c *controller.Context, entry corev1alpha1.InstanceBackupStorage) (opensearchv1.SnapshotRepoConfig, opensearchv1.KeystoreValue, error) {
	name := entry.StorageRef.Name
	bs, err := c.BackupStorage(name)
	if err != nil {
		return opensearchv1.SnapshotRepoConfig{}, opensearchv1.KeystoreValue{}, fmt.Errorf("backup storage %q: %w", name, err)
	}
	settings, err := s3RepositorySettings(bs, repositoryBasePath(c.Namespace(), c.Name()))
	if err != nil {
		return opensearchv1.SnapshotRepoConfig{}, opensearchv1.KeystoreValue{}, err
	}

	repo := opensearchv1.SnapshotRepoConfig{
		Name:     name,
		Type:     s3RepoType,
		Settings: settings,
	}

	client := snapshotClientName(name)
	keystore := opensearchv1.KeystoreValue{
		Secret: corev1.LocalObjectReference{Name: bs.Spec.S3.CredentialsSecretRef.Name},
		KeyMappings: map[string]string{
			"AWS_ACCESS_KEY_ID":     fmt.Sprintf("s3.client.%s.access_key", client),
			"AWS_SECRET_ACCESS_KEY": fmt.Sprintf("s3.client.%s.secret_key", client),
		},
	}

	return repo, keystore, nil
}

// repositoryBasePath is where an Instance keeps its snapshots in a bucket.
// OpenSearch corrupts a repository that two clusters write to, so every
// Instance writes under its own path, even when they share a BackupStorage.
func repositoryBasePath(namespace, instance string) string {
	return namespace + "/" + instance
}

// s3RepositorySettings returns the repository-s3 settings for a backup
// storage. The client, named after the storage, holds its credentials in the
// keystore.
func s3RepositorySettings(bs *backupv1alpha1.BackupStorage, basePath string) (map[string]string, error) {
	if bs.Spec.Type != backupv1alpha1.BackupStorageTypeS3 || bs.Spec.S3 == nil {
		return nil, fmt.Errorf("backup storage %q: only S3-compatible storage is supported", bs.Name)
	}
	s3 := bs.Spec.S3
	settings := map[string]string{
		"bucket":    s3.Bucket,
		"client":    snapshotClientName(bs.Name),
		"region":    s3.Region,
		"base_path": basePath,
	}
	if s3.EndpointURL != "" {
		settings["endpoint"] = s3.EndpointURL
	}
	if s3.ForcePathStyle != nil {
		settings["path_style_access"] = fmt.Sprintf("%t", *s3.ForcePathStyle)
	}
	return settings, nil
}
