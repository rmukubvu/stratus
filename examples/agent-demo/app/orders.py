"""A tiny order service. It has a bug for the agent to find."""

import boto3

TABLE_NAME = "orders"


def client():
    # boto3 reads AWS_ENDPOINT_URL, so the same code runs against AWS or Stratus.
    return boto3.client("dynamodb")


def place_order(ddb, order_id: str, total_cents: int) -> None:
    ddb.put_item(
        TableName=TABLE_NAME,
        Item={"order_id": {"S": order_id}, "total_cents": {"N": str(total_cents)}, "status": {"S": "placed"}},
    )


def count_orders(ddb) -> int:
    return ddb.scan(TableName=TABLE_NAME, Select="COUNT")["Count"]
