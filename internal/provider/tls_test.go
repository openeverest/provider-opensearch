package provider

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/internal/common"
)

type testCA struct {
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	certPEM []byte
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// serverSecret returns a Secret with a server certificate for host signed by
// ca, its key in PKCS#8 unless pkcs1 is set.
func (ca testCA) serverSecret(t *testing.T, name, host string, pkcs1 bool) *corev1.Secret {
	t.Helper()
	return ca.serverSecretFor(t, name, []string{host}, pkcs1)
}

// serverSecretFor is serverSecret for a certificate with several names.
func (ca testCA) serverSecretFor(t *testing.T, name string, dnsNames []string, pkcs1 bool) *corev1.Secret {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyPEM, err := pkcs8PEM(key)
	require.NoError(t, err)
	if pkcs1 {
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: keyPEM,
			caKey:                   ca.certPEM,
		},
	}
}

func withParameters(in *corev1alpha1.Instance, raw string) *corev1alpha1.Instance {
	in.Spec.Parameters = &runtime.RawExtension{Raw: []byte(raw)}
	return in
}

func TestCheckCertificateSecret(t *testing.T) {
	ca := newTestCA(t)
	host := clientHost(testName, testNamespace, common.TopologyStandard)

	tests := []struct {
		name   string
		secret func() *corev1.Secret
		want   string
	}{
		{name: "valid", secret: func() *corev1.Secret { return ca.serverSecret(t, "c", host, false) }},
		{name: "missing keys", secret: func() *corev1.Secret {
			s := ca.serverSecret(t, "c", host, false)
			delete(s.Data, caKey)
			delete(s.Data, corev1.TLSPrivateKeyKey)
			return s
		}, want: `missing "tls.key", "ca.crt"`},
		{name: "PKCS#1 key", secret: func() *corev1.Secret { return ca.serverSecret(t, "c", host, true) }, want: "PKCS#8"},
		{name: "other host", secret: func() *corev1.Secret { return ca.serverSecret(t, "c", "other.svc", false) }, want: "not valid for " + host},
		{name: "other CA", secret: func() *corev1.Secret {
			s := ca.serverSecret(t, "c", host, false)
			s.Data[caKey] = newTestCA(t).certPEM
			return s
		}, want: "not valid for"},
		{name: "key of another certificate", secret: func() *corev1.Secret {
			s := ca.serverSecret(t, "c", host, false)
			s.Data[corev1.TLSPrivateKeyKey] = ca.serverSecret(t, "d", host, false).Data[corev1.TLSPrivateKeyKey]
			return s
		}, want: "tls.crt and tls.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCertificateSecret(tt.secret(), host)
			if tt.want == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestCertificateDNSNames(t *testing.T) {
	standard := certificateDNSNames(testName, testNamespace, common.TopologyStandard)
	assert.Contains(t, standard, clientHost(testName, testNamespace, common.TopologyStandard))

	dedicated := certificateDNSNames(testName, testNamespace, common.TopologyDedicated)
	assert.Equal(t, clientHost(testName, testNamespace, common.TopologyDedicated), dedicated[0])
	assert.Contains(t, dedicated, testName+"."+testNamespace+".svc")
}

func TestSyncCustomCertificate(t *testing.T) {
	ca := newTestCA(t)
	in := withParameters(testInstance(validEngine()), `{"tls":{"certificateSecretName":"my-cert"}}`)
	host := clientHost(testName, testNamespace, common.TopologyStandard)

	t.Run("waits for the certificate secret", func(t *testing.T) {
		c, _ := newTestContext(t, in.DeepCopy())

		err := New().Sync(c)

		var wait *controller.WaitError
		require.ErrorAs(t, err, &wait)
	})

	c, cl := newTestContext(t, in.DeepCopy(), ca.serverSecret(t, "my-cert", host, false))
	require.NoError(t, New().Sync(c))

	cluster := &opensearchv1.OpenSearchCluster{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Name: testName, Namespace: testNamespace}, cluster))
	security := cluster.Spec.Security
	assert.True(t, security.Tls.Transport.Generate, "the transport certificate stays generated")
	assert.False(t, security.Tls.Http.Generate)
	assert.Equal(t, "my-cert", security.Tls.Http.Secret.Name)
	assert.Equal(t, trustedCAName(testName), security.Tls.Http.CaSecret.Name)
	assert.Equal(t, []string{adminDN(testName)}, security.Tls.Http.AdminDn)
	assert.Equal(t, adminCertName(testName), security.Config.AdminSecret.Name)

	get := func(name string) *corev1.Secret {
		s := &corev1.Secret{}
		require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Name: name, Namespace: testNamespace}, s))
		return s
	}
	adminCA := get(adminCAName(testName))
	admin := get(adminCertName(testName))
	assert.Equal(t, ca.certPEM, admin.Data[caKey], "securityadmin verifies the nodes with the user's CA")
	adminCert, err := parseCertificatePEM(admin.Data[corev1.TLSCertKey])
	require.NoError(t, err)
	adminCACert, err := parseCertificatePEM(adminCA.Data[caKey])
	require.NoError(t, err)
	require.NoError(t, adminCert.CheckSignatureFrom(adminCACert))
	assert.Equal(t, adminDN(testName), adminCert.Subject.String())

	trusted, err := parseBundle(get(trustedCAName(testName)).Data[caKey])
	require.NoError(t, err)
	assert.Len(t, trusted, 2, "the nodes trust the user's CA and the admin CA")

	t.Run("keeps the admin certificate", func(t *testing.T) {
		require.NoError(t, New().Sync(c))
		assert.Equal(t, admin.Data[corev1.TLSCertKey], get(adminCertName(testName)).Data[corev1.TLSCertKey])
	})

	t.Run("reissues an expiring admin certificate", func(t *testing.T) {
		assert.False(t, adminCertValid(admin.Data[corev1.TLSCertKey], adminCA.Data[caKey], time.Now().Add(adminCertValidity)))
		assert.False(t, adminCertValid(admin.Data[corev1.TLSCertKey], newTestCA(t).certPEM, time.Now()))
	})

	t.Run("publishes the user's CA", func(t *testing.T) {
		got, err := clientCA(c)
		require.NoError(t, err)
		assert.Equal(t, ca.certPEM, got)
	})
}

