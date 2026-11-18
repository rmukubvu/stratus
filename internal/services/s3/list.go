package s3

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxListKeys is the most keys S3 returns in one page, whatever max-keys
// asks for.
const maxListKeys = 1000

// listPage is one page of a bucket listing: the objects and common
// prefixes after a starting point, in key order.
type listPage struct {
	contents  []objectEntry
	prefixes  []commonPrefix
	truncated bool
	// last is the last key or common prefix on the page, where the next
	// page starts after.
	last string
}

// listObjects answers ListObjectsV2 (list-type=2) and the original
// ListObjects. Both page as S3 does: at most max-keys entries, counting
// objects and common prefixes together, after a starting point that is
// itself left out. ListObjectsV2 starts after its continuation token, or
// else after start-after; ListObjects starts after its marker.
func (s *Service) listObjects(w http.ResponseWriter, r *http.Request, bucket string) error {
	resource := "/" + bucket
	if err := s.requireBucket(bucket, resource); err != nil {
		return err
	}
	q := r.URL.Query()
	v2 := q.Get("list-type") == "2"
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	maxKeys, err := parseMaxKeys(q, resource)
	if err != nil {
		return err
	}

	token, startAfter, marker := q.Get("continuation-token"), q.Get("start-after"), q.Get("marker")
	var after string
	switch {
	case v2 && q.Has("continuation-token"):
		decoded, ok := decodeContinuationToken(token)
		if !ok {
			return apiError(http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect", resource)
		}
		after = decoded
	case v2:
		after = startAfter
	default:
		after = marker
	}

	page, err := s.listBucket(bucket, prefix, delimiter, after, maxKeys)
	if err != nil {
		return internalError(err, resource)
	}

	if v2 {
		result := listObjectsV2Result{
			XMLNS:             xmlNamespace,
			Name:              bucket,
			Prefix:            prefix,
			Delimiter:         delimiter,
			MaxKeys:           maxKeys,
			KeyCount:          len(page.contents) + len(page.prefixes),
			IsTruncated:       page.truncated,
			ContinuationToken: token,
			StartAfter:        startAfter,
			Contents:          page.contents,
			CommonPrefixes:    page.prefixes,
		}
		if page.truncated {
			result.NextContinuationToken = encodeContinuationToken(page.last)
		}
		writeXML(w, http.StatusOK, result)
		return nil
	}

	result := listObjectsResult{
		XMLNS:          xmlNamespace,
		Name:           bucket,
		Prefix:         prefix,
		Marker:         marker,
		Delimiter:      delimiter,
		MaxKeys:        maxKeys,
		IsTruncated:    page.truncated,
		Contents:       page.contents,
		CommonPrefixes: page.prefixes,
	}
	// As in S3, NextMarker comes only with a delimiter; without one, a
	// client carries on from the last key it was given.
	if page.truncated && delimiter != "" {
		result.NextMarker = page.last
	}
	writeXML(w, http.StatusOK, result)
	return nil
}

// listBucket reads one page of the bucket's current objects under prefix.
//
// With a delimiter, every key that has it after the prefix is rolled up
// into the common prefix ending at its first delimiter, and that prefix is
// one entry in the page. An entry is a key, or a common prefix, that sorts
// after after; so a page that ended on a common prefix is followed by the
// first key past everything under it.
func (s *Service) listBucket(bucket, prefix, delimiter, after string, maxKeys int) (listPage, error) {
	var page listPage
	if maxKeys == 0 {
		return page, nil
	}
	err := s.metadata.Scan(objectMetadataBucket, bucket+"/"+prefix, func(k, v []byte) error {
		key := strings.TrimPrefix(string(k), bucket+"/")
		name, rolled := key, false
		if delimiter != "" {
			if i := strings.Index(key[len(prefix):], delimiter); i >= 0 {
				name, rolled = key[:len(prefix)+i+len(delimiter)], true
			}
		}
		if name <= after {
			return nil
		}
		// Keys under one common prefix sort together, so a prefix already
		// on the page is the last entry on it.
		if rolled && len(page.prefixes) > 0 && page.last == name {
			return nil
		}
		if len(page.contents)+len(page.prefixes) == maxKeys {
			page.truncated = true
			return errStopScan
		}
		if rolled {
			page.prefixes = append(page.prefixes, commonPrefix{Prefix: name})
			page.last = name
			return nil
		}
		var meta objectMetadata
		if err := json.Unmarshal(v, &meta); err != nil {
			return nil
		}
		page.contents = append(page.contents, objectEntry{
			Key:          key,
			LastModified: meta.LastModified.UTC().Format(time.RFC3339Nano),
			ETag:         meta.ETag,
			Size:         meta.ContentLength,
			StorageClass: "STANDARD",
		})
		page.last = key
		return nil
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return listPage{}, err
	}
	return page, nil
}

// parseMaxKeys reads max-keys: 1000 when absent, and never more than 1000
// in a page, as in S3. A value that is not a whole number from zero up is
// refused, as S3 refuses it.
func parseMaxKeys(q map[string][]string, resource string) (int, error) {
	values, ok := q["max-keys"]
	if !ok || len(values) == 0 || values[0] == "" {
		return maxListKeys, nil
	}
	n, err := strconv.ParseInt(values[0], 10, 32)
	if err != nil {
		return 0, apiError(http.StatusBadRequest, "InvalidArgument", "Provided max-keys not an integer or within integer range", resource)
	}
	if n < 0 {
		return 0, apiError(http.StatusBadRequest, "InvalidArgument", "Argument maxKeys must be an integer between 0 and 2147483647", resource)
	}
	return min(int(n), maxListKeys), nil
}

// A continuation token is opaque to the client. Here it is the entry the
// page ended on, so a token from any page of any listing of this bucket
// carries on from where that page stopped.
const continuationTokenPrefix = "stratus-list:"

func encodeContinuationToken(last string) string {
	return base64.StdEncoding.EncodeToString([]byte(continuationTokenPrefix + last))
}

func decodeContinuationToken(token string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil || !strings.HasPrefix(string(raw), continuationTokenPrefix) {
		return "", false
	}
	return strings.TrimPrefix(string(raw), continuationTokenPrefix), true
}
