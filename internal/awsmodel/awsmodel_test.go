package awsmodel

import (
	"slices"
	"testing"
)

func TestTheCatalogueCoversEveryEmulatedService(t *testing.T) {
	services := Services()
	for _, want := range []string{"s3", "sqs", "dynamodb", "lambda", "ec2", "ses", "bedrock", "monitoring"} {
		if !slices.Contains(services, want) {
			t.Errorf("Services() = %v, missing %q", services, want)
		}
	}
	for _, name := range services {
		svc, err := Load(name)
		if err != nil {
			t.Fatalf("Load(%q): %v", name, err)
		}
		if len(svc.OperationNames()) == 0 {
			t.Errorf("%s has no operations", name)
		}
	}
}

func TestAnOperationCarriesItsInputConstraints(t *testing.T) {
	svc, err := Load("dynamodb")
	if err != nil {
		t.Fatal(err)
	}
	api, op, ok := svc.Operation("CreateTable")
	if !ok {
		t.Fatal("CreateTable not found")
	}
	if api.Protocol != "json" || api.TargetPrefix != "DynamoDB_20120810" {
		t.Errorf("protocol %q target %q", api.Protocol, api.TargetPrefix)
	}
	input := api.Shape(op.Input)
	if input == nil || !slices.Contains(input.Required, "TableName") {
		t.Fatalf("CreateTableInput = %+v, want TableName required", input)
	}
	name := api.Shape(input.Members["TableName"].Shape)
	if name == nil || name.Min == nil || *name.Min != 1 {
		t.Errorf("TableName shape = %+v, want min 1", name)
	}
}

func TestSESJoinsTheV1AndV2Models(t *testing.T) {
	svc, err := Load("ses")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := svc.Operation("SendRawEmail"); !ok {
		t.Error("v1 SendRawEmail missing")
	}
	api, _, ok := svc.Operation("CreateEmailIdentity")
	if !ok || api.Model != "sesv2" {
		t.Errorf("v2 CreateEmailIdentity: ok=%v api=%+v", ok, api)
	}
}

func TestAnUnknownServiceIsAnError(t *testing.T) {
	if _, err := Load("nope"); err == nil {
		t.Fatal("Load(nope) succeeded")
	}
}
