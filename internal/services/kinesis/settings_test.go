package kinesis

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

func newFixture(t *testing.T) func(op, body string) (map[string]any, error) {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	svc := NewService(meta)
	return func(op, body string) (map[string]any, error) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		if err := svc.Handle(w, req, op); err != nil {
			return nil, err
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out, nil
	}
}

func apiCode(err error) string {
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func summary(t *testing.T, call func(string, string) (map[string]any, error)) map[string]any {
	t.Helper()
	out, err := call("DescribeStreamSummary", `{"StreamName":"orders"}`)
	if err != nil {
		t.Fatal(err)
	}
	return out["StreamDescriptionSummary"].(map[string]any)
}

func TestRetentionStartsAt24HoursAndMovesOnlyInTheDirectionAsked(t *testing.T) {
	call := newFixture(t)
	if _, err := call("CreateStream", `{"StreamName":"orders","ShardCount":1}`); err != nil {
		t.Fatal(err)
	}
	if got := summary(t, call)["RetentionPeriodHours"]; got != 24.0 {
		t.Fatalf("default retention = %v", got)
	}
	if _, err := call("IncreaseStreamRetentionPeriod", `{"StreamName":"orders","RetentionPeriodHours":168}`); err != nil {
		t.Fatal(err)
	}
	if got := summary(t, call)["RetentionPeriodHours"]; got != 168.0 {
		t.Fatalf("after increase = %v", got)
	}
	cases := map[string]string{
		"IncreaseStreamRetentionPeriod": `{"StreamName":"orders","RetentionPeriodHours":48}`,
		"DecreaseStreamRetentionPeriod": `{"StreamName":"orders","RetentionPeriodHours":200}`,
	}
	for op, body := range cases {
		if _, err := call(op, body); apiCode(err) != "InvalidArgumentException" {
			t.Errorf("%s %s: %v", op, body, err)
		}
	}
	if _, err := call("IncreaseStreamRetentionPeriod", `{"StreamName":"orders","RetentionPeriodHours":9000}`); apiCode(err) != "InvalidArgumentException" {
		t.Errorf("beyond 8760 hours: %v", err)
	}
	if _, err := call("DecreaseStreamRetentionPeriod", `{"StreamName":"orders","RetentionPeriodHours":24}`); err != nil {
		t.Fatal(err)
	}
}

func TestStreamTagsRoundTrip(t *testing.T) {
	call := newFixture(t)
	if _, err := call("CreateStream", `{"StreamName":"orders","ShardCount":1,"Tags":{"team":"payments"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("AddTagsToStream", `{"StreamName":"orders","Tags":{"env":"dev"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("RemoveTagsFromStream", `{"StreamName":"orders","TagKeys":["team"]}`); err != nil {
		t.Fatal(err)
	}
	out, err := call("ListTagsForStream", `{"StreamName":"orders"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out["Tags"])
	if string(raw) != `[{"Key":"env","Value":"dev"}]` {
		t.Errorf("tags = %s", raw)
	}
	if _, err := call("AddTagsToStream", `{"StreamName":"orders","Tags":{"aws:x":"y"}}`); apiCode(err) != "InvalidArgumentException" {
		t.Errorf("reserved prefix: %v", err)
	}
}

func TestEncryptionRecordsTheKeyAndCanBeStopped(t *testing.T) {
	call := newFixture(t)
	if _, err := call("CreateStream", `{"StreamName":"orders","ShardCount":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("StartStreamEncryption", `{"StreamName":"orders","EncryptionType":"KMS","KeyId":"alias/aws/kinesis"}`); err != nil {
		t.Fatal(err)
	}
	if s := summary(t, call); s["EncryptionType"] != "KMS" || s["KeyId"] != "alias/aws/kinesis" {
		t.Fatalf("after start: %v", s)
	}
	if _, err := call("StopStreamEncryption", `{"StreamName":"orders","EncryptionType":"KMS","KeyId":"alias/aws/kinesis"}`); err != nil {
		t.Fatal(err)
	}
	if s := summary(t, call); s["EncryptionType"] != "NONE" {
		t.Fatalf("after stop: %v", s)
	}
	if _, err := call("StartStreamEncryption", `{"StreamName":"orders","EncryptionType":"AES"}`); apiCode(err) != "InvalidArgumentException" {
		t.Errorf("bad type: %v", err)
	}
}
