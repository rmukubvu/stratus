package chaos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Gate is the port a service uses to make an event delivery: it hands the
// gate a description of the delivery and a function that performs it, and
// the gate decides whether, when and how often that function runs.
type Gate interface {
	Deliver(ctx context.Context, d Delivery, deliver func(context.Context) error) error
}

// Passthrough is a Gate that injects nothing. Services use it until a real
// gate is wired in, so they never need a nil check.
type Passthrough struct{}

// Deliver runs deliver unchanged.
func (Passthrough) Deliver(ctx context.Context, _ Delivery, deliver func(context.Context) error) error {
	return deliver(ctx)
}

// FaultError is returned to a synchronous source when a chaos rule fails
// its delivery. It carries the AWS error the target would have returned.
type FaultError struct {
	Spec    ErrorSpec
	RuleIDs []string
}

func (e *FaultError) Error() string {
	return fmt.Sprintf("%s: %s (chaos rule %s)", e.Spec.Code, e.Spec.Message, strings.Join(e.RuleIDs, ","))
}

// FunctionErrorType is the errorType of an injected Lambda function error.
const FunctionErrorType = "InjectedFunctionError"

// DeliveryDecider decides which faults to inject into a delivery; *Engine
// fits.
type DeliveryDecider interface {
	DecideDelivery(d Delivery) Decision
}

// ColdStarter discards a Lambda function's warm container.
type ColdStarter func(ctx context.Context, function string) error

// Injector is the Gate backed by a chaos engine.
type Injector struct {
	decider   DeliveryDecider
	coldStart ColdStarter
	logger    *slog.Logger
	// sleep and spawn are seams for tests.
	sleep func(ctx context.Context, d time.Duration) error
	spawn func(func())
}

// NewInjector builds a Gate that applies the decider's faults. coldStart
// and logger may be nil.
func NewInjector(decider DeliveryDecider, coldStart ColdStarter, logger *slog.Logger) *Injector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Injector{
		decider:   decider,
		coldStart: coldStart,
		logger:    logger,
		sleep:     sleepContext,
		spawn:     func(f func()) { go f() },
	}
}

// Deliver applies the faults decided for d around deliver:
//
//   - latency waits before delivering; an async delivery waits in the
//     background, so the source is not held up and later events can
//     overtake it;
//   - a cold start discards the target function's warm container first;
//   - drop skips the delivery, and the source sees success;
//   - an error skips the delivery; a synchronous source gets a *FaultError,
//     an async source sees success, as it would in AWS;
//   - duplicate delivers twice.
func (i *Injector) Deliver(ctx context.Context, d Delivery, deliver func(context.Context) error) error {
	decision := i.decider.DecideDelivery(d)
	if !decision.Faulted() {
		return deliver(ctx)
	}
	i.logger.Info("chaos fault injected into event delivery",
		"rules", strings.Join(decision.RuleIDs, ","),
		"source", d.Source, "source_arn", d.SourceARN,
		"service", d.Service, "operation", d.Operation, "target_arn", d.TargetARN,
	)
	if d.Async && decision.Delay > 0 {
		// Detach from the caller's context: the source has moved on, and
		// the delivery must still happen once the delay is over.
		bg := context.WithoutCancel(ctx)
		i.spawn(func() {
			if err := i.apply(bg, d, decision, deliver); err != nil {
				i.logger.Warn("delayed event delivery failed", "target_arn", d.TargetARN, "error", err)
			}
		})
		return nil
	}
	err := i.apply(ctx, d, decision, deliver)
	var fault *FaultError
	if d.Async && errors.As(err, &fault) {
		return nil
	}
	return err
}

func (i *Injector) apply(ctx context.Context, d Delivery, decision Decision, deliver func(context.Context) error) error {
	if decision.Delay > 0 {
		if err := i.sleep(ctx, decision.Delay); err != nil {
			return err
		}
	}
	if decision.ColdStart != "" && i.coldStart != nil {
		if err := i.coldStart(ctx, decision.ColdStart); err != nil {
			i.logger.Warn("chaos cold start failed", "function", decision.ColdStart, "error", err)
		}
	}
	if decision.Drop {
		return nil
	}
	if decision.Error != nil {
		return &FaultError{Spec: *decision.Error, RuleIDs: decision.RuleIDs}
	}
	if decision.FunctionError != "" {
		return &FaultError{Spec: ErrorSpec{StatusCode: 200, Code: FunctionErrorType, Message: decision.FunctionError}, RuleIDs: decision.RuleIDs}
	}
	if err := deliver(ctx); err != nil {
		return err
	}
	if decision.Duplicate {
		return deliver(ctx)
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
