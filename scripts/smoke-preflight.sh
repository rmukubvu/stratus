#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFLIGHT_DIR="${PREFLIGHT_DIR:-$(cd "${ROOT}/.." && pwd)/preflight-stratus}"
PREFLIGHT_TARGET="${PREFLIGHT_TARGET:-cdk}"
STRATUS_BIN="${STRATUS_BIN:-/tmp/stratus-preflight-bin}"

if [[ ! -d "${PREFLIGHT_DIR}" ]]; then
  echo "preflight workspace not found at ${PREFLIGHT_DIR}" >&2
  exit 1
fi
if [[ ! -f "${PREFLIGHT_DIR}/scripts/smoke-fixtures.sh" ]]; then
  echo "preflight smoke script not found in ${PREFLIGHT_DIR}" >&2
  exit 1
fi

if [[ -z "${STRATUS_PREFLIGHT_ENDPOINT:-}" ]]; then
  if python3 - <<'PY'
import socket
s = socket.socket()
try:
    s.bind(("127.0.0.1", 4566))
except OSError:
    raise SystemExit(1)
finally:
    s.close()
PY
  then
    STRATUS_PREFLIGHT_ENDPOINT="http://127.0.0.1:4566"
  else
    STRATUS_PREFLIGHT_ENDPOINT="http://127.0.0.1:4567"
  fi
fi

STRATUS_PREFLIGHT_PORT="$(
  python3 - "$STRATUS_PREFLIGHT_ENDPOINT" <<'PY'
import sys
from urllib.parse import urlparse
parsed = urlparse(sys.argv[1])
print(parsed.port or (443 if parsed.scheme == "https" else 80))
PY
)"

env GOCACHE=/tmp/stratus-gocache GOTMPDIR=/tmp/stratus-gotmp \
  go build -o "${STRATUS_BIN}" ./cmd/stratus

pushd "${PREFLIGHT_DIR}" >/dev/null
env \
  EMULATOR_COMMAND="${STRATUS_BIN}" \
  EMULATOR_ENDPOINT="${STRATUS_PREFLIGHT_ENDPOINT}" \
  EMULATOR_PORT="${STRATUS_PREFLIGHT_PORT}" \
  bash ./scripts/smoke-fixtures.sh "${PREFLIGHT_TARGET}"
popd >/dev/null
