#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE_DIR="${ROOT}/test/fixtures/java-sdk-smoke"
ENDPOINT_URL="${STRATUS_ENDPOINT_URL:-http://127.0.0.1:4566}"

if ! command -v java >/dev/null 2>&1; then
  echo "java is required for the Java SDK fixture" >&2
  exit 1
fi
if ! command -v mvn >/dev/null 2>&1; then
  echo "maven is required for the Java SDK fixture" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to probe stratus health" >&2
  exit 1
fi

if ! curl -fsS "${ENDPOINT_URL}/_stratus/health" >/dev/null; then
  echo "stratus is not healthy at ${ENDPOINT_URL}" >&2
  echo "start it in another terminal with:" >&2
  echo "  stratus --log-format pretty --log-level debug" >&2
  exit 1
fi

pushd "${FIXTURE_DIR}" >/dev/null
mvn -q -Dstratus.endpoint="${ENDPOINT_URL}" test
popd >/dev/null
