package lambda

import (
	"reflect"
	"testing"
)

func TestSettleSQSBatch(t *testing.T) {
	batch := []QueueMessage{
		{MessageID: "m1", ReceiptHandle: "h1"},
		{MessageID: "m2", ReceiptHandle: "h2"},
		{MessageID: "m3", ReceiptHandle: "h3"},
	}
	all := []string{"h1", "h2", "h3"}

	cases := []struct {
		name    string
		payload string
		report  bool
		want    []string
		wantErr bool
	}{
		{name: "without partial responses success settles every message", payload: `{"batchItemFailures":[{"itemIdentifier":"m1"}]}`, want: all},
		{name: "no response", payload: ``, report: true, want: all},
		{name: "null response", payload: `null`, report: true, want: all},
		{name: "empty object", payload: `{}`, report: true, want: all},
		{name: "null list", payload: `{"batchItemFailures":null}`, report: true, want: all},
		{name: "empty list", payload: `{"batchItemFailures":[]}`, report: true, want: all},
		{name: "failed items stay on the queue", payload: `{"batchItemFailures":[{"itemIdentifier":"m1"},{"itemIdentifier":"m3"}]}`, report: true, want: []string{"h2"}},
		{name: "every item failed", payload: `{"batchItemFailures":[{"itemIdentifier":"m1"},{"itemIdentifier":"m2"},{"itemIdentifier":"m3"}]}`, report: true, want: []string{}},
		{name: "response is not an object", payload: `"oops"`, report: true, wantErr: true},
		{name: "unknown message id", payload: `{"batchItemFailures":[{"itemIdentifier":"nope"}]}`, report: true, wantErr: true},
		{name: "empty identifier", payload: `{"batchItemFailures":[{"itemIdentifier":""}]}`, report: true, wantErr: true},
		{name: "null identifier", payload: `{"batchItemFailures":[{"itemIdentifier":null}]}`, report: true, wantErr: true},
		{name: "misspelt key", payload: `{"batchItemFailures":[{"itemId":"m1"}]}`, report: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := settleSQSBatch(batch, []byte(tc.payload), tc.report)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected the whole batch to fail, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("settle: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("deleted %v, want %v", got, tc.want)
			}
		})
	}
}
