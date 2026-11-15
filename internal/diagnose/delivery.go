package diagnose

import (
	"sync"
	"time"
)

// Delivery describes an event Stratus delivered internally, from a source
// to a target, with no API request a client could see fail: an SQS message
// handed to a Lambda event source mapping, an SNS notification, an
// EventBridge or Scheduler target, a Lambda destination.
type Delivery struct {
	// Source is the service making the delivery, e.g. "sqs".
	Source    string
	SourceARN string
	TargetARN string
}

// DeliveryLog keeps the most recent failed event deliveries, newest first,
// so diagnosis can explain failures that happened out of the caller's
// sight. It is safe for concurrent use.
type DeliveryLog struct {
	mu       sync.Mutex
	capacity int
	failures []Failure
}

const defaultDeliveryLogSize = 200

// NewDeliveryLog keeps up to capacity failures; zero or less means 200.
func NewDeliveryLog(capacity int) *DeliveryLog {
	if capacity <= 0 {
		capacity = defaultDeliveryLogSize
	}
	return &DeliveryLog{capacity: capacity}
}

// Record adds a failed delivery. A failure without a time is stamped now.
func (l *DeliveryLog) Record(f Failure) {
	if f.Time.IsZero() {
		f.Time = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = append([]Failure{f}, l.failures...)
	if len(l.failures) > l.capacity {
		l.failures = l.failures[:l.capacity]
	}
}

// Failures returns the recorded failures, newest first.
func (l *DeliveryLog) Failures() []Failure {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Failure(nil), l.failures...)
}
