package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/diagnose"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

// recordingGate is the chaos gate every service delivers events through,
// decorated so a delivery that fails for real is logged for diagnosis.
// Event deliveries have no client waiting on them, so without this an
// event source mapping whose function throws, or whose runtime cannot
// start, fails without a trace.
//
// It also gives each delivery an ID when its source set none, so a chaos
// hit on the delivery can be matched to it.
type recordingGate struct {
	next  chaos.Gate
	log   *diagnose.DeliveryLog
	now   func() time.Time
	newID func() string
}

func newRecordingGate(next chaos.Gate, log *diagnose.DeliveryLog) recordingGate {
	return recordingGate{next: next, log: log, now: time.Now, newID: uuid.NewString}
}

func (g recordingGate) Deliver(ctx context.Context, d chaos.Delivery, deliver func(context.Context) error) error {
	if d.ID == "" {
		d.ID = g.newID()
	}
	return g.next.Deliver(ctx, d, func(ctx context.Context) error {
		err := deliver(ctx)
		if err != nil && ctx.Err() == nil {
			g.log.Record(deliveryFailure(d, err, g.now().UTC()))
		}
		return err
	})
}

// deliveryFailure describes a failed delivery the way diagnosis groups API
// failures: by receiving service, operation and error code.
func deliveryFailure(d chaos.Delivery, err error, at time.Time) diagnose.Failure {
	f := diagnose.Failure{
		Time: at, RequestID: d.ID, Service: d.Service, Operation: d.Operation, Method: "EVENT", Path: d.TargetARN,
		Code: "DeliveryFailed", Status: 500, Message: err.Error(),
		Delivery: &diagnose.Delivery{Source: d.Source, SourceARN: d.SourceARN, TargetARN: d.TargetARN},
	}
	var functionErr *lambdasvc.FunctionError
	var apiErr *apierror.Error
	switch {
	case errors.As(err, &functionErr):
		f.Code, f.Status = diagnose.FunctionErrorCode, 200
		f.Message = functionErr.Message
		if functionErr.Type != "" {
			f.Message = functionErr.Type + ": " + functionErr.Message
		}
	case errors.As(err, &apiErr):
		f.Code, f.Status, f.Message = apiErr.Code, apiErr.StatusCode, apiErr.Message
	}
	return f
}

// injectedDeliveryFailures turns chaos hits that failed an event delivery
// into failures, so diagnosis reports them beside the real ones. The hit's
// request ID is the delivery ID, which attributionsFrom maps to the rule.
func injectedDeliveryFailures(hits []chaos.Hit) []diagnose.Failure {
	var out []diagnose.Failure
	for _, h := range hits {
		if h.Scope != chaos.ScopeEvent || !failsDelivery(h.Action) {
			continue
		}
		out = append(out, diagnose.Failure{
			Time: h.Time, RequestID: h.RequestID, Service: h.Service, Operation: h.Operation, Method: "EVENT", Path: h.Path,
			Code: "InjectedFault", Message: "chaos rule " + h.RuleID + " injected " + string(h.Action) + " into the delivery",
			Delivery: &diagnose.Delivery{Source: h.Source, SourceARN: h.SourceARN, TargetARN: h.Path},
		})
	}
	return out
}

// failsDelivery reports whether a chaos action stops a delivery from
// having its effect; latency, cold starts and duplicates do not.
func failsDelivery(a chaos.Action) bool {
	switch a {
	case chaos.ActionLatency, chaos.ActionColdStart, chaos.ActionDuplicate:
		return false
	}
	return true
}
