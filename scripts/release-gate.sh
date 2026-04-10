#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFLIGHT_DIR="${PREFLIGHT_DIR:-$(cd "${ROOT}/.." && pwd)/preflight}"
STRATUS_BIN="${STRATUS_BIN:-/tmp/stratus-release-gate-bin}"
STRATUS_GATE_LOG="${STRATUS_GATE_LOG:-/tmp/stratus-release-gate.log}"
STRATUS_GOCACHE="${STRATUS_GOCACHE:-/tmp/stratus-gocache}"
STRATUS_GOTMPDIR="${STRATUS_GOTMPDIR:-/tmp/stratus-gotmp}"

if [[ ! -d "${PREFLIGHT_DIR}" ]]; then
  echo "preflight workspace not found at ${PREFLIGHT_DIR}" >&2
  exit 1
fi

if [[ ! -f "${PREFLIGHT_DIR}/scripts/smoke-fixtures.sh" ]]; then
  echo "preflight smoke script not found in ${PREFLIGHT_DIR}" >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required for the release gate" >&2
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required for the release gate" >&2
  exit 1
fi

if [[ -z "${STRATUS_GATE_ENDPOINT:-}" ]]; then
  STRATUS_GATE_ENDPOINT="$(
    python3 - <<'PY'
import socket
for port in (4566, 4567):
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", port))
    except OSError:
        pass
    else:
        print(f"http://127.0.0.1:{port}")
        raise SystemExit(0)
    finally:
        s.close()
raise SystemExit(1)
PY
  )" || {
    echo "could not find a free local port for the release gate" >&2
    exit 1
  }
fi

PORT="$(
  python3 - "$STRATUS_GATE_ENDPOINT" <<'PY'
import sys
from urllib.parse import urlparse
parsed = urlparse(sys.argv[1])
print(parsed.port or (443 if parsed.scheme == "https" else 80))
PY
)"

DATA_DIR="$(mktemp -d /tmp/stratus-release-gate-data.XXXXXX)"
PID=""

cleanup() {
  if [[ -n "${PID}" ]]; then
    kill "${PID}" >/dev/null 2>&1 || true
    wait "${PID}" >/dev/null 2>&1 || true
  fi
  rm -rf "${DATA_DIR}"
}

trap cleanup EXIT

wait_for_health() {
  local endpoint="$1"
  for _ in $(seq 1 120); do
    if curl -fsS "${endpoint}/_stratus/health" >/dev/null; then
      return 0
    fi
    sleep 0.25
  done

  if [[ -f "${STRATUS_GATE_LOG}" ]]; then
    tail -n 100 "${STRATUS_GATE_LOG}" >&2 || true
  fi
  echo "timed out waiting for stratus at ${endpoint}" >&2
  return 1
}

echo "==> building stratus"
mkdir -p "${STRATUS_GOCACHE}" "${STRATUS_GOTMPDIR}"
env GOCACHE="${STRATUS_GOCACHE}" GOTMPDIR="${STRATUS_GOTMPDIR}" \
  go build -o "${STRATUS_BIN}" ./cmd/stratus

echo "==> starting stratus for Java SDK smoke"
rm -f "${STRATUS_GATE_LOG}"
"${STRATUS_BIN}" --port "${PORT}" --data-dir "${DATA_DIR}" >"${STRATUS_GATE_LOG}" 2>&1 &
PID="$!"
wait_for_health "${STRATUS_GATE_ENDPOINT}"

echo "==> running Java AWS SDK smoke"
STRATUS_ENDPOINT_URL="${STRATUS_GATE_ENDPOINT}" \
  bash "${ROOT}/scripts/smoke-java-sdk.sh"

echo "==> stopping Java smoke instance"
kill "${PID}" >/dev/null 2>&1 || true
wait "${PID}" >/dev/null 2>&1 || true
PID=""
rm -rf "${DATA_DIR}"
DATA_DIR="$(mktemp -d /tmp/stratus-release-gate-data.XXXXXX)"

echo "==> running Preflight CDK smoke"
PREFLIGHT_DIR="${PREFLIGHT_DIR}" \
STRATUS_BIN="${STRATUS_BIN}" \
STRATUS_PREFLIGHT_ENDPOINT="${STRATUS_GATE_ENDPOINT}" \
bash "${ROOT}/scripts/smoke-preflight.sh"

echo "==> release gate passed"
