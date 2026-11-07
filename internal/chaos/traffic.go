package chaos

import "strings"

// Request describes an incoming AWS API request.
type Request struct {
	RequestID string
	Service   string
	Operation string
	Path      string
	Bucket    string
	Function  string
}

// Delivery describes an event Stratus delivers internally, from a source
// service to a target, without an API request: an SQS message handed to a
// Lambda event source mapping, an SNS notification fanned out to a queue,
// an EventBridge event sent to a target, a Step Functions task invoking a
// function, or a Lambda destination record.
type Delivery struct {
	// ID correlates the delivery with the event it carries, such as a
	// message or event ID. It is optional.
	ID string
	// Source is the service making the delivery, e.g. "sqs" or "events".
	Source string
	// SourceARN is the queue, topic, stream, rule, state machine or
	// function the event comes from.
	SourceARN string
	// Service is the service receiving the delivery, e.g. "lambda".
	Service string
	// Operation is the target operation: Invoke, SendMessage, Publish or
	// StartExecution.
	Operation string
	// TargetARN is the function, queue, topic or state machine receiving
	// the delivery.
	TargetARN string
	// Function names the target Lambda function, if any, so cold start
	// faults know which warm container to discard.
	Function string
	// Async is true when, in AWS, the source does not wait for the target:
	// SNS fan-out, EventBridge targets, event source mappings and Lambda
	// destinations. A delayed async delivery arrives late (and may be
	// overtaken by later events), and a failed one is not delivered;
	// the source carries on either way. A synchronous delivery, such as a
	// Step Functions task, waits out delays and sees injected errors.
	Async bool
}

// traffic is the common shape the rules match against, for API requests
// and event deliveries alike.
type traffic struct {
	scope     Scope
	id        string
	service   string
	operation string
	source    string
	path      string
	sourceARN string
	function  string
	// resources are the strings a rule's Resource is a substring of.
	resources []string
}

func (t traffic) mentions(resource string) bool {
	for _, s := range t.resources {
		if strings.Contains(s, resource) {
			return true
		}
	}
	return false
}

func (r Request) traffic() traffic {
	return traffic{
		scope:     ScopeAPI,
		id:        r.RequestID,
		service:   r.Service,
		operation: r.Operation,
		path:      r.Path,
		function:  r.Function,
		resources: []string{r.Path, r.Bucket, r.Function},
	}
}

func (d Delivery) traffic() traffic {
	return traffic{
		scope:     ScopeEvent,
		id:        d.ID,
		service:   d.Service,
		operation: d.Operation,
		source:    d.Source,
		path:      d.TargetARN,
		sourceARN: d.SourceARN,
		function:  d.Function,
		resources: []string{d.SourceARN, d.TargetARN, d.Function},
	}
}