func parseBundle(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no certificates")
	}
	return certs, nil
}

func TestSyncUserSecret(t *testing.T) {
	in := testInstance(validEngine())
	in.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: "creds"}
	user := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte("s3cret-Passw0rd")},
	}
	c, cl := newTestContext(t, in, user)

	require.NoError(t, New().Sync(c))

	cluster := &opensearchv1.OpenSearchCluster{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Name: testName, Namespace: testNamespace}, cluster))
	assert.Equal(t, adminCredentialsName(testName), cluster.Spec.Security.Config.AdminCredentialsSecret.Name)
	assert.True(t, cluster.Spec.Security.Tls.Http.Generate)

	creds := &corev1.Secret{}
	key := client.ObjectKey{Name: adminCredentialsName(testName), Namespace: testNamespace}
	require.NoError(t, cl.Get(t.Context(), key, creds))
	assert.Equal(t, "admin", string(creds.Data["username"]))
	assert.Equal(t, "s3cret-Passw0rd", string(creds.Data["password"]))

	t.Run("does not follow later edits", func(t *testing.T) {
		user.Data["password"] = []byte("changed")
		require.NoError(t, cl.Update(t.Context(), user))
		require.NoError(t, New().Sync(c))
		require.NoError(t, cl.Get(t.Context(), key, creds))
		assert.Equal(t, "s3cret-Passw0rd", string(creds.Data["password"]))
	})
}

