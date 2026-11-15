#!/usr/bin/env bash
# Sets the image of the default OpenSearch version on the Provider, as a
# provider release rebuilding that image would.
set -euo pipefail
kubectl patch provider opensearch --type json -p "[
  {\"op\":\"test\",\"path\":\"/spec/componentTypes/opensearch/versions/1/version\",\"value\":\"3.8.0\"},
  {\"op\":\"replace\",\"path\":\"/spec/componentTypes/opensearch/versions/1/image\",\"value\":\"$1\"}
]"
