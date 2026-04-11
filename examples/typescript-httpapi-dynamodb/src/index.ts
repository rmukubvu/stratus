import type { APIGatewayProxyHandlerV2 } from "aws-lambda";

type ItemPayload = {
  id: string;
};

export const handler: APIGatewayProxyHandlerV2 = async (event) => {
  const tableName = process.env.TABLE_NAME;
  const stratusEndpoint = process.env.STRATUS_ENDPOINT;
  if (!tableName || !stratusEndpoint) {
    throw new Error("TABLE_NAME and STRATUS_ENDPOINT are required");
  }

  const payload = JSON.parse(event.body ?? "{}") as Partial<ItemPayload>;
  if (!payload.id) {
    return {
      statusCode: 400,
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ message: "id is required" }),
    };
  }

  const response = await fetch(stratusEndpoint, {
    method: "POST",
    headers: {
      "content-type": "application/x-amz-json-1.0",
      "x-amz-target": "DynamoDB_20120810.PutItem",
    },
    body: JSON.stringify({
      TableName: tableName,
      Item: {
        id: { S: payload.id },
        status: { S: "stored" },
      },
    }),
  });

  if (!response.ok) {
    throw new Error(`dynamodb put-item failed: ${response.status} ${await response.text()}`);
  }

  return {
    statusCode: 202,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ id: payload.id, status: "stored" }),
  };
};
