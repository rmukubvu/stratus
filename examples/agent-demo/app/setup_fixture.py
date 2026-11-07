"""Creates the tables the service needs, as a CI setup step would."""

import boto3


def main() -> None:
    ddb = boto3.client("dynamodb")
    ddb.create_table(
        TableName="Orders",
        AttributeDefinitions=[{"AttributeName": "order_id", "AttributeType": "S"}],
        KeySchema=[{"AttributeName": "order_id", "KeyType": "HASH"}],
        BillingMode="PAY_PER_REQUEST",
    )


if __name__ == "__main__":
    main()
