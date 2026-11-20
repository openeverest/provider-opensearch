package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/definition/backupclasses"
)

func TestShippedBackupClasses(t *testing.T) {
	p := New()
	classes := p.ShippedBackupClasses()
	require.Len(t, classes, 1)
	class := classes[0]
	assert.Equal(t, backupClassName, class.Name)
	assert.Equal(t, p.Name(), class.Spec.ExecutorProvider())

	entry := class.Spec.FindBackupStorageClass(s3class.ClassName)
	require.NotNil(t, entry)
	assert.Equal(t, []string{s3class.AuthModeAccessKey}, entry.AuthModes)
	require.NotNil(t, entry.ParametersSchema)
	var enum []string
	for _, v := range entry.ParametersSchema.OpenAPIV3Schema.Properties["implementation"].Enum {
		enum = append(enum, string(v.Raw))
	}
	// repository-s3 cannot reach GCS through its S3 client.
	assert.Equal(t, []string{`"aws"`, `"minio"`, `"ceph"`, `"other"`}, enum)
	assert.Equal(t, controller.TLSSupport{}, backupclasses.TLSSupport(backupClassName, s3class.ClassName))
}
