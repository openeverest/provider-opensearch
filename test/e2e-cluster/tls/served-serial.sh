#!/usr/bin/env bash
# Print the serial number of the HTTP certificate an Instance serves, as seen
# from the toolbox pod. Only reads the certificate, so it skips verification;
# opensearch.sh is what checks trust.
#
# Usage: served-serial.sh <instance>
set -euo pipefail

instance=$1
host=$(kubectl -n "$NAMESPACE" get secret "$instance-conn" -o jsonpath='{.data.host}' | base64 -d)
port=$(kubectl -n "$NAMESPACE" get secret "$instance-conn" -o jsonpath='{.data.port}' | base64 -d)
kubectl -n "$NAMESPACE" exec toolbox -- curl -sS -k -o /dev/null --max-time 30 -w '%{certs}' \
  "https://$host:$port/" | sed -n 's/^Serial Number: *//p' | head -1
