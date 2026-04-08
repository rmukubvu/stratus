#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE_DIR="${ROOT}/test/fixtures/terraform-foundation"
ENDPOINT_URL="${STRATUS_ENDPOINT_URL:-http://127.0.0.1:4566}"

if ! command -v terraform >/dev/null 2>&1; then
  echo "terraform is required for the terraform fixture" >&2
  exit 1
fi

pushd "${FIXTURE_DIR}" >/dev/null
terraform init -input=false
terraform apply -input=false -auto-approve -var="endpoint_url=${ENDPOINT_URL}"
popd >/dev/null
