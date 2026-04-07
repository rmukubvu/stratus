package s3

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/store/fsblob"
)

const (
	bucketMetadataBucket = "s3-buckets"
	objectMetadataBucket = "s3-objects"
	documentNamespace    = "s3"
	xmlNamespace         = "http://s3.amazonaws.com/doc/2006-03-01/"
)

var errStopScan = errors.New("stop scan")

type Options struct {
	Metadata store.Store
	Blobs    *fsblob.Store
}

type Service struct {
	metadata store.Store
	blobs    *fsblob.Store
	now      func() time.Time
}

type bucketMetadata struct {
	Name    string    `json:"name"`
	Region  string    `json:"region"`
	Created time.Time `json:"created"`
}

type objectMetadata struct {
	Key           string    `json:"key"`
	ETag          string    `json:"etag"`
	ContentType   string    `json:"content_type"`
	ContentLength int64     `json:"content_length"`
	LastModified  time.Time `json:"last_modified"`
}

type listBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	XMLNS   string        `xml:"xmlns,attr"`
	Owner   bucketOwner   `xml:"Owner"`
	Buckets []bucketEntry `xml:"Buckets>Bucket"`
}

type bucketOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listObjectsV2Result struct {
	XMLName     xml.Name      `xml:"ListBucketResult"`
	XMLNS       string        `xml:"xmlns,attr"`
	Name        string        `xml:"Name"`
	Prefix      string        `xml:"Prefix"`
	MaxKeys     int           `xml:"MaxKeys"`
	KeyCount    int           `xml:"KeyCount"`
	IsTruncated bool          `xml:"IsTruncated"`
	Contents    []objectEntry `xml:"Contents"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func NewService(opts Options) *Service {
	return &Service{
		metadata: opts.Metadata,
		blobs:    opts.Blobs,
		now:      time.Now,
	}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if bucket == "" && key == "" {
		if r.Method != http.MethodGet {
			return methodNotAllowed("/")
		}
		return s.listBuckets(w)
	}

	if key == "" {
		switch r.Method {
		case http.MethodPut:
			return s.createBucket(w, r, bucket)
		case http.MethodGet:
			return s.listObjectsV2(w, r, bucket)
		case http.MethodHead:
			return s.headBucket(w, bucket)
		case http.MethodDelete:
			return s.deleteBucket(w, bucket)
		default:
			return methodNotAllowed("/" + bucket)
		}
	}

	switch r.Method {
	case http.MethodPut:
		return s.putObject(w, r, bucket, key)
	case http.MethodGet:
		return s.getObject(w, bucket, key)
	case http.MethodHead:
		return s.headObject(w, bucket, key)
	case http.MethodDelete:
		return s.deleteObject(w, bucket, key)
	default:
		return methodNotAllowed("/" + bucket + "/" + key)
	}
}

func (s *Service) listBuckets(w http.ResponseWriter) error {
	var buckets []bucketEntry
	if err := s.metadata.Scan(bucketMetadataBucket, "", func(_, v []byte) error {
		var meta bucketMetadata
		if err := json.Unmarshal(v, &meta); err != nil {
			return nil
		}
		buckets = append(buckets, bucketEntry{
			Name:         meta.Name,
			CreationDate: meta.Created.UTC().Format(time.RFC3339),
		})
		return nil
	}); err != nil {
		return internalError(err, "/")
	}

	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].Name < buckets[j].Name
	})
	if buckets == nil {
		buckets = []bucketEntry{}
	}

	writeXML(w, http.StatusOK, listBucketsResult{
		XMLNS: xmlNamespace,
		Owner: bucketOwner{
			ID:          "000000000000",
			DisplayName: "stratus",
		},
		Buckets: buckets,
	})
	return nil
}

func (s *Service) createBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket)
	}
	if exists {
		return apiError(http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", "/"+bucket)
	}

	region, err := locationConstraint(r.Body)
	if err != nil {
		return apiError(http.StatusBadRequest, "MalformedXML", err.Error(), "/"+bucket)
	}
	if region == "" {
		region = "us-east-1"
	}

	raw, err := json.Marshal(bucketMetadata{
		Name:    bucket,
		Region:  region,
		Created: s.now().UTC(),
	})
	if err != nil {
		return internalError(err, "/"+bucket)
	}
	if err := s.metadata.Put(bucketMetadataBucket, bucket, raw); err != nil {
		return internalError(err, "/"+bucket)
	}

	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) headBucket(w http.ResponseWriter, bucket string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket)
	}
	if !exists {
		return apiError(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) deleteBucket(w http.ResponseWriter, bucket string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket)
	}
	if !exists {
		return apiError(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket)
	}

	hasObjects := false
	err = s.metadata.Scan(objectMetadataBucket, bucket+"/", func(_, _ []byte) error {
		hasObjects = true
		return errStopScan
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return internalError(err, "/"+bucket)
	}
	if hasObjects {
		return apiError(http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty.", "/"+bucket)
	}

	if err := s.metadata.Delete(bucketMetadataBucket, bucket); err != nil {
		return internalError(err, "/"+bucket)
	}
	if err := s.blobs.DeleteNamespace(documentNamespace + "/" + bucket); err != nil {
		return internalError(err, "/"+bucket)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if !exists {
		return apiError(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket)
	}

	result, err := s.blobs.Put(documentNamespace+"/"+bucket, key, r.Body)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	raw, err := json.Marshal(objectMetadata{
		Key:           key,
		ETag:          `"` + result.MD5Hex + `"`,
		ContentType:   contentType,
		ContentLength: result.Size,
		LastModified:  s.now().UTC(),
	})
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if err := s.metadata.Put(objectMetadataBucket, objectKey(bucket, key), raw); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}

	w.Header().Set("ETag", `"`+result.MD5Hex+`"`)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) getObject(w http.ResponseWriter, bucket, key string) error {
	meta, err := s.loadObject(bucket, key)
	if err != nil {
		return err
	}

	file, err := s.blobs.Open(documentNamespace+"/"+bucket, key)
	if err != nil {
		if os.IsNotExist(err) {
			return apiError(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", "/"+bucket+"/"+key)
		}
		return internalError(err, "/"+bucket+"/"+key)
	}
	defer file.Close()

	setObjectHeaders(w, meta)
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, file); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	return nil
}

func (s *Service) headObject(w http.ResponseWriter, bucket, key string) error {
	meta, err := s.loadObject(bucket, key)
	if err != nil {
		return err
	}
	setObjectHeaders(w, meta)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) deleteObject(w http.ResponseWriter, bucket, key string) error {
	if err := s.metadata.Delete(objectMetadataBucket, objectKey(bucket, key)); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if err := s.blobs.Delete(documentNamespace+"/"+bucket, key); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket)
	}
	if !exists {
		return apiError(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket)
	}

	prefix := r.URL.Query().Get("prefix")
	maxKeys := 1000
	if raw := r.URL.Query().Get("max-keys"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxKeys = parsed
		}
	}

	var contents []objectEntry
	scanPrefix := bucket + "/"
	if prefix != "" {
		scanPrefix += prefix
	}

	err = s.metadata.Scan(objectMetadataBucket, scanPrefix, func(k, v []byte) error {
		if len(contents) >= maxKeys {
			return errStopScan
		}

		var meta objectMetadata
		if err := json.Unmarshal(v, &meta); err != nil {
			return nil
		}

		contents = append(contents, objectEntry{
			Key:          strings.TrimPrefix(string(k), bucket+"/"),
			LastModified: meta.LastModified.UTC().Format(time.RFC3339Nano),
			ETag:         meta.ETag,
			Size:         meta.ContentLength,
			StorageClass: "STANDARD",
		})
		return nil
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return internalError(err, "/"+bucket)
	}

	sort.Slice(contents, func(i, j int) bool {
		return contents[i].Key < contents[j].Key
	})
	if contents == nil {
		contents = []objectEntry{}
	}

	writeXML(w, http.StatusOK, listObjectsV2Result{
		XMLNS:       xmlNamespace,
		Name:        bucket,
		Prefix:      prefix,
		MaxKeys:     maxKeys,
		KeyCount:    len(contents),
		IsTruncated: false,
		Contents:    contents,
	})
	return nil
}

func (s *Service) bucketExists(bucket string) (bool, error) {
	raw, err := s.metadata.Get(bucketMetadataBucket, bucket)
	if err != nil {
		return false, err
	}
	return raw != nil, nil
}

func (s *Service) loadObject(bucket, key string) (objectMetadata, error) {
	raw, err := s.metadata.Get(objectMetadataBucket, objectKey(bucket, key))
	if err != nil {
		return objectMetadata{}, internalError(err, "/"+bucket+"/"+key)
	}
	if raw == nil {
		return objectMetadata{}, apiError(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", "/"+bucket+"/"+key)
	}

	var meta objectMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return objectMetadata{}, internalError(fmt.Errorf("decode object metadata: %w", err), "/"+bucket+"/"+key)
	}
	return meta, nil
}

func setObjectHeaders(w http.ResponseWriter, meta objectMetadata) {
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.ContentLength, 10))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
}

func objectKey(bucket, key string) string {
	return bucket + "/" + key
}

func locationConstraint(body io.ReadCloser) (string, error) {
	if body == nil {
		return "", nil
	}
	defer body.Close()

	type createBucketConfiguration struct {
		LocationConstraint string `xml:"LocationConstraint"`
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", nil
	}

	var cfg createBucketConfiguration
	if err := xml.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("invalid CreateBucketConfiguration body")
	}
	return cfg.LocationConstraint, nil
}

func methodNotAllowed(resource string) error {
	return apiError(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.", resource)
}

func internalError(err error, resource string) error {
	return apiError(http.StatusInternalServerError, "InternalError", err.Error(), resource)
}

func apiError(status int, code, message, resource string) error {
	return &apierror.Error{
		StatusCode: status,
		Code:       code,
		Message:    message,
		Resource:   resource,
	}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
