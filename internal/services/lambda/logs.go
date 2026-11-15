package lambda

import (
	"context"
	"strings"
	"time"
)

// LogSink receives a function's output, as Lambda sends it to CloudWatch
// Logs; the logs service fits.
type LogSink interface {
	Append(group, stream string, lines []string, at time.Time) error
}

// SetLogSink sends every invocation's output to sink, under the log group
// /aws/lambda/<function>, so code and agents can read what a function
// printed even when nobody invoked it directly (event source mappings,
// async invokes, SNS and EventBridge targets).
func (s *Service) SetLogSink(sink LogSink) {
	s.logs = sink
}

// invoke runs one invocation on the runtime and ships its output.
func (s *Service) invoke(ctx context.Context, record functionRecord, payload []byte) (InvokeResult, error) {
	result, err := s.invoker.Invoke(ctx, s.specFromRecord(record), payload)
	s.shipLogs(record, result.Logs)
	return result, err
}

func (s *Service) shipLogs(record functionRecord, output string) {
	output = strings.TrimRight(output, "\n")
	if s.logs == nil || output == "" {
		return
	}
	now := s.now().UTC()
	_ = s.logs.Append(logGroupName(record.FunctionName), logStreamName(now, record.Version), strings.Split(output, "\n"), now)
}

func logGroupName(function string) string { return "/aws/lambda/" + function }

// logStreamName follows Lambda's date/[version]instance shape; Stratus has
// one stream per function version per day.
func logStreamName(at time.Time, version string) string {
	if version == "" {
		version = "$LATEST"
	}
	return at.Format("2006/01/02") + "/[" + version + "]stratus"
}
