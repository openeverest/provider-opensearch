#!/usr/bin/env bash
# Issue a certificate into a Secret of $NAMESPACE, standing in for one a user
# brings, from a CA Issuer named "ca" (created on first use, see
# ca-issuer.yaml). The Secret holds tls.crt, tls.key and ca.crt.
#
# Usage: certificate.sh <secret> <dns-name> [PKCS8|PKCS1]
set -euo pipefail

secret=$1 dns_name=$2 encoding=${3:-PKCS8}
dir=$(dirname "$0")

# cert-manager's webhook may still be starting right after its install.
for _ in $(seq 30); do
  kubectl -n "$NAMESPACE" apply -f "$dir/ca-issuer.yaml" >/dev/null 2>&1 && break
  sleep 5
done
kubectl -n "$NAMESPACE" apply -f "$dir/ca-issuer.yaml" >/dev/null
kubectl -n "$NAMESPACE" wait issuer/ca --for=condition=Ready --timeout 2m >/dev/null

kubectl -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: $secret
spec:
  secretName: $secret
  dnsNames: [$dns_name]
  usages: [server auth, digital signature, key encipherment]
  privateKey:
    algorithm: RSA
    size: 2048
    encoding: $encoding
  issuerRef:
    kind: Issuer
    name: ca
EOF
kubectl -n "$NAMESPACE" wait certificate/"$secret" --for=condition=Ready --timeout 2m >/dev/null
