// Package bench measures how quickly a test suite can get back to a known
// state between tests, using the isolation strategies Stratus supports.
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AWS is the small slice of the S3, DynamoDB and SQS APIs the benchmark
// drives. It speaks the wire protocols directly so the benchmark measures
// Stratus, not an SDK.
type AWS struct {
	Endpoint string
	HTTP     *http.Client
}

func (a AWS) do(ctx context.Context, method, path string, header http.Header, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (a AWS) jsonCall(ctx context.Context, target string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	h := http.Header{"X-Amz-Target": {target}, "Content-Type": {"application/x-amz-json-1.0"}}
	raw, err := a.do(ctx, http.MethodPost, "/", h, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// S3

func (a AWS) CreateBucket(ctx context.Context, bucket string) error {
	_, err := a.do(ctx, http.MethodPut, "/"+bucket, nil, nil)
	return err
}

func (a AWS) PutObject(ctx context.Context, bucket, key string, data []byte) error {
	_, err := a.do(ctx, http.MethodPut, "/"+bucket+"/"+key, nil, data)
	return err
}

func (a AWS) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := a.do(ctx, http.MethodDelete, "/"+bucket+"/"+key, nil, nil)
	return err
}

// CountObjects counts the keys in a bucket, following pagination.
func (a AWS) CountObjects(ctx context.Context, bucket string) (int, error) {
	total, token := 0, ""
	for {
		path := "/" + bucket + "?list-type=2"
		if token != "" {
			path += "&continuation-token=" + token
		}
		raw, err := a.do(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return 0, err
		}
		var page struct {
			KeyCount              int    `xml:"KeyCount"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("list %s: %w", bucket, err)
		}
		total += page.KeyCount
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return total, nil
		}
		token = page.NextContinuationToken
	}
}

// DynamoDB

func (a AWS) CreateTable(ctx context.Context, table string) error {
	return a.jsonCall(ctx, "DynamoDB_20120810.CreateTable", map[string]any{
		"TableName":            table,
		"AttributeDefinitions": []map[string]string{{"AttributeName": "pk", "AttributeType": "S"}},
		"KeySchema":            []map[string]string{{"AttributeName": "pk", "KeyType": "HASH"}},
		"BillingMode":          "PAY_PER_REQUEST",
	}, nil)
}

// PutItems writes items with keys prefix-0 .. prefix-(n-1), 25 per batch.
func (a AWS) PutItems(ctx context.Context, table, prefix string, n int) error {
	for start := 0; start < n; start += 25 {
		var reqs []map[string]any
		for i := start; i < n && i < start+25; i++ {
			reqs = append(reqs, map[string]any{"PutRequest": map[string]any{"Item": map[string]any{
				"pk":     map[string]string{"S": fmt.Sprintf("%s-%d", prefix, i)},
				"status": map[string]string{"S": "pending"},
				"total":  map[string]string{"N": fmt.Sprint(i * 7 % 1000)},
			}}})
		}
		var out struct {
			UnprocessedItems map[string][]any `json:"UnprocessedItems"`
		}
		if err := a.jsonCall(ctx, "DynamoDB_20120810.BatchWriteItem", map[string]any{"RequestItems": map[string]any{table: reqs}}, &out); err != nil {
			return err
		}
		if len(out.UnprocessedItems) > 0 {
			return fmt.Errorf("batch write to %s left unprocessed items", table)
		}
	}
	return nil
}

func (a AWS) CountItems(ctx context.Context, table string) (int, error) {
	total := 0
	var start any
	for {
		in := map[string]any{"TableName": table, "Select": "COUNT"}
		if start != nil {
			in["ExclusiveStartKey"] = start
		}
		var out struct {
			Count            int `json:"Count"`
			LastEvaluatedKey any `json:"LastEvaluatedKey"`
		}
		if err := a.jsonCall(ctx, "DynamoDB_20120810.Scan", in, &out); err != nil {
			return 0, err
		}
		total += out.Count
		if out.LastEvaluatedKey == nil {
			return total, nil
		}
		start = out.LastEvaluatedKey
	}
}

// SQS

func (a AWS) CreateQueue(ctx context.Context, name string) (string, error) {
	var out struct {
		QueueURL string `json:"QueueUrl"`
	}
	err := a.jsonCall(ctx, "AmazonSQS.CreateQueue", map[string]string{"QueueName": name}, &out)
	return out.QueueURL, err
}

func (a AWS) SendMessages(ctx context.Context, queueURL string, n int) error {
	for i := 0; i < n; i++ {
		in := map[string]string{"QueueUrl": queueURL, "MessageBody": fmt.Sprintf(`{"order":%d}`, i)}
		if err := a.jsonCall(ctx, "AmazonSQS.SendMessage", in, nil); err != nil {
			return err
		}
	}
	return nil
}

func (a AWS) CountMessages(ctx context.Context, queueURL string) (int, error) {
	var out struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := a.jsonCall(ctx, "AmazonSQS.GetQueueAttributes", map[string]any{
		"QueueUrl": queueURL, "AttributeNames": []string{"ApproximateNumberOfMessages"},
	}, &out); err != nil {
		return 0, err
	}
	var n int
	_, err := fmt.Sscan(out.Attributes["ApproximateNumberOfMessages"], &n)
	return n, err
}

func (a AWS) QueueURL(ctx context.Context, name string) (string, error) {
	var out struct {
		QueueURL string `json:"QueueUrl"`
	}
	err := a.jsonCall(ctx, "AmazonSQS.GetQueueUrl", map[string]string{"QueueName": name}, &out)
	return out.QueueURL, err
}

func (a AWS) CountBuckets(ctx context.Context) (int, error) {
	raw, err := a.do(ctx, http.MethodGet, "/", nil, nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Buckets []struct{} `xml:"Buckets>Bucket"`
	}
	if err := xml.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("list buckets: %w", err)
	}
	return len(out.Buckets), nil
}
