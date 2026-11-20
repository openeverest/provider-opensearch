#!/usr/bin/env bash
# Scrape test-instance's published metrics endpoint the way a monitoring
# extension does: on a selected pod, as the published credential, TLS
# verified against the published CA with the published server name.
# Usage: scrape.sh [path], default the published path.
set -euo pipefail

src=$(kubectl -n "$NAMESPACE" get instance test-instance -o jsonpath='{.status.monitoring.sources}')
endpoint=$(echo "$src" | jq -c '.metrics[0]')
secret=$(echo "$src" | jq -r '.credentials[] | select(.profile == "metrics") | .secretRef.name')
ca_secret=$(echo "$endpoint" | jq -r '.tls.ca.secretKeyRef.name')
server=$(echo "$endpoint" | jq -r '.tls.serverName')
path=${1:-$(echo "$endpoint" | jq -r '.path')}
selector=$(echo "$endpoint" | jq -r '.podSelector | to_entries | map("\(.key)=\(.value)") | join(",")')
pod_ip=$(kubectl -n "$NAMESPACE" get pods -l "$selector" -o jsonpath='{.items[0].status.podIP}')

field() { kubectl -n "$NAMESPACE" get secret "$1" -o jsonpath="{.data.$2}" | base64 -d; }
username=$(field "$secret" username)
password=$(field "$secret" password)
ca=$(kubectl -n "$NAMESPACE" get secret "$ca_secret" -o jsonpath='{.data.ca\.crt}')

kubectl -n "$NAMESPACE" exec toolbox -- sh -c "echo '$ca' | base64 -d > /tmp/metrics-ca.crt"
kubectl -n "$NAMESPACE" exec toolbox -- curl -sS --max-time 60 -o /dev/stdout -w '\nHTTP %{http_code}\n' \
  --cacert /tmp/metrics-ca.crt --resolve "$server:9200:$pod_ip" -u "$username:$password" \
  "https://$server:9200$path"
