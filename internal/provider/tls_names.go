package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-opensearch/definition"
)

// appliedAdditionalNames returns the additional names to request the issued
// HTTP certificate for. A new instance takes them as they are. On a running
// one a change reissues the certificate and restarts the nodes, so it waits
// for maintenance approval and keeps the names the Certificate has until
// then.
func appliedAdditionalNames(c *controller.Context, t definition.TLSParameters, topology string, running bool) ([]string, error) {
	if t.Issuer == nil {
		return nil, nil
	}
	desired := additionalNames(t)
	if !running {
		return desired, nil
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	key := client.ObjectKey{Namespace: c.Namespace(), Name: issuedCertificateName(c.Name())}
	if err := c.Client().Get(c.Context(), key, cert); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return desired, nil
		}
		return nil, fmt.Errorf("get certificate %q: %w", key.Name, err)
	}
	dnsNames, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	recorded := extraNames(dnsNames, certificateDNSNames(c.Name(), c.Namespace(), topology))
	if slices.Equal(desired, recorded) {
		return desired, nil
	}
	description := "Reissue the HTTP certificate without additional names and restart the OpenSearch nodes"
	if len(desired) > 0 {
		description = fmt.Sprintf("Reissue the HTTP certificate for the additional names %s and restart the OpenSearch nodes",
			strings.Join(desired, ", "))
	}
	if c.RequestMaintenance("tls-names-"+namesHash(desired), description, controller.MaintenanceRollingRestart) {
		return desired, nil
	}
	return recorded, nil
}

// servedAdditionalNames returns the additional names in the issued HTTP
// certificate cert-manager has written, sorted.
func servedAdditionalNames(c *controller.Context, t definition.TLSParameters, topology string) ([]string, error) {
	if t.Issuer == nil {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := c.Get(secret, issuedCertificateName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get certificate secret %q: %w", issuedCertificateName(c.Name()), err)
	}
	cert, err := parseCertificatePEM(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return nil, fmt.Errorf("certificate secret %q: tls.crt: %w", secret.Name, err)
	}
	return extraNames(cert.DNSNames, certificateDNSNames(c.Name(), c.Namespace(), topology)), nil
}

// extraNames returns the names not in own, sorted and without duplicates.
func extraNames(names, own []string) []string {
	var extra []string
	for _, name := range names {
		if !slices.Contains(own, name) {
			extra = append(extra, name)
		}
	}
	slices.Sort(extra)
	return slices.Compact(extra)
}

func namesHash(names []string) string {
	sum := sha256.Sum256([]byte(strings.Join(names, ",")))
	return hex.EncodeToString(sum[:])[:10]
}
