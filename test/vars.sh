#!/bin/bash

# Environment variables sourced by the `make test-integration*` targets before
# running chainsaw. Use it to pin the operator/engine versions your tests run
# against so they are reproducible locally and in CI.
#
# Reference values in chainsaw test files via ($values) bindings or plain
# environment substitution in `script:` steps.

export PROVIDER_ROOT_PATH=${PROVIDER_ROOT_PATH:-${PWD}}
echo "PROVIDER_ROOT_PATH=${PROVIDER_ROOT_PATH}"

export OPENSEARCH_OPERATOR_VERSION=${OPENSEARCH_OPERATOR_VERSION:-"3.0.14"}
echo "OPENSEARCH_OPERATOR_VERSION=${OPENSEARCH_OPERATOR_VERSION}"

export OPENSEARCH_VERSION=${OPENSEARCH_VERSION:-"3.8.0"}
echo "OPENSEARCH_VERSION=${OPENSEARCH_VERSION}"
