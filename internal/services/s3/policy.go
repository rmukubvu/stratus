package s3

import (
	"encoding/json"
	"io"
	"net/http"
)

// maxBucketPolicySize is the 20 KB limit S3 puts on a bucket policy.
const maxBucketPolicySize = 20 * 1024

// handleBucketPolicy serves PutBucketPolicy, GetBucketPolicy and
// DeleteBucketPolicy.
func (s *Service) handleBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) error {
	resource := "/" + bucket
	if err := s.requireBucket(bucket, resource); err != nil {
		return err
	}
	switch r.Method {
	case http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBucketPolicySize+1))
		if err != nil {
			return internalError(err, resource)
		}
		if len(raw) > maxBucketPolicySize {
			return apiError(http.StatusBadRequest, "PolicyTooLarge", "The bucket policy exceeds 20 KB.", resource)
		}
		if !json.Valid(raw) {
			return apiError(http.StatusBadRequest, "MalformedPolicy", "Policies must be valid JSON.", resource)
		}
		if err := s.metadata.Put(bucketPolicyBucket, bucket, raw); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	case http.MethodGet:
		policy, ok, err := s.BucketPolicy(bucket)
		if err != nil {
			return internalError(err, resource)
		}
		if !ok {
			return apiError(http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist", resource)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, policy)
		return nil
	case http.MethodDelete:
		if err := s.metadata.Delete(bucketPolicyBucket, bucket); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return methodNotAllowed(resource)
	}
}

// BucketPolicy returns a bucket's policy document.
func (s *Service) BucketPolicy(bucket string) (string, bool, error) {
	raw, err := s.metadata.Get(bucketPolicyBucket, bucket)
	if err != nil {
		return "", false, err
	}
	return string(raw), raw != nil, nil
}
