package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// testBackupStorage returns an accepted s3 BackupStorage named name, with
// parameters params, and its credentials Secret.
func testBackupStorage(name string, params map[string]any) (*backupv1alpha1.BackupStorage, *corev1.Secret) {
	raw, _ := json.Marshal(params)
	bs := &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: backupv1alpha1.BackupStorageSpec{
			BackupStorageClassRef: commonv1alpha1.ObjectRef{Name: s3class.ClassName},
			Location: backupv1alpha1.BackupStorageLocation{
				ObjectStore: &backupv1alpha1.ObjectStoreLocation{Bucket: "my-bucket", Prefix: "team-a"},
			},
			Parameters: &runtime.RawExtension{Raw: raw},
			Authentication: commonv1alpha1.Authentication{
				Mode:                 s3class.AuthModeAccessKey,
				CredentialsSecretRef: &commonv1alpha1.SecretRef{Name: name + "-creds"},
			},
		},
		Status: backupv1alpha1.BackupStorageStatus{Conditions: []metav1.Condition{{
			Type: backupv1alpha1.BackupStorageConditionAccepted, Status: metav1.ConditionTrue,
			Reason: backupv1alpha1.ReasonAccepted, ObservedGeneration: 1,
		}}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-creds", Namespace: testNamespace},
		Data: map[string][]byte{
			s3class.KeyAWSAccessKeyID:     []byte("key"),
			s3class.KeyAWSSecretAccessKey: []byte("secret"),
		},
	}
	return bs, secret
}

func minioParams() map[string]any {
	return map[string]any{
		"region":         "us-east-1",
		"endpointURL":    "https://minio.example.com",
		"forcePathStyle": true,
		"implementation": "minio",
	}
}

func instanceWithBackup(storages ...corev1alpha1.InstanceBackupStorage) *corev1alpha1.Instance {
	in := testInstance(validEngine())
	in.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
		Enabled:  true,
		ClassRef: commonv1alpha1.ObjectRef{Name: backupClassName},
		Storages: storages,
	}
	return in
}

func storageEntry(name string) corev1alpha1.InstanceBackupStorage {
	return corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: name}}
}

// storageReason returns the reason the runtime reports for the entry name.
func storageReason(t *testing.T, c *controller.Context, name string) string {
	t.Helper()
	storages, err := c.BackupStorages()
	require.NoError(t, err)
	for _, s := range storages {
		if s.Name() == name {
			return s.Reason
		}
	}
	t.Fatalf("no entry %q", name)
	return ""
}