func TestValidateReferencesSecurity(t *testing.T) {
	ca := newTestCA(t)
	host := clientHost(testName, testNamespace, common.TopologyStandard)
	userSecret := func(data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: testNamespace}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	withUser := func(in *corev1alpha1.Instance) *corev1alpha1.Instance {
		in.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: "creds"}
		return in
	}
	customCluster := func() *opensearchv1.OpenSearchCluster {
		cluster := existingCluster("", nil)
		cluster.Spec.Security = &opensearchv1.Security{Tls: &opensearchv1.TlsConfig{Http: &opensearchv1.TlsConfigHttp{
			TlsCertificateConfig: opensearchv1.TlsCertificateConfig{Secret: corev1.LocalObjectReference{Name: "my-cert"}},
		}}}
		return cluster
	}
	certParams := `{"tls":{"certificateSecretName":"my-cert"}}`

	tests := []struct {
		name string
		in   *corev1alpha1.Instance
		objs []client.Object
		want []string
	}{
		{name: "certificate secret", in: withParameters(testInstance(validEngine()), certParams),
			objs: []client.Object{ca.serverSecret(t, "my-cert", host, false)}},
		{name: "certificate secret missing", in: withParameters(testInstance(validEngine()), certParams),
			want: []string{"spec.parameters.tls.certificateSecretName"}},
		{name: "certificate for another host", in: withParameters(testInstance(validEngine()), certParams),
			objs: []client.Object{ca.serverSecret(t, "my-cert", "other.svc", false)},
			want: []string{"spec.parameters.tls.certificateSecretName"}},
		{name: "issuer not found", in: withParameters(testInstance(validEngine()), `{"tls":{"issuer":{"name":"ca"}}}`),
			want: []string{"spec.parameters.tls.issuer.name"}},
		{name: "external issuer is not looked up", in: withParameters(testInstance(validEngine()),
			`{"tls":{"issuer":{"name":"pca","kind":"AWSPCAClusterIssuer","group":"awspca.cert-manager.io"}}}`)},
		{name: "user secret", in: withUser(testInstance(validEngine())),
			objs: []client.Object{userSecret(map[string]string{"username": "admin", "password": "p"})}},
		{name: "user secret missing", in: withUser(testInstance(validEngine())),
			want: []string{"spec.userSecretRef.name"}},
		{name: "user secret without password", in: withUser(testInstance(validEngine())),
			objs: []client.Object{userSecret(map[string]string{"username": "admin"})},
			want: []string{"spec.userSecretRef.name"}},
		{name: "user secret for another user", in: withUser(testInstance(validEngine())),
			objs: []client.Object{userSecret(map[string]string{"username": "bob", "password": "p"})},
			want: []string{"spec.userSecretRef.name"}},

		// Against the running cluster.
		{name: "custom certificate on a generated cluster", in: withParameters(testInstance(validEngine()), certParams),
			objs: []client.Object{ca.serverSecret(t, "my-cert", host, false), existingCluster("", nil)},
			want: []string{"spec.parameters.tls"}},
		{name: "generated certificate on a custom cluster", in: testInstance(validEngine()),
			objs: []client.Object{customCluster()},
			want: []string{"spec.parameters.tls"}},
		{name: "same custom certificate", in: withParameters(testInstance(validEngine()), certParams),
			objs: []client.Object{ca.serverSecret(t, "my-cert", host, false), customCluster()}},
		{name: "user secret added to a running cluster", in: withUser(testInstance(validEngine())),
			objs: []client.Object{userSecret(map[string]string{"password": "p"}), existingCluster("", nil)},
			want: []string{"spec.userSecretRef"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestContext(t, tt.in, tt.objs...)

			errs, err := New().ValidateReferences(c)

			require.NoError(t, err)
			assert.Equal(t, tt.want, fieldsOf(errs), "errors: %v", errs)
		})
	}
}

