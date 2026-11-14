package lambda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
)

// countingInvoker records invocations and answers each with {}.
type countingInvoker struct {
	mu    sync.Mutex
	calls int
	done  chan struct{}
}

func (c *countingInvoker) Invoke(context.Context, FunctionSpec, []byte) (InvokeResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.done != nil {
		c.done <- struct{}{}
	}
	return InvokeResult{Payload: []byte(`{}`)}, nil
}
func (c *countingInvoker) CleanupFunction(context.Context, string) error { return nil }
func (c *countingInvoker) Close() error                                  { return nil }

func (c *countingInvoker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// gate throttles the first `throttle` acquisitions, recording what it saw.
type gate struct {
	mu       sync.Mutex
	throttle int
	seen     []Invocation
	released int
}

func (g *gate) Acquire(_ context.Context, inv Invocation) (func(), *ConcurrencyThrottle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, inv)
	if g.throttle > 0 {
		g.throttle--
		return func() {}, &ConcurrencyThrottle{Reason: "ReservedFunctionConcurrentInvocationLimitExceeded"}
	}
	return func() {
		g.mu.Lock()
		g.released++
		g.mu.Unlock()
	}, nil
}

func concurrencyService(t *testing.T, functions ...string) (*Service, *countingInvoker) {
	t.Helper()
	svc := newTestService(t)
	inv := &countingInvoker{}
	svc.invoker = inv
	for _, fn := range functions {
		provisionTestFunction(t, svc, fn)
	}
	return svc, inv
}

func call(t *testing.T, svc *Service, method, path, op, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	var reader *bytes.Buffer
	if body != "" {
		reader = bytes.NewBufferString(body)
	} else {
		reader = &bytes.Buffer{}
	}
	rec := httptest.NewRecorder()
	err := svc.Handle(rec, httptest.NewRequest(method, "http://localhost:4566"+path, reader), op)
	return rec, err
}

func errorCode(err error) string {
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func TestFunctionConcurrencyAPIs(t *testing.T) {
	svc, _ := concurrencyService(t, "orders", "audit")

	rec, err := call(t, svc, http.MethodPut, "/2017-10-31/functions/orders/concurrency", "PutFunctionConcurrency", `{"ReservedConcurrentExecutions":50}`)
	if err != nil || rec.Code != http.StatusOK {
		t.Fatalf("put: %d %v", rec.Code, err)
	}
	rec, _ = call(t, svc, http.MethodGet, "/2019-09-30/functions/orders/concurrency", "GetFunctionConcurrency", "")
	if got := rec.Body.String(); !bytes.Contains([]byte(got), []byte(`"ReservedConcurrentExecutions":50`)) {
		t.Fatalf("get: %s", got)
	}
	rec, _ = call(t, svc, http.MethodGet, "/2015-03-31/functions/orders", "GetFunction", "")
	var fn struct {
		Concurrency map[string]int `json:"Concurrency"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fn)
	if fn.Concurrency["ReservedConcurrentExecutions"] != 50 {
		t.Fatalf("GetFunction Concurrency: %s", rec.Body.String())
	}

	rec, _ = call(t, svc, http.MethodGet, "/2016-08-19/account-settings", "GetAccountSettings", "")
	var settings struct {
		AccountLimit map[string]int64 `json:"AccountLimit"`
		AccountUsage map[string]int64 `json:"AccountUsage"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &settings)
	if settings.AccountLimit["ConcurrentExecutions"] != 1000 || settings.AccountLimit["UnreservedConcurrentExecutions"] != 950 || settings.AccountUsage["FunctionCount"] != 2 {
		t.Fatalf("account settings: %s", rec.Body.String())
	}

	// Reserving 851 more would leave 99 unreserved, under the minimum of 100.
	if _, err := call(t, svc, http.MethodPut, "/2017-10-31/functions/audit/concurrency", "PutFunctionConcurrency", `{"ReservedConcurrentExecutions":851}`); errorCode(err) != "InvalidParameterValueException" {
		t.Fatalf("over-reservation: %v", err)
	}
	// Changing a reservation counts only the difference.
	if _, err := call(t, svc, http.MethodPut, "/2017-10-31/functions/orders/concurrency", "PutFunctionConcurrency", `{"ReservedConcurrentExecutions":900}`); err != nil {
		t.Fatalf("raising orders to 900: %v", err)
	}
	if _, err := call(t, svc, http.MethodPut, "/2017-10-31/functions/missing/concurrency", "PutFunctionConcurrency", `{"ReservedConcurrentExecutions":1}`); errorCode(err) != "ResourceNotFoundException" {
		t.Fatalf("missing function: %v", err)
	}

	rec, err = call(t, svc, http.MethodDelete, "/2017-10-31/functions/orders/concurrency", "DeleteFunctionConcurrency", "")
	if err != nil || rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", rec.Code, err)
	}
	rec, _ = call(t, svc, http.MethodGet, "/2019-09-30/functions/orders/concurrency", "GetFunctionConcurrency", "")
	if rec.Body.String() != "{}\n" {
		t.Fatalf("get after delete: %q", rec.Body.String())
	}
}

