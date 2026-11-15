#!/usr/bin/env bash
# Runs Stratus in a Kubernetes pod that mounts the node's docker.sock, then
# checks that Lambda (including a call back into Stratus), RDS and MSK work
# from inside the pod and that deleting the pod leaves no containers behind.
#
# Needs: a cluster whose node runs Docker (minikube --container-runtime=docker),
# kubectl, the AWS CLI, and these images in the node's Docker: stratus:ci,
# postgres:16-alpine, redpandadata/redpanda:v24.2.7,
# public.ecr.aws/lambda/python:3.11.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
node_docker=${NODE_DOCKER:-"minikube ssh --"}
work="$(mktemp -d)"
forward_pid=""

cleanup() {
  [ -n "$forward_pid" ] && kill "$forward_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  kubectl logs stratus-runner -c stratus --tail=50 >&2 || true
  exit 1
}

kubectl apply -f "$here/runner-pod.yaml"
kubectl wait --for=condition=Ready pod/stratus-runner --timeout=180s

node_ip="$(kubectl get pod stratus-runner -o jsonpath='{.status.hostIP}')"
pod_ip="$(kubectl get pod stratus-runner -o jsonpath='{.status.podIP}')"
kubectl logs stratus-runner -c stratus | grep -q "\"reach\":\"host:$node_ip\"" ||
  fail "Stratus did not pick host reach on $node_ip"

kubectl port-forward pod/stratus-runner 4566:4566 >/dev/null 2>&1 &
forward_pid=$!
for _ in $(seq 1 30); do curl -fsS http://127.0.0.1:4566/_stratus/health >/dev/null 2>&1 && break; sleep 1; done

export AWS_ENDPOINT_URL=http://127.0.0.1:4566 AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1

echo "--- Lambda: invoke, and call Stratus back from the function"
cat > "$work/app.py" <<'PY'
import json, os, urllib.request

def handler(event, context):
    with urllib.request.urlopen(os.environ["STRATUS_CALLBACK"] + "/_stratus/health", timeout=5) as r:
        return {"ok": True, "callback": r.status}
PY
(cd "$work" && zip -q fn.zip app.py)
aws lambda create-function --function-name k8s --runtime python3.11 \
  --role arn:aws:iam::000000000000:role/x --handler app.handler \
  --environment "Variables={STRATUS_CALLBACK=http://$pod_ip:4566}" \
  --zip-file "fileb://$work/fn.zip" >/dev/null
aws lambda invoke --function-name k8s "$work/out.json" >/dev/null
grep -q '"callback": 200' "$work/out.json" || fail "lambda: $(cat "$work/out.json")"

echo "--- RDS: connect from the pod"
aws rds create-db-instance --db-instance-identifier orders --engine postgres \
  --db-instance-class db.t3.micro --master-username app --master-user-password secret123 \
  --allocated-storage 20 >/dev/null
read -r address port < <(aws rds describe-db-instances --db-instance-identifier orders \
  --query 'DBInstances[0].Endpoint.[Address,Port]' --output text)
[ "$address" = "$node_ip" ] || fail "rds endpoint is $address, want the node $node_ip"
kubectl exec stratus-runner -c psql -- env PGPASSWORD=secret123 \
  psql -h "$address" -p "$port" -U app -d postgres -tc "select 1" | grep -q 1 || fail "psql"

echo "--- MSK: produce and consume from the pod"
arn="$(aws kafka create-cluster-v2 --cluster-name events --provisioned \
  '{"BrokerNodeGroupInfo":{"InstanceType":"kafka.m5.large","ClientSubnets":["subnet-1","subnet-2"]},"KafkaVersion":"3.6.0","NumberOfBrokerNodes":2}' \
  --query ClusterArn --output text)"
brokers="$(aws kafka get-bootstrap-brokers --cluster-arn "$arn" --query BootstrapBrokerString --output text)"
case "$brokers" in "$node_ip":*) ;; *) fail "brokers are $brokers, want the node $node_ip" ;; esac
rpk() { kubectl exec -i stratus-runner -c rpk -- rpk "$@" -X brokers="$brokers"; }
rpk topic create orders >/dev/null
echo '{"order":1}' | rpk topic produce orders >/dev/null
rpk topic consume orders -n 1 | grep -q '"order\\":1' || fail "kafka consume"

echo "--- Snapshot and restore with engines running"
kubectl exec stratus-runner -c stratus -- env STRATUS_ENDPOINT=http://127.0.0.1:4566 stratus snapshot save k8s >/dev/null
kubectl exec stratus-runner -c stratus -- env STRATUS_ENDPOINT=http://127.0.0.1:4566 stratus snapshot restore k8s >/dev/null
read -r address port < <(aws rds describe-db-instances --db-instance-identifier orders \
  --query 'DBInstances[0].Endpoint.[Address,Port]' --output text)
kubectl exec stratus-runner -c psql -- env PGPASSWORD=secret123 \
  psql -h "$address" -p "$port" -U app -d postgres -tc "select 1" | grep -q 1 || fail "psql after restore"

echo "--- Delete the pod: Stratus must remove its containers"
kill "$forward_pid" 2>/dev/null || true
forward_pid=""
kubectl delete pod stratus-runner --wait=true --timeout=120s
left="$($node_docker docker ps -aq --filter label=io.stratus.managed=true | tr -d '\r')"
[ -z "$left" ] || { $node_docker docker ps -a --filter label=io.stratus.managed=true; fail "containers left behind"; }

echo "PASS: Stratus works in a Kubernetes pod on the node's Docker socket"
