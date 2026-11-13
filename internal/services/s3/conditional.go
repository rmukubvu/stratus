package s3

import (
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
)

// Conditional writes: PutObject and CompleteMultipartUpload honour
// If-None-Match: * (write only if the key has no current object) and
// If-Match: <etag> (write only over that exact object), as S3 does.
// A key whose latest version is a delete marker has no current object.

const keyLockStripes = 64

// keyLocks serialises conditional writes to the same key, so two
// If-None-Match: * writes racing for one key cannot both succeed.
type keyLocks struct {
	stripes [keyLockStripes]sync.Mutex
}

func (l *keyLocks) lock(bucket, key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(objectKey(bucket, key)))
	m := &l.stripes[h.Sum32()%keyLockStripes]
	m.Lock()
	return m.Unlock
}

func isConditionalWrite(r *http.Request) bool {
	return r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Match") != ""
}

// checkWritePreconditions evaluates If-None-Match and If-Match against the
// key's current object. The caller holds the key lock.
func (s *Service) checkWritePreconditions(r *http.Request, bucket, key string) error {
	resource := "/" + bucket + "/" + key
	ifNoneMatch, ifMatch := r.Header.Get("If-None-Match"), r.Header.Get("If-Match")
	if ifNoneMatch != "" && ifNoneMatch != "*" {
		return apiError(http.StatusNotImplemented, "NotImplemented", "A header you provided implies functionality that is not implemented", resource)
	}
	current, found, err := s.currentObject(bucket, key)
	if err != nil {
		return internalError(err, resource)
	}
	if ifNoneMatch == "*" && found {
		return preconditionFailed(resource)
	}
	if ifMatch != "" {
		if !found {
			return errNoSuchKey(bucket, key)
		}
		if ifMatch != "*" && !etagMatches(ifMatch, current.ETag) {
			return preconditionFailed(resource)
		}
	}
	return nil
}

// etagMatches compares an If-Match value (one or more comma-separated,
// optionally quoted ETags) with a stored ETag.
func etagMatches(header, etag string) bool {
	want := strings.Trim(etag, `"`)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if strings.Trim(candidate, `"`) == want {
			return true
		}
	}
	return false
}

func preconditionFailed(resource string) error {
	return apiError(http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", resource)
}
