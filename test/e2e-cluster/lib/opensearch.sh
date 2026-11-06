#!/usr/bin/env bash
# Call an Instance's OpenSearch REST API from the "toolbox" pod in $NAMESPACE
# (see toolbox.yaml), with what the provider publishes: host, port and
# credentials from the <instance>-conn Secret. TLS is verified against the
# operator-generated CA in <instance>-ca; nothing runs with -k.
#
# Usage: opensearch.sh <instance> <METHOD> <path> [json-body]
set -euo pipefail

instance=$1 method=$2 path=$3 body=${4:-}

field() {
  kubectl -n "$NAMESPACE" get secret "$1" -o jsonpath="{.data.$2}" | base64 -d
}

host=$(field "$instance-conn" host)
port=$(field "$instance-conn" port)
username=$(field "$instance-conn" username)
password=$(field "$instance-conn" password)
ca=$(kubectl -n "$NAMESPACE" get secret "$instance-ca" -o jsonpath='{.data.ca\.crt}')

kubectl -n "$NAMESPACE" exec toolbox -- sh -c "echo '$ca' | base64 -d > /tmp/$instance-ca.crt"
kubectl -n "$NAMESPACE" exec toolbox -- curl -sS --fail-with-body --max-time 60 \
  --cacert "/tmp/$instance-ca.crt" -u "$username:$password" \
  -X "$method" -H 'Content-Type: application/json' ${body:+-d "$body"} \
  "https://$host:$port$path"
