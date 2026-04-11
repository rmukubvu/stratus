# TypeScript HTTP API -> Lambda -> DynamoDB

This example shows a simple local Stratus flow:

- API Gateway HTTP API receives `POST /items`
- a TypeScript Lambda runs as `nodejs20.x`
- the Lambda writes the payload into DynamoDB

## Requirements

- `stratus` running locally
- Docker running locally
- Node.js 20+
- AWS CLI

## Start Stratus

```bash
stratus serve --port 4566 --data-dir ./data --log-format pretty
```

## Build the Lambda zip

```bash
cd examples/typescript-httpapi-dynamodb
npm install
npm run build
```

That produces `dist/function.zip`.

## Create the DynamoDB table

```bash
aws --endpoint-url http://127.0.0.1:4566 dynamodb create-table \
  --table-name http-items \
  --attribute-definitions AttributeName=id,AttributeType=S \
  --key-schema AttributeName=id,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST
```

## Create the Lambda function

The Lambda runs inside Docker, so it must reach Stratus via `host.docker.internal`.

```bash
aws --endpoint-url http://127.0.0.1:4566 lambda create-function \
  --function-name node-http-ddb \
  --runtime nodejs20.x \
  --role arn:aws:iam::000000000000:role/node-http-ddb-role \
  --handler index.handler \
  --zip-file fileb://dist/function.zip \
  --environment 'Variables={TABLE_NAME=http-items,STRATUS_ENDPOINT=http://host.docker.internal:4566}'
```

## Create the HTTP API

```bash
API_ID="$(aws --endpoint-url http://127.0.0.1:4566 apigatewayv2 create-api \
  --name node-http-api \
  --protocol-type HTTP \
  --query ApiId \
  --output text)"

INTEGRATION_ID="$(aws --endpoint-url http://127.0.0.1:4566 apigatewayv2 create-integration \
  --api-id "$API_ID" \
  --integration-type AWS_PROXY \
  --integration-uri arn:aws:lambda:us-east-1:000000000000:function:node-http-ddb \
  --payload-format-version 2.0 \
  --query IntegrationId \
  --output text)"

aws --endpoint-url http://127.0.0.1:4566 apigatewayv2 create-route \
  --api-id "$API_ID" \
  --route-key 'POST /items' \
  --target "integrations/$INTEGRATION_ID"

aws --endpoint-url http://127.0.0.1:4566 apigatewayv2 create-stage \
  --api-id "$API_ID" \
  --stage-name '$default' \
  --auto-deploy
```

## Send a request

```bash
curl -sS \
  -X POST \
  -H 'content-type: application/json' \
  -d '{"id":"item-123"}' \
  "http://127.0.0.1:4566/_aws/execute-api/$API_ID/items"
```

Expected response:

```json
{"id":"item-123","status":"stored"}
```

## Verify the DynamoDB write

```bash
aws --endpoint-url http://127.0.0.1:4566 dynamodb get-item \
  --table-name http-items \
  --key '{"id":{"S":"item-123"}}'
```

You should see the stored item with `status = stored`.
