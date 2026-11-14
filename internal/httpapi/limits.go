package httpapi

import (
	"net/http"
	"strings"

	"github.com/stratus/internal/limits"
)

// LimitGate admits a request against the active limits profile, returning
// a breach when it exceeds a quota.
type LimitGate interface {
	Admit(req limits.Request) *limits.Breach
}

// LimitHeader names the quota rule that throttled a response, so a
// throttle caused by the limits profile can be told from a chaos fault.
const LimitHeader = "X-Stratus-Limit"

// admitLimits checks the request against the limits profile and returns
// the service's throttling error when a quota is exceeded.
func (s *Server) admitLimits(w http.ResponseWriter, r *http.Request, md RequestMetadata) error {
	if s.limits == nil || md.Classification.Protocol == ProtocolInternal {
		return nil
	}
	breach := s.limits.Admit(limitRequest(r, md))
	if breach == nil {
		return nil
	}
	w.Header().Set(LimitHeader, breach.RuleID)
	return limits.ThrottleError(*breach)
}

func limitRequest(r *http.Request, md RequestMetadata) limits.Request {
	c := md.Classification
	req := limits.Request{
		RequestID: md.RequestID,
		Account:   md.Scope.Account,
		Region:    md.Scope.Region,
		Service:   c.Service,
		Operation: c.Operation,
	}
	switch c.Service {
	case "s3":
		req.Resource = c.Bucket
		if c.Bucket != "" {
			req.Prefix = s3Prefix(c.Bucket, c.Key)
		}
	case "lambda":
		req.Resource = lambdaFunctionName(c.Service, r.URL.Path)
	}
	return req
}

// s3Prefix is the "directory" part of an object key, which is roughly how
// S3 partitions request rates: "bucket/a/b/" for key "a/b/c.json".
func s3Prefix(bucket, key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return bucket + "/" + key[:i+1]
	}
	return bucket + "/"
}
