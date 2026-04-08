const cdk = require("aws-cdk-lib");
const { InlineHttpApiStack } = require("../lib/inline-httpapi-stack");

const app = new cdk.App();

new InlineHttpApiStack(app, "StratusInlineHttpApi", {
  env: {
    account: process.env.CDK_DEFAULT_ACCOUNT || "000000000000",
    region: process.env.CDK_DEFAULT_REGION || "us-east-1",
  },
});

app.synth();