func TestBuildBackupConfig(t *testing.T) {
	noCluster := &opensearchv1.OpenSearchCluster{}

	t.Run("backup not enabled", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		repos, keystore, plugins, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		assert.Nil(t, repos)
		assert.Nil(t, keystore)
		assert.Nil(t, plugins)
	})

	t.Run("enabled but no storages", func(t *testing.T) {
		c, _ := newTestContext(t, instanceWithBackup())

		repos, keystore, plugins, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		assert.Nil(t, repos)
		assert.Nil(t, keystore)
		assert.Nil(t, plugins)
	})

	t.Run("one s3 storage writes under the Instance's path", func(t *testing.T) {
		bs, secret := testBackupStorage("my-storage", minioParams())
		c, _ := newTestContext(t, instanceWithBackup(storageEntry("my-storage")), bs, secret)

		repos, keystore, plugins, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		require.Len(t, repos, 1)
		assert.Equal(t, "my-storage", repos[0].Name)
		assert.Equal(t, s3RepoType, repos[0].Type)
		assert.Equal(t, map[string]string{
			"bucket":            "my-bucket",
			"client":            "my-storage",
			"region":            "us-east-1",
			"endpoint":          "https://minio.example.com",
			"path_style_access": "true",
			// Two clusters writing to one repository corrupt it.
			"base_path": "team-a/backups/" + testNamespace + "/" + testName + "/uid/my-storage",
		}, repos[0].Settings)

		require.Len(t, keystore, 1)
		assert.Equal(t, "my-storage-creds", keystore[0].Secret.Name)
		assert.Equal(t, map[string]string{
			"AWS_ACCESS_KEY_ID":     "s3.client.my-storage.access_key",
			"AWS_SECRET_ACCESS_KEY": "s3.client.my-storage.secret_key",
		}, keystore[0].KeyMappings)

		assert.Equal(t, []string{s3Plugin}, plugins)
		assert.Empty(t, storageReason(t, c, "my-storage"))
	})

	t.Run("missing storage is not added", func(t *testing.T) {
		c, _ := newTestContext(t, instanceWithBackup(storageEntry("ghost")))

		repos, keystore, plugins, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		assert.Nil(t, repos)
		assert.Nil(t, keystore)
		assert.Nil(t, plugins)
	})

	t.Run("storage that is not usable keeps what the cluster has", func(t *testing.T) {
		bs, secret := testBackupStorage("my-storage", minioParams())
		bs.Generation = 2 // core has not judged the change yet
		live := &opensearchv1.OpenSearchCluster{}
		live.Spec.General.SnapshotRepositories = []opensearchv1.SnapshotRepoConfig{
			{Name: "my-storage", Type: s3RepoType, Settings: map[string]string{"base_path": "old"}},
		}
		live.Spec.General.Keystore = []opensearchv1.KeystoreValue{{
			Secret:      corev1.LocalObjectReference{Name: "old-creds"},
			KeyMappings: map[string]string{"AWS_ACCESS_KEY_ID": "s3.client.my-storage.access_key"},
		}}
		c, _ := newTestContext(t, instanceWithBackup(storageEntry("my-storage")), bs, secret)

		repos, keystore, plugins, err := buildBackupConfig(c, live)

		require.NoError(t, err)
		assert.Equal(t, live.Spec.General.SnapshotRepositories, repos)
		assert.Equal(t, live.Spec.General.Keystore, keystore)
		assert.Equal(t, []string{s3Plugin}, plugins)
		assert.Equal(t, corev1alpha1.ReasonBackupStorageNotAccepted, storageReason(t, c, "my-storage"))
	})

	t.Run("gcs is rejected and does not block other storages", func(t *testing.T) {
		gcsParams := minioParams()
		gcsParams["implementation"] = "gcs"
		gcs, gcsSecret := testBackupStorage("gcs", gcsParams)
		ok, okSecret := testBackupStorage("ok", minioParams())
		c, _ := newTestContext(t, instanceWithBackup(storageEntry("gcs"), storageEntry("ok")), gcs, gcsSecret, ok, okSecret)

		repos, _, _, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		require.Len(t, repos, 1)
		assert.Equal(t, "ok", repos[0].Name)
		assert.Equal(t, corev1alpha1.ReasonImplementationUnsupported, storageReason(t, c, "gcs"))
	})

	t.Run("custom CA is rejected", func(t *testing.T) {
		bs, secret := testBackupStorage("my-storage", minioParams())
		bs.Spec.TLS = &commonv1alpha1.TLSConfig{CA: &commonv1alpha1.CABundleSource{
			SecretKeyRef: &commonv1alpha1.SecretKeyRef{Name: "ca", Key: "ca.crt"},
		}}
		c, _ := newTestContext(t, instanceWithBackup(storageEntry("my-storage")), bs, secret)

		repos, _, _, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		assert.Empty(t, repos)
		assert.Equal(t, corev1alpha1.ReasonTLSOptionUnsupported, storageReason(t, c, "my-storage"))
	})

	t.Run("multiple storages", func(t *testing.T) {
		a, aSecret := testBackupStorage("storage-a", minioParams())
		b, bSecret := testBackupStorage("storage-b", minioParams())
		c, _ := newTestContext(t, instanceWithBackup(storageEntry("storage-a"), storageEntry("storage-b")), a, aSecret, b, bSecret)

		repos, keystore, _, err := buildBackupConfig(c, noCluster)

		require.NoError(t, err)
		assert.Len(t, repos, 2)
		assert.Len(t, keystore, 2)
	})
}
