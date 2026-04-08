#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE_DIR="${ROOT}/test/fixtures/cdk-inline-httpapi"
ENDPOINT_URL="${STRATUS_ENDPOINT_URL:-http://127.0.0.1:4566}"
STACK_NAME="${STRATUS_CDK_STACK_NAME:-StratusInlineHttpApi}"

if ! command -v node >/dev/null 2>&1; then
  echo "node is required for the CDK fixture" >&2
  exit 1
fi
if ! command -v npx >/dev/null 2>&1; then
  echo "npx is required for the CDK fixture" >&2
  exit 1
fi
if ! command -v aws >/dev/null 2>&1; then
  echo "aws cli is required for the CDK fixture" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required for the CDK fixture" >&2
  exit 1
fi
if [[ ! -d "${FIXTURE_DIR}/node_modules" ]]; then
  echo "missing node_modules in ${FIXTURE_DIR}; run npm install there first" >&2
  exit 1
fi

pushd "${FIXTURE_DIR}" >/dev/null
env \
  AWS_ENDPOINT_URL="${ENDPOINT_URL}" \
  AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}" \
  AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}" \
  AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}" \
  CDK_DEFAULT_ACCOUNT=000000000000 \
  CDK_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}" \
  npx cdk deploy "${STACK_NAME}" --require-approval never
popd >/dev/null

API_ID="$(
  AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}" \
  AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}" \
  AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}" \
  AWS_EC2_METADATA_DISABLED=true \
  aws --endpoint-url "${ENDPOINT_URL}" \
    cloudformation list-stack-resources \
    --stack-name "${STACK_NAME}" \
    --query "StackResourceSummaries[?ResourceType=='AWS::ApiGatewayV2::Api'].PhysicalResourceId | [0]" \
    --output text
)"

if [[ -z "${API_ID}" || "${API_ID}" == "None" ]]; then
  echo "failed to resolve deployed API id from stack ${STACK_NAME}" >&2
  exit 1
fi

curl -fsS "${ENDPOINT_URL}/_aws/execute-api/${API_ID}/hello"
