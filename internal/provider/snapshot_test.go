package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

func testBackupStorage(name string, s3 *backupv1alpha1.BackupStorageS3Spec) *backupv1alpha1.BackupStorage {
	bs := &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       backupv1alpha1.BackupStorageSpec{Type: backupv1alpha1.BackupStorageTypeS3, S3: s3},
	}
	if s3 == nil {
		bs.Spec.Type = ""
	}
	return bs
}

func validS3Spec() *backupv1alpha1.BackupStorageS3Spec {
	return &backupv1alpha1.BackupStorageS3Spec{
		Bucket:               "my-bucket",
		Region:               "us-east-1",
		EndpointURL:          "https://minio.example.com",
		ForcePathStyle:       ptr.To(true),
		CredentialsSecretRef: commonv1alpha1.SecretRef{Name: "my-storage-creds"},
	}
}

func instanceWithBackup(storages ...corev1alpha1.InstanceBackupStorage) *corev1alpha1.Instance {
	in := testInstance(validEngine())
	in.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
		Enabled:  true,
		ClassRef: commonv1alpha1.ObjectRef{Name: "opensearch-snapshot"},
		Storages: storages,
	}
	return in
}

func TestBuildBackupConfig(t *testing.T) {
	t.Run("backup not enabled", func(t *testing.T) {
		c, _ := newTestContext(t, testInstance(validEngine()))

		repos, keystore, plugins, err := buildBackupConfig(c)

		require.NoError(t, err)
		assert.Nil(t, repos)
		assert.Nil(t, keystore)
		assert.Nil(t, plugins)
	})

	t.Run("enabled but no storages", func(t *testing.T) {
		in := instanceWithBackup()
		c, _ := newTestContext(t, in)

		repos, keystore, plugins, err := buildBackupConfig(c)

		require.NoError(t, err)
		assert.Nil(t, repos)
		assert.Nil(t, keystore)
		assert.Nil(t, plugins)
	})

	t.Run("one S3 storage", func(t *testing.T) {
		in := instanceWithBackup(corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "my-storage"},
		})
		storage := testBackupStorage("my-storage", validS3Spec())
		c, _ := newTestContext(t, in, storage)

		repos, keystore, plugins, err := buildBackupConfig(c)

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
		}, repos[0].Settings)

		require.Len(t, keystore, 1)
		assert.Equal(t, "my-storage-creds", keystore[0].Secret.Name)
		assert.Equal(t, map[string]string{
			"AWS_ACCESS_KEY_ID":     "s3.client.my-storage.access_key",
			"AWS_SECRET_ACCESS_KEY": "s3.client.my-storage.secret_key",
		}, keystore[0].KeyMappings)

		assert.Equal(t, []string{s3Plugin}, plugins)
	})

	t.Run("storage not found", func(t *testing.T) {
		in := instanceWithBackup(corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "ghost"},
		})
		c, _ := newTestContext(t, in)

		_, _, _, err := buildBackupConfig(c)

		assert.ErrorContains(t, err, "ghost")
	})

	t.Run("non-S3 storage is rejected", func(t *testing.T) {
		in := instanceWithBackup(corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "my-storage"},
		})
		storage := testBackupStorage("my-storage", nil)
		c, _ := newTestContext(t, in, storage)

		_, _, _, err := buildBackupConfig(c)

		assert.ErrorContains(t, err, "only S3-compatible storage is supported")
	})

	t.Run("multiple storages", func(t *testing.T) {
		in := instanceWithBackup(
			corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: "storage-a"}},
			corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: "storage-b"}},
		)
		storageA := testBackupStorage("storage-a", validS3Spec())
		storageB := testBackupStorage("storage-b", validS3Spec())
		c, _ := newTestContext(t, in, storageA, storageB)

		repos, keystore, _, err := buildBackupConfig(c)

		require.NoError(t, err)
		assert.Len(t, repos, 2)
		assert.Len(t, keystore, 2)
	})
}
