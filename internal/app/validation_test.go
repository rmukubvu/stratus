package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestARequestThatBreaksTheAWSModelGetsAWSsValidationError(t *testing.T) {
	e := startEmulator(t)

	req, _ := http.NewRequest(http.MethodPost, e.base+"/", strings.NewReader(`{"KeySpec": "AES_512"}`))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261125/us-east-1/kms/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")
	req.Header.Set("X-Amz-Target", "TrentService.GenerateDataKey")
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusBadRequest || body.Type != "ValidationException" {
		t.Fatalf("got %d %+v", resp.StatusCode, body)
	}
	want := "2 validation errors detected: Value null at 'keyId' failed to satisfy constraint: Member must not be null; " +
		"Value 'AES_512' at 'keySpec' failed to satisfy constraint: Member must satisfy enum value set: [AES_256, AES_128]"
	if body.Message != want {
		t.Fatalf("message\n got %q\nwant %q", body.Message, want)
	}
}
