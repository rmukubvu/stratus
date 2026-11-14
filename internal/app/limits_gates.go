package app

import (
	"context"
	"strings"

	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/loadrun"
	"github.com/stratus/internal/services/dynamodb"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

// capacityGate adapts the limits governor to DynamoDB's capacity port.
type capacityGate struct{ governor *limits.Governor }

func (g capacityGate) Consume(ctx context.Context, demands []dynamodb.CapacityDemand) []*dynamodb.CapacityThrottle {
	if !g.governor.Enabled() {
		return nil
	}
	requestID := httpapi.MetadataFromContext(ctx).RequestID
	reqs := make([]limits.CapacityRequest, len(demands))
	for i, d := range demands {
		reqs[i] = limits.CapacityRequest{
			Request: limits.Request{
				RequestID: requestID, Account: d.Account, Region: d.Region,
				Service: "dynamodb", Operation: d.Operation, Resource: d.Table,
			},
			OnDemand:     d.OnDemand,
			ReadCapacity: float64(d.ReadCapacity), WriteCapacity: float64(d.WriteCapacity),
			PartitionKey: d.PartitionKey, Units: d.Units, Write: d.Write,
		}
	}
	breaches := g.governor.ConsumeCapacity(reqs)
	if breaches == nil {
		return nil
	}
	out := make([]*dynamodb.CapacityThrottle, len(breaches))
	for i, b := range breaches {
		if b != nil {
			out[i] = &dynamodb.CapacityThrottle{Reason: b.Reason, OnDemandMax: strings.Contains(b.Reason, "MaxOnDemand")}
		}
	}
	return out
}

// concurrencyGate adapts the limits governor to Lambda's concurrency port.
type concurrencyGate struct{ governor *limits.Governor }

func (g concurrencyGate) Acquire(ctx context.Context, inv lambdasvc.Invocation) (func(), *lambdasvc.ConcurrencyThrottle) {
	if !g.governor.Enabled() {
		return func() {}, nil
	}
	release, breach := g.governor.AcquireFunction(limits.FunctionRequest{
		Request: limits.Request{
			RequestID: httpapi.MetadataFromContext(ctx).RequestID, Account: inv.Account, Region: inv.Region,
			Service: "lambda", Operation: "Invoke", Resource: inv.Function,
		},
		Reserved: inv.Reserved, ReservedTotal: inv.ReservedTotal,
	})
	if breach != nil {
		return release, &lambdasvc.ConcurrencyThrottle{Reason: breach.Reason}
	}
	return release, nil
}

// runObserver adapts load runs to the HTTP server's observer port.
type runObserver struct{ runs *loadrun.Recorder }

func (o runObserver) Observe(r httpapi.RequestOutcome) {
	o.runs.Observe(loadrun.Observation{
		Time: r.Time, Service: r.Service, Operation: r.Operation, Status: r.Status, Code: r.Code,
		Duration: r.Duration, Fault: r.Fault, LimitRule: r.LimitRule,
	})
}
