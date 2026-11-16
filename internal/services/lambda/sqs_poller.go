package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/stratus/internal/chaos"
)

// How SQS event source mappings poll, mirroring AWS: an idle mapping keeps
// one poller, and a mapping with a backlog scales up to five concurrent
// batches unless ScalingConfig.MaximumConcurrency says otherwise.
const (
	sqsPollInterval         = 200 * time.Millisecond
	sqsBatchingStep         = 100 * time.Millisecond
	defaultSQSPollers       = 5
	defaultSQSBatchSize     = 10
	maxSQSBatchSize         = 10000
	maxSQSUnwindowedBatch   = 10
	maxSQSBatchingWindow    = 300
	minSQSMaximumConcurrent = 2
	maxSQSMaximumConcurrent = 1000
)

// errDeliveryDropped marks a batch a chaos rule dropped before it reached
// the function. Nothing is deleted, so the messages come back once their
// visibility timeout ends, as they would after a lost invocation in AWS.
var errDeliveryDropped = errors.New("event delivery was dropped before reaching the function")

// QueueMessage is one message an event source mapping received from a
// queue. Receiving it starts its visibility timeout.
type QueueMessage struct {
	MessageID     string
	ReceiptHandle string
	Body          string
	MD5OfBody     string
	Attributes    map[string]string
}

// QueueConsumer is the queue side of an SQS event source mapping.
// ReceiveFromARN receives up to max visible messages with the queue's own
// visibility timeout, redriving any message past its maxReceiveCount to the
// dead-letter queue; DeleteFromARN deletes received messages.
type QueueConsumer interface {
	ReceiveFromARN(queueARN string, max int) ([]QueueMessage, error)
	DeleteFromARN(queueARN string, receiptHandles []string) error
}

// SetQueueConsumer connects SQS event source mappings to the queues they
// poll.
func (s *Service) SetQueueConsumer(consumer QueueConsumer) {
	s.queueConsumer = consumer
}

// sqsPoller is the background state of the SQS event source mappings: the
// ticker loop, and how many workers each mapping has running.
type sqsPoller struct {
	interval time.Duration
	wake     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	started  bool
	stopOnce sync.Once
	workers  sync.WaitGroup

	mu     sync.Mutex
	active map[string]int
}

func newSQSPoller() *sqsPoller {
	ctx, cancel := context.WithCancel(context.Background())
	return &sqsPoller{
		interval: sqsPollInterval,
		wake:     make(chan struct{}, 1),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		active:   map[string]int{},
	}
}

// acquire reserves a worker slot for a mapping, if it has fewer than limit
// workers running and the poller has not stopped.
func (p *sqsPoller) acquire(mappingID string, limit int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil || p.active[mappingID] >= limit {
		return false
	}
	p.active[mappingID]++
	p.workers.Add(1)
	return true
}

func (p *sqsPoller) release(mappingID string) {
	p.mu.Lock()
	p.active[mappingID]--
	if p.active[mappingID] <= 0 {
		delete(p.active, mappingID)
	}
	p.mu.Unlock()
	p.workers.Done()
}

// StartPolling starts polling the queues of SQS event source mappings.
// Without it, messages wait in their queues.
func (s *Service) StartPolling() {
	p := s.poller
	p.started = true
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			s.pollQueues()
			select {
			case <-p.ctx.Done():
				return
			case <-ticker.C:
			case <-p.wake:
			}
		}
	}()
}

// Close stops the pollers, cancelling batches in flight, and waits for
// them. It is safe without StartPolling, and safe to call twice.
func (s *Service) Close() error {
	p := s.poller
	p.stopOnce.Do(func() {
		p.cancel()
		if p.started {
			<-p.done
		}
		p.workers.Wait()
	})
	return nil
}

// QueueMessagesAvailable tells the pollers that a queue has new messages,
// so a mapping picks them up without waiting for the next tick.
func (s *Service) QueueMessagesAvailable(string) {
	select {
	case s.poller.wake <- struct{}{}:
	default:
	}
}

// pollQueues gives every enabled SQS mapping that has no worker running
// one worker. Workers scale themselves up while there is a backlog.
func (s *Service) pollQueues() {
	if s.queueConsumer == nil {
		return
	}
	mappings, err := s.listEventSourceMappingRecords("", "")
	if err != nil {
		return
	}
	for _, mapping := range mappings {
		if !mapping.Enabled || !isSQSQueueARN(mapping.EventSourceArn) {
			continue
		}
		if s.poller.acquire(mapping.UUID, 1) {
			go s.runSQSWorker(mapping.UUID)
		}
	}
}

