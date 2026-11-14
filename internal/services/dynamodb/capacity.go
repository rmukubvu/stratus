package dynamodb

import (
	"context"
	"net/http"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/reqnote"
)

// CapacityGate decides whether tables have the read or write capacity a
// request needs. It is a port: the limits package adapts to it, and with
// no gate set every request has capacity.
type CapacityGate interface {
	// Consume draws capacity for demands that succeed or fail together.
	// It returns nil when all were admitted and consumed; otherwise it
	// consumes nothing and returns one entry per demand, nil for the
	// demands that would have fit.
	Consume(ctx context.Context, demands []CapacityDemand) []*CapacityThrottle
}

// CapacityDemand is what one item read or write costs a table.
type CapacityDemand struct {
	Account   string
	Region    string
	Operation string
	Table     string
	// OnDemand is true for PAY_PER_REQUEST tables; ReadCapacity and
	// WriteCapacity are a PROVISIONED table's units.
	OnDemand                    bool
	ReadCapacity, WriteCapacity int64
	// PartitionKey places the item on a partition; empty for a Scan.
	PartitionKey string
	Units        float64
	Write        bool
}

// CapacityThrottle says why a demand did not fit, with the reason DynamoDB
// reports in ThrottlingReasons.
type CapacityThrottle struct {
	Reason string
	// OnDemandMax is true when an on-demand table hit its maximum
	// throughput, which DynamoDB reports as ThrottlingException.
	OnDemandMax bool
}

// SetCapacity installs the capacity gate; nil turns capacity off.
func (s *Service) SetCapacity(g CapacityGate) { s.capacity = g }

// demand describes a read or write of units against table t.
func (s *Service) demand(r *http.Request, t tableRecord, partition string, units float64, write bool) CapacityDemand {
	d := CapacityDemand{
		Account: s.scope.Account, Region: s.scope.Region, Operation: operationOf(r), Table: t.TableName,
		OnDemand: t.BillingMode != "PROVISIONED" || t.Throughput == nil, PartitionKey: partition, Units: units, Write: write,
	}
	if !d.OnDemand {
		d.ReadCapacity, d.WriteCapacity = t.Throughput.ReadCapacityUnits, t.Throughput.WriteCapacityUnits
	}
	return d
}

// writeDemand describes the write of a prepared outcome.
func (s *Service) writeDemand(r *http.Request, o writeOutcome, units float64) CapacityDemand {
	return s.demand(r, o.table, itemPartition(o.table, o.key), units, true)
}

// itemPartition is the partition key value of an item or key.
func itemPartition(t tableRecord, key item) string { return canonicalKey(key[t.key().Hash]) }

// consume draws capacity for demands as one unit. It returns nil, or the
// throttles of a request that must fail. It also notes which partitions
// the request touched, so a load run can spot hot partitions whether or
// not capacity is enforced.
func (s *Service) consume(r *http.Request, demands ...CapacityDemand) []*CapacityThrottle {
	for _, d := range demands {
		if d.PartitionKey != "" {
			reqnote.AddKey(r.Context(), d.Table, d.PartitionKey)
		}
	}
	if s.capacity == nil || len(demands) == 0 {
		return nil
	}
	return s.capacity.Consume(r.Context(), demands)
}

// consumeOne draws capacity for a single-item request and returns the
// throttling error to send when it does not fit.
func (s *Service) consumeOne(r *http.Request, d CapacityDemand) error {
	if th := s.consume(r, d); th != nil && th[0] != nil {
		return throughputExceeded(s.tableARN(d.Table), th[0])
	}
	return nil
}

// consumeTransaction draws capacity for every item of a transaction at
// once. When any item does not fit, nothing is consumed and the
// transaction is cancelled with ThrottlingError for those items.
func (s *Service) consumeTransaction(r *http.Request, demands []CapacityDemand) error {
	throttles := s.consume(r, demands...)
	if throttles == nil {
		return nil
	}
	reasons := make([]cancellation, len(demands))
	for i, th := range throttles {
		reasons[i] = cancellation{Code: "None"}
		if th != nil {
			reasons[i] = cancellation{Code: "ThrottlingError", Message: "Throughput exceeds the current capacity of your table or index."}
		}
	}
	return transactionCanceled(reasons)
}

// throughputExceeded is the error DynamoDB returns when a table or
// partition is out of capacity, with ThrottlingReasons.
func throughputExceeded(tableARN string, th *CapacityThrottle) error {
	code := "ProvisionedThroughputExceededException"
	message := "The level of configured provisioned throughput for the table was exceeded. Consider increasing your provisioning level with the UpdateTable API."
	switch {
	case th.OnDemandMax:
		code = "ThrottlingException"
		message = "Throughput exceeds the maximum on-demand throughput configured for the table."
	case isKeyRange(th.Reason):
		message = "Throughput exceeds the current capacity for one or more partitions of the table. Spread requests across more partition key values."
	}
	return &throttleError{
		apiErr:  apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message},
		reasons: []throttlingReason{{Reason: th.Reason, Resource: tableARN}},
	}
}

func isKeyRange(reason string) bool {
	return reason == "TableReadKeyRangeThroughputExceeded" || reason == "TableWriteKeyRangeThroughputExceeded"
}

type throttlingReason struct {
	Reason   string `json:"reason"`
	Resource string `json:"resource"`
}

// throttleError carries ThrottlingReasons, which the shared error writer
// cannot express.
type throttleError struct {
	apiErr  apierror.Error
	reasons []throttlingReason
}

func (e *throttleError) Error() string { return e.apiErr.Error() }
func (e *throttleError) Unwrap() error { return &e.apiErr }
func (e *throttleError) WriteErrorResponse(w http.ResponseWriter) {
	writeJSON(w, e.apiErr.StatusCode, map[string]any{
		"__type": errorTypePrefix + e.apiErr.Code, "message": e.apiErr.Message, "ThrottlingReasons": e.reasons,
	})
}

// operationOf names the DynamoDB operation from the X-Amz-Target header.
func operationOf(r *http.Request) string {
	target := r.Header.Get("X-Amz-Target")
	for i := len(target) - 1; i >= 0; i-- {
		if target[i] == '.' {
			return target[i+1:]
		}
	}
	return target
}