func TestSyncAdditionalNames(t *testing.T) {
	ctx := t.Context()
	ca := newTestCA(t)
	key := client.ObjectKey{Namespace: testNamespace, Name: testName}
	params := func(names ...string) string {
		raw, err := json.Marshal(map[string]any{"tls": map[string]any{
			"issuer":          map[string]any{"name": "ca", "kind": "ClusterIssuer"},
			"additionalNames": names,
		}})
		require.NoError(t, err)
		return string(raw)
	}
	certificate := func(t *testing.T, cl client.Client) *unstructured.Unstructured {
		t.Helper()
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: issuedCertificateName(testName)}, cert))
		return cert
	}
	certNames := func(t *testing.T, cl client.Client) []string {
		t.Helper()
		names, _, _ := unstructured.NestedStringSlice(certificate(t, cl).Object, "spec", "dnsNames")
		return names
	}
	// issue plays cert-manager, writing the certificate the Certificate asks for.
	issue := func(t *testing.T, cl client.Client) {
		t.Helper()
		secret := ca.serverSecretFor(t, issuedCertificateName(testName), certNames(t, cl), false)
		existing := &corev1.Secret{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(secret), existing); err == nil {
			existing.Data = secret.Data
			require.NoError(t, cl.Update(ctx, existing))
			return
		}
		require.NoError(t, cl.Create(ctx, secret))
	}
	restartAnnotation := func(t *testing.T, cl client.Client) string {
		t.Helper()
		cluster := &opensearchv1.OpenSearchCluster{}
		require.NoError(t, cl.Get(ctx, key, cluster))
		return cluster.Spec.NodePools[0].Annotations[tlsNamesAnnotation]
	}
	syncStored := func(t *testing.T, cl client.Client) (*controller.Context, error) {
		t.Helper()
		in := &corev1alpha1.Instance{}
		require.NoError(t, cl.Get(ctx, key, in))
		c := controller.NewContext(ctx, cl, in, common.ProviderName)
		return c, New().Sync(c)
	}
	updateInstance := func(t *testing.T, cl client.Client, mutate func(*corev1alpha1.Instance)) {
		t.Helper()
		in := &corev1alpha1.Instance{}
		require.NoError(t, cl.Get(ctx, key, in))
		mutate(in)
		require.NoError(t, cl.Update(ctx, in))
	}
	var wait *controller.WaitError

	t.Run("without names the nodes carry no restart annotation", func(t *testing.T) {
		c, cl := newTestContext(t, withParameters(testInstance(validEngine()), `{"tls":{"issuer":{"name":"ca"}}}`))
		require.ErrorAs(t, New().Sync(c), &wait)
		issue(t, cl)
		_, err := syncStored(t, cl)
		require.NoError(t, err)
		assert.Empty(t, restartAnnotation(t, cl))
	})

	c, cl := newTestContext(t, withParameters(testInstance(validEngine()), params("b.example.com", "a.example.com")))
	require.ErrorAs(t, New().Sync(c), &wait)
	assert.Subset(t, certNames(t, cl), []string{"a.example.com", "b.example.com"}, "a new instance takes the names right away")
	assert.Subset(t, certNames(t, cl), certificateDNSNames(testName, testNamespace, common.TopologyStandard))
	issuerRef, _, _ := unstructured.NestedStringMap(certificate(t, cl).Object, "spec", "issuerRef")
	assert.Equal(t, map[string]string{"group": "cert-manager.io", "kind": "ClusterIssuer", "name": "ca"}, issuerRef)
	issue(t, cl)
	_, err := syncStored(t, cl)
	require.NoError(t, err)
	created := restartAnnotation(t, cl)
	assert.Equal(t, namesHash([]string{"a.example.com", "b.example.com"}), created)

	updateInstance(t, cl, func(in *corev1alpha1.Instance) {
		in.Spec.Parameters.Raw = []byte(params("c.example.com"))
	})
	c, err = syncStored(t, cl)
	require.NoError(t, err)
	pending := c.GetPendingMaintenance()
	require.Len(t, pending, 1, "a change on a running instance waits for approval")
	assert.Equal(t, "tls-names-"+namesHash([]string{"c.example.com"}), pending[0].ApprovalToken)
	assert.Equal(t, controller.MaintenanceRollingRestart, pending[0].Severity)
	assert.Contains(t, certNames(t, cl), "a.example.com")
	assert.NotContains(t, certNames(t, cl), "c.example.com")
	assert.Equal(t, created, restartAnnotation(t, cl))

	updateInstance(t, cl, func(in *corev1alpha1.Instance) {
		in.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: pending[0].ApprovalToken}
	})
	_, err = syncStored(t, cl)
	require.ErrorAs(t, err, &wait, "waits for cert-manager to reissue the certificate")
	assert.Contains(t, certNames(t, cl), "c.example.com")
	assert.NotContains(t, certNames(t, cl), "a.example.com")
	assert.Equal(t, created, restartAnnotation(t, cl), "the nodes restart only once the certificate carries the names")

	issue(t, cl)
	c, err = syncStored(t, cl)
	require.NoError(t, err)
	assert.Equal(t, namesHash([]string{"c.example.com"}), restartAnnotation(t, cl))
	assert.Empty(t, c.GetPendingMaintenance())
}

func TestSyncExternalIssuer(t *testing.T) {
	in := withParameters(testInstance(validEngine()),
		`{"tls":{"issuer":{"name":"pca","kind":"AWSPCAClusterIssuer","group":"awspca.cert-manager.io"}}}`)
	c, cl := newTestContext(t, in)
	var wait *controller.WaitError
	require.ErrorAs(t, New().Sync(c), &wait)

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: issuedCertificateName(testName)}, cert))
	issuerRef, _, _ := unstructured.NestedStringMap(cert.Object, "spec", "issuerRef")
	assert.Equal(t, map[string]string{"group": "awspca.cert-manager.io", "kind": "AWSPCAClusterIssuer", "name": "pca"}, issuerRef)
}
