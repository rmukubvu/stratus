#!/usr/bin/env bash
# Writes Terraform for every messaging option and checks it: every module
# must be terraform fmt clean and valid, and every option Stratus can
# test-deploy must pass `stratus advise terraform --deploy` (init, apply, a
# plan that finds no changes, destroy) against a running Stratus.
#
#   stratus serve &
#   scripts/smoke-advise-terraform.sh            # STRATUS_BIN defaults to stratus
set -euo pipefail

STRATUS="${STRATUS_BIN:-stratus}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if ! command -v terraform >/dev/null 2>&1; then
  echo "terraform is required" >&2
  exit 1
fi

start() {
  "$STRATUS" advise start messaging "$@" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}

# Defaults favour a plain queue; fan-out answers favour SNS and EventBridge;
# replay with per-key order favours Kinesis.
queue=$(start --title "Order jobs")
fanout=$(start --title "Order fan-out" consumers=2 routing=true)
stream=$(start --title "Order stream" ordering=per_key replay=true consumers=2 peak_rate=1500)
ordered=$(start --title "Ordered jobs" ordering=per_key)
for id in "$queue" "$fanout" "$stream" "$ordered"; do "$STRATUS" advise assess "$id" >/dev/null; done

failed=0
report() { # option, exit code
  if [ "$2" -eq 0 ]; then
    echo "ok    $1: $(grep '^test deploy' "$WORK/$1.out")"
  else
    failed=1
    echo "FAIL  $1" >&2
    cat "$WORK/$1.out" >&2
    grep -E '^Error' "$WORK/$1.log" >&2 || tail -20 "$WORK/$1.log" >&2
  fi
}

# Kinesis first and alone: it fills the provider cache, which Terraform
# cannot fill from several processes at once.
code=0
"$STRATUS" advise terraform "$stream" --option kinesis --out "$WORK/kinesis" --deploy >"$WORK/kinesis.out" 2>"$WORK/kinesis.log" || code=$?
report kinesis "$code"

# Each option deploys into its own throwaway account, so the rest run at once.
pids=()
for pair in "$queue sqs-standard" "$fanout sns-sqs" "$fanout eventbridge"; do
  set -- $pair
  "$STRATUS" advise terraform "$1" --option "$2" --out "$WORK/$2" --deploy >"$WORK/$2.out" 2>"$WORK/$2.log" &
  pids+=("$!:$2")
done

for entry in "${pids[@]}"; do
  pid="${entry%%:*}" option="${entry#*:}"
  code=0
  wait "$pid" || code=$?
  report "$option" "$code"
done
# Modules Stratus cannot test-deploy are still checked: formatted and valid.
"$STRATUS" advise terraform "$ordered" --option sqs-fifo --out "$WORK/sqs-fifo" >/dev/null
"$STRATUS" advise terraform "$stream" --option msk --out "$WORK/msk" >/dev/null
for option in sqs-fifo msk; do
  if terraform -chdir="$WORK/$option" init -backend=false -input=false >/dev/null && terraform -chdir="$WORK/$option" validate >/dev/null; then
    echo "ok    $option: valid (not test-deployable on Stratus)"
  else
    failed=1
    echo "FAIL  $option: terraform validate" >&2
  fi
done
if ! terraform fmt -check -recursive "$WORK" >/dev/null; then
  failed=1
  echo "FAIL  generated Terraform is not terraform fmt clean:" >&2
  terraform fmt -check -recursive -diff "$WORK" >&2 || true
fi
exit "$failed"