// runSQSWorker processes batches for one mapping until its queue is empty,
// the mapping is disabled or deleted, or the poller stops. Each batch that
// finds messages adds a worker, up to the mapping's concurrency.
func (s *Service) runSQSWorker(mappingID string) {
	defer s.poller.release(mappingID)
	for s.poller.ctx.Err() == nil {
		mapping, err := s.loadEventSourceMapping(mappingID)
		if err != nil || !mapping.Enabled {
			return
		}
		if s.pollSQSBatch(s.poller.ctx, mapping) == 0 {
			return
		}
		if s.poller.acquire(mappingID, mapping.pollers()) {
			go s.runSQSWorker(mappingID)
		}
	}
}

// pollSQSBatch receives one batch, invokes the function with it and deletes
// the messages the invocation settled. Messages it does not delete become
// visible again when their visibility timeout ends, so they are retried and,
// past the queue's maxReceiveCount, redriven to its dead-letter queue. It
// returns how many messages it received.
func (s *Service) pollSQSBatch(ctx context.Context, mapping eventSourceMappingRecord) int {
	batch := s.receiveSQSBatch(ctx, mapping)
	if len(batch) == 0 {
		return 0
	}
	handles, err := s.deliverSQSBatch(ctx, mapping, batch)
	if err == nil && len(handles) > 0 {
		_ = s.queueConsumer.DeleteFromARN(mapping.EventSourceArn, handles)
	}
	return len(batch)
}

// receiveSQSBatch receives up to BatchSize messages. With a batching
// window, it keeps gathering until the batch is full or the window, which
// opens at the first message, closes.
func (s *Service) receiveSQSBatch(ctx context.Context, mapping eventSourceMappingRecord) []QueueMessage {
	batch, err := s.queueConsumer.ReceiveFromARN(mapping.EventSourceArn, mapping.BatchSize)
	if err != nil || len(batch) == 0 || mapping.MaximumBatchingWindowInSeconds <= 0 {
		return batch
	}
	deadline := time.Now().Add(time.Duration(mapping.MaximumBatchingWindowInSeconds) * time.Second)
	for len(batch) < mapping.BatchSize && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return batch
		case <-time.After(sqsBatchingStep):
		}
		more, err := s.queueConsumer.ReceiveFromARN(mapping.EventSourceArn, mapping.BatchSize-len(batch))
		if err != nil {
			break
		}
		batch = append(batch, more...)
	}
	return batch
}

// deliverSQSBatch invokes the function with a batch through the chaos gate
// and returns the receipt handles of the messages to delete. An error means
// the whole batch goes back to the queue: the function threw or timed out,
// was throttled, returned an invalid partial batch response, or a chaos
// rule failed or dropped the delivery.
func (s *Service) deliverSQSBatch(ctx context.Context, mapping eventSourceMappingRecord, batch []QueueMessage) ([]string, error) {
	body, err := json.Marshal(s.sqsEvent(mapping.EventSourceArn, batch))
	if err != nil {
		return nil, err
	}
	delivery := chaos.Delivery{
		Source:    "sqs",
		SourceARN: mapping.EventSourceArn,
		Service:   "lambda",
		Operation: "Invoke",
		TargetARN: s.lambdaARN(mapping.FunctionName),
		Function:  mapping.FunctionName,
	}
	var (
		handles   []string
		delivered bool
	)
	err = s.faults.Deliver(ctx, delivery, func(ctx context.Context) error {
		delivered = true
		record, err := s.resolveRecord(mapping.FunctionName, "")
		if err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(record.Timeout+5)*time.Second)
		defer cancel()
		result, err := s.execute(callCtx, record, body)
		if err != nil {
			return err
		}
		if err := result.Failure(mapping.FunctionName); err != nil {
			return err
		}
		handles, err = settleSQSBatch(batch, result.Payload, mapping.reportsItemFailures())
		return err
	})
	if err != nil {
		return nil, err
	}
	if !delivered {
		return nil, errDeliveryDropped
	}
	return handles, nil
}

// sqsEvent builds the event Lambda passes a function for a batch of
// messages.
func (s *Service) sqsEvent(queueARN string, batch []QueueMessage) map[string]any {
	records := make([]map[string]any, 0, len(batch))
	for _, message := range batch {
		attributes := message.Attributes
		if attributes == nil {
			attributes = map[string]string{}
		}
		records = append(records, map[string]any{
			"messageId":         message.MessageID,
			"receiptHandle":     message.ReceiptHandle,
			"body":              message.Body,
			"attributes":        attributes,
			"messageAttributes": map[string]any{},
			"md5OfBody":         message.MD5OfBody,
			"eventSource":       "aws:sqs",
			"eventSourceARN":    queueARN,
			"awsRegion":         s.scope.Region,
		})
	}
	return map[string]any{"Records": records}
}

func isSQSQueueARN(arn string) bool {
	return eventSourceService(arn) == "sqs"
}
