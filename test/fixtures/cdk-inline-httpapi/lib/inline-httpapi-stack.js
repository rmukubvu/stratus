const cdk = require("aws-cdk-lib");
const iam = require("aws-cdk-lib/aws-iam");
const lambda = require("aws-cdk-lib/aws-lambda");
const apigwv2 = require("aws-cdk-lib/aws-apigatewayv2");

class InlineHttpApiStack extends cdk.Stack {
  constructor(scope, id, props) {
    super(scope, id, {
      ...props,
      synthesizer: new cdk.BootstraplessSynthesizer(),
    });

    const role = new iam.CfnRole(this, "InlineFunctionRole", {
      assumeRolePolicyDocument: {
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Principal: {
              Service: "lambda.amazonaws.com",
            },
            Action: "sts:AssumeRole",
          },
        ],
      },
      policies: [
        {
          policyName: "inline-logs",
          policyDocument: {
            Version: "2012-10-17",
            Statement: [
              {
                Effect: "Allow",
                Action: ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"],
                Resource: "*",
              },
            ],
          },
        },
      ],
      roleName: "stratus-inline-httpapi-role",
    });

    const fn = new lambda.CfnFunction(this, "InlineFunction", {
      code: {
        zipFile: [
          "def main(event, context):",
          "    return {",
          "        'statusCode': 200,",
          "        'headers': {'content-type': 'application/json'},",
          "        'body': '{\"ok\": true, \"source\": \"cdk\"}'",
          "    }",
        ].join("\n"),
      },
      functionName: "stratus-inline-httpapi",
      handler: "index.main",
      role: role.attrArn,
      runtime: "python3.11",
      timeout: 10,
    });

    const api = new apigwv2.CfnApi(this, "HttpApi", {
      name: "stratus-inline-httpapi",
      protocolType: "HTTP",
    });

    const integration = new apigwv2.CfnIntegration(this, "LambdaIntegration", {
      apiId: api.ref,
      integrationType: "AWS_PROXY",
      integrationUri: fn.attrArn,
      payloadFormatVersion: "2.0",
    });

    new apigwv2.CfnRoute(this, "HelloRoute", {
      apiId: api.ref,
      routeKey: "GET /hello",
      target: cdk.Fn.join("", ["integrations/", integration.ref]),
    });

    new apigwv2.CfnStage(this, "DefaultStage", {
      apiId: api.ref,
      autoDeploy: true,
      stageName: "$default",
    });

    new lambda.CfnPermission(this, "InvokePermission", {
      action: "lambda:InvokeFunction",
      functionName: fn.ref,
      principal: "apigateway.amazonaws.com",
    });

    new cdk.CfnOutput(this, "ApiEndpoint", {
      value: api.attrApiEndpoint,
    });
  }
}

module.exports = { InlineHttpApiStack };
