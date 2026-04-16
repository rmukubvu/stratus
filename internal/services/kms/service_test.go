package kms

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/store/bbolt"
)

func TestGetKeyRotationStatusDefaultsToDisabled(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(metadata)
	keyID, _, err := svc.CreateKey(CreateKeyInput{Description: "fixture"})
	if err != nil {
		t.Fatalf("CreateKey failed: %v", err)
	}

	req := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewBufferString(`{"KeyId":"`+keyID+`"}`))
	rec := httptest.NewRecorder()
	if err := svc.Handle(rec, req, "GetKeyRotationStatus"); err != nil {
		t.Fatalf("GetKeyRotationStatus failed: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"KeyRotationEnabled":false`) {
		t.Fatalf("unexpected GetKeyRotationStatus response: %s", body)
	}
}

func TestListResourceTagsDefaultsToEmpty(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(metadata)
	keyID, _, err := svc.CreateKey(CreateKeyInput{Description: "fixture"})
	if err != nil {
		t.Fatalf("CreateKey failed: %v", err)
	}

	req := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewBufferString(`{"KeyId":"`+keyID+`"}`))
	rec := httptest.NewRecorder()
	if err := svc.Handle(rec, req, "ListResourceTags"); err != nil {
		t.Fatalf("ListResourceTags failed: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"Tags":[]`) {
		t.Fatalf("unexpected ListResourceTags response: %s", body)
	}
}
