package provider

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/definition/backupclasses"
)

// s3RepoType is the repository type name the repository-s3 plugin registers
// under. Only BackupStorages of the s3 class are supported.
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

// buildBackupConfig resolves the Instance's backup storages into the
// operator's snapshotRepositories, keystore and pluginsList. Returns zero
// values when backup is not enabled or no storages are registered.
//
// Each storage the runtime judges usable gets a repository under the path the
// runtime assigns the entry (<prefix>/backups/<namespace>/<instance>/<uid>/<storage>):
// OpenSearch corrupts a repository two clusters write to, so every Instance
// writes under its own path, even when they share a BackupStorage.
//
// An entry that is not usable, or that the provider cannot render, keeps
// what the live cluster has for it, and is not added when the cluster has
// nothing yet (the hold rule): a broken storage never drops a working
// repository, nor blocks the other storages.
func buildBackupConfig(c *controller.Context, live *opensearchv1.OpenSearchCluster) ([]opensearchv1.SnapshotRepoConfig, []opensearchv1.KeystoreValue, []string, error) {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled || len(backupCfg.Storages) == 0 {
		return nil, nil, nil, nil
	}
	storages, err := c.BackupStorages()
	if err != nil {
		return nil, nil, nil, err
	}

	repos := make([]opensearchv1.SnapshotRepoConfig, 0, len(storages))
	keystore := make([]opensearchv1.KeystoreValue, 0, len(storages))
	for _, s := range storages {
		if s.Apply() {
			repo, kv, err := snapshotRepository(c, s.Storage, s.Path)
			if err == nil {
				repos = append(repos, repo)
				keystore = append(keystore, kv)
				continue
			}
			c.RejectBackupStorage(s.Name(), err)
		}
		if repo, kv, ok := liveSnapshotRepository(live, s.Name()); ok {
			repos = append(repos, repo)
			keystore = append(keystore, kv)
		}
	}
	if len(repos) == 0 {
		return nil, nil, nil, nil
	}
	return repos, keystore, []string{s3Plugin}, nil
}

// snapshotRepository renders the repository named after bs, writing under
// basePath, and the keystore entry holding its client's credentials.
func snapshotRepository(c *controller.Context, bs *backupv1alpha1.BackupStorage, basePath string) (opensearchv1.SnapshotRepoConfig, opensearchv1.KeystoreValue, error) {
	settings, err := s3RepositorySettings(c, bs, basePath)
	if err != nil {
		return opensearchv1.SnapshotRepoConfig{}, opensearchv1.KeystoreValue{}, err
	}
	client := snapshotClientName(bs.Name)
	keystore := opensearchv1.KeystoreValue{
		Secret: corev1.LocalObjectReference{Name: bs.Spec.Authentication.CredentialsSecretRef.Name},
		KeyMappings: map[string]string{
			s3class.KeyAWSAccessKeyID:     fmt.Sprintf("s3.client.%s.access_key", client),
			s3class.KeyAWSSecretAccessKey: fmt.Sprintf("s3.client.%s.secret_key", client),
		},
	}
	return opensearchv1.SnapshotRepoConfig{Name: bs.Name, Type: s3RepoType, Settings: settings}, keystore, nil
}

// liveSnapshotRepository returns what the live cluster has for the
// repository named name: the repository and its client's keystore entry.
func liveSnapshotRepository(live *opensearchv1.OpenSearchCluster, name string) (opensearchv1.SnapshotRepoConfig, opensearchv1.KeystoreValue, bool) {
	if live == nil {
		return opensearchv1.SnapshotRepoConfig{}, opensearchv1.KeystoreValue{}, false
	}
	var repo *opensearchv1.SnapshotRepoConfig
	for i := range live.Spec.General.SnapshotRepositories {
		if live.Spec.General.SnapshotRepositories[i].Name == name {
			repo = &live.Spec.General.SnapshotRepositories[i]
			break
		}
	}
	if repo == nil {
		return opensearchv1.SnapshotRepoConfig{}, opensearchv1.KeystoreValue{}, false
	}
	accessKey := fmt.Sprintf("s3.client.%s.access_key", snapshotClientName(name))
	for _, kv := range live.Spec.General.Keystore {
		if kv.KeyMappings[s3class.KeyAWSAccessKeyID] == accessKey {
			return *repo, kv, true
		}
	}
	return opensearchv1.SnapshotRepoConfig{}, opensearchv1.KeystoreValue{}, false
}

// s3RepositorySettings returns the repository-s3 settings for an s3
// BackupStorage, writing under basePath. The client, named after the
// storage, holds its credentials in the keystore.
//
// repository-s3 cannot reach GCS through its S3 client, and has no
// per-client CA or skip-verify setting (see class.yaml): such a storage is
// rejected.
func s3RepositorySettings(c *controller.Context, bs *backupv1alpha1.BackupStorage, basePath string) (map[string]string, error) {
	params, _, err := c.BackupStorageS3(bs)
	if err != nil {
		return nil, err
	}
	if params.EffectiveImplementation() == s3class.ImplementationGCS {
		return nil, fmt.Errorf("%w: repository-s3 cannot use GCS through its S3 client", controller.ErrImplementationUnsupported)
	}
	if _, err := c.BackupStorageTLS(bs, backupclasses.TLSSupport(backupClassName, s3class.ClassName)); err != nil {
		return nil, err
	}
	settings := map[string]string{
		"bucket":    bs.Spec.Location.ObjectStore.Bucket,
		"client":    snapshotClientName(bs.Name),
		"region":    params.Region,
		"endpoint":  params.EndpointURL,
		"base_path": basePath,
	}
	if params.ForcePathStyle {
		settings["path_style_access"] = strconv.FormatBool(true)
	}
	return settings, nil
}