func TestDeletingAFunctionDropsItsReservation(t *testing.T) {
	svc, _ := concurrencyService(t, "orders")
	if err := svc.SetReservedConcurrency("orders", 10); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), "orders"); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := svc.reservations(""); total != 0 {
		t.Fatalf("reservation outlived its function: %d", total)
	}
}

func TestThrottledInvokeIsA429WithAReason(t *testing.T) {
	svc, inv := concurrencyService(t, "orders", "audit")
	_ = svc.SetReservedConcurrency("orders", 5)
	_ = svc.SetReservedConcurrency("audit", 3)
	g := &gate{throttle: 1}
	svc.SetConcurrency(g)

	_, err := call(t, svc, http.MethodPost, "/2015-03-31/functions/orders/invocations", "Invoke", `{}`)
	var throttled *throttledError
	if !errors.As(err, &throttled) || errorCode(err) != "TooManyRequestsException" {
		t.Fatalf("want a throttle, got %v", err)
	}
	rec := httptest.NewRecorder()
	throttled.WriteErrorResponse(rec)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusTooManyRequests || body["Reason"] != "ReservedFunctionConcurrentInvocationLimitExceeded" {
		t.Fatalf("429 body: %d %s", rec.Code, rec.Body.String())
	}
	if inv.count() != 0 {
		t.Fatal("a throttled invocation ran")
	}
	if seen := g.seen[0]; seen.Function != "orders" || seen.Reserved == nil || *seen.Reserved != 5 || seen.ReservedTotal != 8 {
		t.Fatalf("gate saw %+v", seen)
	}

	if _, err := call(t, svc, http.MethodPost, "/2015-03-31/functions/orders/invocations", "Invoke", `{}`); err != nil {
		t.Fatalf("second invoke: %v", err)
	}
	if inv.count() != 1 || g.released != 1 {
		t.Fatalf("calls %d, released %d", inv.count(), g.released)
	}
}

func TestAsyncInvokeWaitsOutThrottles(t *testing.T) {
	svc, inv := concurrencyService(t, "orders")
	inv.done = make(chan struct{}, 1)
	svc.SetConcurrency(&gate{throttle: 2})

	req := httptest.NewRequest(http.MethodPost, "http://localhost:4566/2015-03-31/functions/orders/invocations", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Amz-Invocation-Type", "Event")
	rec := httptest.NewRecorder()
	if err := svc.Handle(rec, req, "Invoke"); err != nil || rec.Code != http.StatusAccepted {
		t.Fatalf("event invoke: %d %v", rec.Code, err)
	}
	select {
	case <-inv.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a throttled Event invocation never ran")
	}
}
