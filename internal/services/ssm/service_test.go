package ssm

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/store/bbolt"
)

func TestListTagsForResourceReturnsEmptyTagList(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(metadata)
	if err := svc.PutParameter(PutParameterInput{
		Name:  "/stratus/fixture",
		Type:  "String",
		Value: "value",
	}); err != nil {
		t.Fatalf("PutParameter failed: %v", err)
	}

	req := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewBufferString(`{"ResourceType":"Parameter","ResourceId":"/stratus/fixture"}`))
	rec := httptest.NewRecorder()
	if err := svc.Handle(rec, req, "ListTagsForResource"); err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"TagList":[]`) {
		t.Fatalf("unexpected ListTagsForResource response: %s", body)
	}
}
