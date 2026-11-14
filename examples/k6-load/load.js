// k6 load test that calls DynamoDB on Stratus directly, the way an app's
// AWS SDK would. Half the writes go to one partition key, so a limits
// profile shows the hot partition throttling while the rest succeed.
//
//   stratus load run --name orders --limits strict -- k6 run load.js
import http from "k6/http";
import { check } from "k6";

const endpoint = __ENV.STRATUS_ENDPOINT || "http://localhost:4566";
const table = __ENV.TABLE || "orders";

export const options = {
  scenarios: {
    writes: { executor: "constant-arrival-rate", rate: 200, timeUnit: "1s", duration: "30s", preAllocatedVUs: 50 },
  },
};

function dynamodb(target, body) {
  return http.post(endpoint, JSON.stringify(body), {
    headers: {
      "Content-Type": "application/x-amz-json-1.0",
      "X-Amz-Target": `DynamoDB_20120810.${target}`,
      // Stratus accepts any credentials unless IAM enforcement is on.
      Authorization: "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/dynamodb/aws4_request, SignedHeaders=host, Signature=0",
    },
    tags: { operation: target },
  });
}

export function setup() {
  dynamodb("CreateTable", {
    TableName: table,
    BillingMode: "PROVISIONED",
    ProvisionedThroughput: { ReadCapacityUnits: 50, WriteCapacityUnits: 50 },
    AttributeDefinitions: [{ AttributeName: "pk", AttributeType: "S" }],
    KeySchema: [{ AttributeName: "pk", KeyType: "HASH" }],
  });
}

export default function () {
  const pk = Math.random() < 0.5 ? "customer#hot" : `customer#${Math.floor(Math.random() * 10000)}`;
  const res = dynamodb("PutItem", { TableName: table, Item: { pk: { S: pk }, at: { N: String(Date.now()) } } });
  // A throttle is an expected answer under load. With chaos.json, some
  // writes also fail on purpose; the run summary counts those as faults.
  check(res, { "ok or throttled": (r) => r.status === 200 || (r.status === 400 && r.body.includes("ProvisionedThroughputExceeded")) });
}
