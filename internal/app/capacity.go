package app

import (
	"context"
	"strings"

	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/services/dynamodb"
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
