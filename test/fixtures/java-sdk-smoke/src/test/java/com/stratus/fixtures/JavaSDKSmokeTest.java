package com.stratus.fixtures;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.http.urlconnection.UrlConnectionHttpClient;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.core.ResponseBytes;
import software.amazon.awssdk.core.sync.RequestBody;
import software.amazon.awssdk.services.dynamodb.DynamoDbClient;
import software.amazon.awssdk.services.dynamodb.model.AttributeDefinition;
import software.amazon.awssdk.services.dynamodb.model.AttributeValue;
import software.amazon.awssdk.services.dynamodb.model.BillingMode;
import software.amazon.awssdk.services.dynamodb.model.GetItemResponse;
import software.amazon.awssdk.services.dynamodb.model.KeySchemaElement;
import software.amazon.awssdk.services.dynamodb.model.KeyType;
import software.amazon.awssdk.services.dynamodb.model.ScalarAttributeType;
import software.amazon.awssdk.services.s3.S3Client;
import software.amazon.awssdk.services.s3.S3Configuration;
import software.amazon.awssdk.services.s3.model.GetObjectRequest;
import software.amazon.awssdk.services.s3.model.GetObjectResponse;
import software.amazon.awssdk.services.sqs.SqsClient;
import software.amazon.awssdk.services.sqs.model.Message;
import software.amazon.awssdk.services.sts.StsClient;
import software.amazon.awssdk.services.sts.model.GetCallerIdentityResponse;

public class JavaSDKSmokeTest {
    private static final Region REGION = Region.US_EAST_1;
    private static final StaticCredentialsProvider CREDS =
        StaticCredentialsProvider.create(AwsBasicCredentials.create("test", "test"));

    @Test
    void awsSdkV2CanCallStratus() {
        String endpoint = System.getProperty("stratus.endpoint", "http://127.0.0.1:4566");
        String tableName = "java-sdk-" + UUID.randomUUID().toString().replace("-", "").substring(0, 12);
        String itemId = "item-" + UUID.randomUUID().toString().replace("-", "").substring(0, 8);
        String expected = "hello from java sdk";
        String queueName = "java-sdk-" + UUID.randomUUID().toString().replace("-", "").substring(0, 12);
        String bucketName = "java-sdk-" + UUID.randomUUID().toString().replace("-", "").substring(0, 16);
        String objectKey = "fixtures/hello.txt";
        String objectBody = "hello from java sdk s3";

        try (StsClient sts = StsClient.builder()
                .region(REGION)
                .credentialsProvider(CREDS)
                .endpointOverride(URI.create(endpoint))
                .httpClientBuilder(UrlConnectionHttpClient.builder())
                .build();
             DynamoDbClient dynamo = DynamoDbClient.builder()
                .region(REGION)
                .credentialsProvider(CREDS)
                .endpointOverride(URI.create(endpoint))
                .httpClientBuilder(UrlConnectionHttpClient.builder())
                .build();
             SqsClient sqs = SqsClient.builder()
                .region(REGION)
                .credentialsProvider(CREDS)
                .endpointOverride(URI.create(endpoint))
                .httpClientBuilder(UrlConnectionHttpClient.builder())
                .build();
             S3Client s3 = S3Client.builder()
                .region(REGION)
                .credentialsProvider(CREDS)
                .endpointOverride(URI.create(endpoint))
                .httpClientBuilder(UrlConnectionHttpClient.builder())
                .serviceConfiguration(S3Configuration.builder().pathStyleAccessEnabled(true).build())
                .build()) {

            GetCallerIdentityResponse identity = sts.getCallerIdentity();
            assertEquals("000000000000", identity.account());
            assertTrue(identity.arn().contains("arn:aws:iam::000000000000:"));

            dynamo.createTable(builder -> builder
                .tableName(tableName)
                .billingMode(BillingMode.PAY_PER_REQUEST)
                .attributeDefinitions(
                    AttributeDefinition.builder().attributeName("id").attributeType(ScalarAttributeType.S).build()
                )
                .keySchema(
                    KeySchemaElement.builder().attributeName("id").keyType(KeyType.HASH).build()
                )
            );

            dynamo.putItem(builder -> builder
                .tableName(tableName)
                .item(
                    java.util.Map.of(
                        "id", AttributeValue.builder().s(itemId).build(),
                        "message", AttributeValue.builder().s(expected).build()
                    )
                )
            );

            GetItemResponse item = dynamo.getItem(builder -> builder
                .tableName(tableName)
                .key(java.util.Map.of("id", AttributeValue.builder().s(itemId).build()))
            );
            assertEquals(itemId, item.item().get("id").s());
            assertEquals(expected, item.item().get("message").s());

            String queueUrl = sqs.createQueue(builder -> builder.queueName(queueName)).queueUrl();
            sqs.sendMessage(builder -> builder.queueUrl(queueUrl).messageBody(expected));
            Message message = sqs.receiveMessage(builder -> builder.queueUrl(queueUrl).maxNumberOfMessages(1))
                .messages()
                .stream()
                .findFirst()
                .orElseThrow();
            assertEquals(expected, message.body());
            assertFalse(message.receiptHandle().isBlank());
            sqs.deleteMessage(builder -> builder.queueUrl(queueUrl).receiptHandle(message.receiptHandle()));
            sqs.deleteQueue(builder -> builder.queueUrl(queueUrl));

            s3.createBucket(builder -> builder.bucket(bucketName));
            s3.putObject(
                builder -> builder.bucket(bucketName).key(objectKey).contentType("text/plain"),
                RequestBody.fromString(objectBody, StandardCharsets.UTF_8)
            );
            ResponseBytes<GetObjectResponse> object = s3.getObjectAsBytes(
                GetObjectRequest.builder().bucket(bucketName).key(objectKey).build()
            );
            assertEquals(objectBody, object.asUtf8String());
        }
    }
}
