#!/usr/bin/env bash
# Call an Instance's OpenSearch Dashboards API from the "toolbox" pod in
# $NAMESPACE (see toolbox.yaml) through the <instance>-dashboards service,
# logged in with the published admin credentials. TLS is verified against the
# CA published in the <instance>-conn Secret, which also signs the Dashboards
# certificate the operator generates.
#
# Usage: dashboards.sh <instance> <path>
set -euo pipefail

instance=$1 path=$2

field() {
  kubectl -n "$NAMESPACE" get secret "$instance-conn" -o jsonpath="{.data.$1}" | base64 -d
}

username=$(field username)
password=$(field password)
ca=$(kubectl -n "$NAMESPACE" get secret "$instance-conn" -o jsonpath='{.data.ca\.crt}')

kubectl -n "$NAMESPACE" exec toolbox -- sh -c "echo '$ca' | base64 -d > /tmp/$instance-ca.crt"
kubectl -n "$NAMESPACE" exec toolbox -- curl -sS --fail-with-body --max-time 60 \
  --cacert "/tmp/$instance-ca.crt" -u "$username:$password" \
  "https://$instance-dashboards.$NAMESPACE.svc:5601$path"
