package s3

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	multipartBucket      = "s3-multipart"
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
	Key            string    `json:"key"`
	ETag           string    `json:"etag"`
	ChecksumCRC32  string    `json:"checksum_crc32,omitempty"`
	ChecksumSHA256 string    `json:"checksum_sha256,omitempty"`
	ContentType    string    `json:"content_type"`
	ContentLength  int64     `json:"content_length"`
	LastModified   time.Time `json:"last_modified"`
}

type multipartUpload struct {
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	UploadID    string    `json:"upload_id"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

type multipartPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
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

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func NewService(opts Options) *Service {
	return &Service{
		metadata: opts.Metadata,
		blobs:    opts.Blobs,
		now:      time.Now,
	}
}

func (s *Service) ReadObjectBytes(bucket, key string) ([]byte, error) {
	if _, err := s.loadObject(bucket, key); err != nil {
		return nil, err
	}
	file, err := s.blobs.Open(documentNamespace+"/"+bucket, key)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apiError(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", "/"+bucket+"/"+key)
		}
		return nil, internalError(err, "/"+bucket+"/"+key)
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil {
		return nil, internalError(err, "/"+bucket+"/"+key)
	}
	return body, nil
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if uploadID := r.URL.Query().Get("uploadId"); uploadID != "" {
		switch r.Method {
		case http.MethodPut:
			return s.uploadPart(w, r, bucket, key, uploadID)
		case http.MethodPost:
			return s.completeMultipartUpload(w, r, bucket, key, uploadID)
		case http.MethodDelete:
			return s.abortMultipartUpload(w, bucket, uploadID)
		default:
			return methodNotAllowed("/" + bucket + "/" + key)
		}
	}
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
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			return s.copyObject(w, r, bucket, key)
		}
		return s.putObject(w, r, bucket, key)
	case http.MethodPost:
		if _, ok := r.URL.Query()["uploads"]; ok {
			return s.createMultipartUpload(w, r, bucket, key)
		}
		return methodNotAllowed("/" + bucket + "/" + key)
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

	bodyReader := io.Reader(r.Body)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "aws-chunked") {
		bodyReader = httputil.NewChunkedReader(bufio.NewReader(r.Body))
	}
	result, err := s.blobs.Put(documentNamespace+"/"+bucket, key, bodyReader)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta := objectMetadata{
		Key:            key,
		ETag:           `"` + result.MD5Hex + `"`,
		ChecksumCRC32:  result.CRC32Base64,
		ChecksumSHA256: result.SHA256Base64,
		ContentType:    contentType,
		ContentLength:  result.Size,
		LastModified:   s.now().UTC(),
	}
	if err := s.putObjectMetadata(bucket, meta); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}

	if !strings.EqualFold(r.Header.Get("Content-Encoding"), "aws-chunked") {
		w.Header().Set("ETag", `"`+result.MD5Hex+`"`)
	}
	setChecksumHeaders(w, meta)
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
	w.Header().Set("Accept-Ranges", "bytes")
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

func (s *Service) createMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if !exists {
		return apiError(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket)
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	upload := multipartUpload{
		Bucket:      bucket,
		Key:         key,
		UploadID:    strconv.FormatInt(s.now().UnixNano(), 36),
		ContentType: contentType,
		CreatedAt:   s.now().UTC(),
	}
	raw, err := json.Marshal(upload)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if err := s.metadata.Put(multipartBucket, multipartUploadKey(bucket, upload.UploadID), raw); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{
		XMLNS:    xmlNamespace,
		Bucket:   bucket,
		Key:      key,
		UploadID: upload.UploadID,
	})
	return nil
}

func (s *Service) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) error {
	upload, err := s.loadMultipartUpload(bucket, uploadID)
	if err != nil {
		return err
	}
	if upload.Key != key {
		return apiError(http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.", "/"+bucket+"/"+key)
	}
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 || partNumber > 10000 {
		return apiError(http.StatusBadRequest, "InvalidArgument", "partNumber must be between 1 and 10000.", "/"+bucket+"/"+key)
	}
	result, err := s.blobs.Put(multipartNamespace(bucket, uploadID), strconv.Itoa(partNumber), r.Body)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	part := multipartPart{PartNumber: partNumber, ETag: `"` + result.MD5Hex + `"`, Size: result.Size}
	raw, err := json.Marshal(part)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if err := s.metadata.Put(multipartBucket, multipartPartKey(bucket, uploadID, partNumber), raw); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	w.Header().Set("ETag", part.ETag)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) error {
	upload, err := s.loadMultipartUpload(bucket, uploadID)
	if err != nil {
		return err
	}
	if upload.Key != key {
		return apiError(http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.", "/"+bucket+"/"+key)
	}
	parts, err := decodeCompletedParts(r.Body)
	if err != nil {
		return apiError(http.StatusBadRequest, "MalformedXML", err.Error(), "/"+bucket+"/"+key)
	}
	readers := make([]io.Reader, 0, len(parts))
	closers := make([]io.Closer, 0, len(parts))
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()

	for _, completed := range parts {
		partMeta, err := s.loadMultipartPart(bucket, uploadID, completed.PartNumber)
		if err != nil {
			return err
		}
		if completed.ETag != "" && completed.ETag != partMeta.ETag {
			return apiError(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.", "/"+bucket+"/"+key)
		}
		file, err := s.blobs.Open(multipartNamespace(bucket, uploadID), strconv.Itoa(completed.PartNumber))
		if err != nil {
			return internalError(err, "/"+bucket+"/"+key)
		}
		closers = append(closers, file)
		readers = append(readers, file)
	}

	result, err := s.blobs.Put(documentNamespace+"/"+bucket, key, io.MultiReader(readers...))
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	meta := objectMetadata{
		Key:            key,
		ETag:           `"` + result.MD5Hex + `"`,
		ChecksumCRC32:  result.CRC32Base64,
		ChecksumSHA256: result.SHA256Base64,
		ContentType:    upload.ContentType,
		ContentLength:  result.Size,
		LastModified:   s.now().UTC(),
	}
	if err := s.putObjectMetadata(bucket, meta); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if err := s.deleteMultipartUpload(bucket, uploadID); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{
		XMLNS:    xmlNamespace,
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     meta.ETag,
	})
	return nil
}

func (s *Service) abortMultipartUpload(w http.ResponseWriter, bucket, uploadID string) error {
	if err := s.deleteMultipartUpload(bucket, uploadID); err != nil {
		return internalError(err, "/"+bucket)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	exists, err := s.bucketExists(bucket)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	if !exists {
		return apiError(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket)
	}
	source := strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/")
	source, err = url.PathUnescape(source)
	if err != nil || source == "" {
		return apiError(http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source must identify an existing object.", "/"+bucket+"/"+key)
	}
	parts := strings.SplitN(source, "/", 2)
	if len(parts) != 2 {
		return apiError(http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source must identify an existing object.", "/"+bucket+"/"+key)
	}
	sourceMeta, err := s.loadObject(parts[0], parts[1])
	if err != nil {
		return err
	}
	file, err := s.blobs.Open(documentNamespace+"/"+parts[0], parts[1])
	if err != nil {
		if os.IsNotExist(err) {
			return apiError(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", "/"+parts[0]+"/"+parts[1])
		}
		return internalError(err, "/"+bucket+"/"+key)
	}
	defer file.Close()

	result, err := s.blobs.Put(documentNamespace+"/"+bucket, key, file)
	if err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}
	meta := objectMetadata{
		Key:            key,
		ETag:           `"` + result.MD5Hex + `"`,
		ChecksumCRC32:  result.CRC32Base64,
		ChecksumSHA256: result.SHA256Base64,
		ContentType:    sourceMeta.ContentType,
		ContentLength:  result.Size,
		LastModified:   s.now().UTC(),
	}
	if err := s.putObjectMetadata(bucket, meta); err != nil {
		return internalError(err, "/"+bucket+"/"+key)
	}

	type copyResult struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}
	writeXML(w, http.StatusOK, copyResult{
		ETag:         meta.ETag,
		LastModified: meta.LastModified.UTC().Format(time.RFC3339Nano),
	})
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

func (s *Service) putObjectMetadata(bucket string, meta objectMetadata) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return s.metadata.Put(objectMetadataBucket, objectKey(bucket, meta.Key), raw)
}

func (s *Service) loadMultipartUpload(bucket, uploadID string) (multipartUpload, error) {
	raw, err := s.metadata.Get(multipartBucket, multipartUploadKey(bucket, uploadID))
	if err != nil {
		return multipartUpload{}, internalError(err, "/"+bucket)
	}
	if raw == nil {
		return multipartUpload{}, apiError(http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.", "/"+bucket)
	}
	var upload multipartUpload
	if err := json.Unmarshal(raw, &upload); err != nil {
		return multipartUpload{}, internalError(err, "/"+bucket)
	}
	return upload, nil
}

func (s *Service) loadMultipartPart(bucket, uploadID string, partNumber int) (multipartPart, error) {
	raw, err := s.metadata.Get(multipartBucket, multipartPartKey(bucket, uploadID, partNumber))
	if err != nil {
		return multipartPart{}, internalError(err, "/"+bucket)
	}
	if raw == nil {
		return multipartPart{}, apiError(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.", "/"+bucket)
	}
	var part multipartPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return multipartPart{}, internalError(err, "/"+bucket)
	}
	return part, nil
}

func (s *Service) deleteMultipartUpload(bucket, uploadID string) error {
	if err := s.metadata.Delete(multipartBucket, multipartUploadKey(bucket, uploadID)); err != nil {
		return err
	}
	if err := s.metadata.DeletePrefix(multipartBucket, multipartPartPrefix(bucket, uploadID)); err != nil {
		return err
	}
	return s.blobs.DeleteNamespace(multipartNamespace(bucket, uploadID))
}

func setObjectHeaders(w http.ResponseWriter, meta objectMetadata) {
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.ContentLength, 10))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	setChecksumHeaders(w, meta)
}

func setChecksumHeaders(w http.ResponseWriter, meta objectMetadata) {
	if meta.ChecksumCRC32 != "" {
		w.Header().Set("x-amz-checksum-crc32", meta.ChecksumCRC32)
	}
	if meta.ChecksumSHA256 != "" {
		w.Header().Set("x-amz-checksum-sha256", meta.ChecksumSHA256)
	}
}

func objectKey(bucket, key string) string {
	return bucket + "/" + key
}

func multipartUploadKey(bucket, uploadID string) string {
	return bucket + "|" + uploadID
}

func multipartPartPrefix(bucket, uploadID string) string {
	return bucket + "|" + uploadID + "|part|"
}

func multipartPartKey(bucket, uploadID string, partNumber int) string {
	return multipartPartPrefix(bucket, uploadID) + fmt.Sprintf("%05d", partNumber)
}

func multipartNamespace(bucket, uploadID string) string {
	return documentNamespace + "-multipart/" + bucket + "/" + uploadID
}

type completeMultipartUploadRequest struct {
	Parts []completedPart `xml:"Part"`
}

type completedPart struct {
	ETag       string `xml:"ETag"`
	PartNumber int    `xml:"PartNumber"`
}

func decodeCompletedParts(body io.ReadCloser) ([]completedPart, error) {
	if body == nil {
		return nil, fmt.Errorf("missing CompleteMultipartUpload body")
	}
	defer body.Close()
	var payload completeMultipartUploadRequest
	if err := xml.NewDecoder(body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid CompleteMultipartUpload body")
	}
	if len(payload.Parts) == 0 {
		return nil, fmt.Errorf("at least one part is required")
	}
	return payload.Parts, nil
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
