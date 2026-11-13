package s3

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Versioned storage. The current version of a key stays in
// objectMetadataBucket under "<bucket>/<key>", so reads and listings of
// current objects are unchanged. In a bucket with versioning Enabled every
// version, delete markers included, is also recorded in objectVersionsBucket
// under "<bucket>/<key>\x00<version id>", and its body is stored in the
// bucket's version namespace under the version ID. An object written before
// versioning was enabled is the "null" version and keeps its body at the
// unversioned location.
const (
	objectVersionsBucket = "s3-object-versions"
	versionNamespace     = "s3-versions"
	nullVersionID        = "null"

	versioningEnabled   = "Enabled"
	versioningSuspended = "Suspended"
)

var versionSequence atomic.Uint32

// newVersionID returns an opaque 32-character ID that sorts newest first:
// inverted nanoseconds, an inverted sequence for writes in the same
// nanosecond, then random bits.
func newVersionID(now time.Time) string {
	var random [4]byte
	_, _ = rand.Read(random[:])
	return fmt.Sprintf("%016x%08x%s", uint64(math.MaxInt64-now.UnixNano()), math.MaxUint32-versionSequence.Add(1), hex.EncodeToString(random[:]))
}

func versionRecordKey(bucket, key, versionID string) string {
	return versionPrefix(bucket, key) + versionID
}

func versionPrefix(bucket, key string) string {
	return objectKey(bucket, key) + "\x00"
}

// blobLocation is where a version's body is stored.
func blobLocation(bucket string, meta objectMetadata) (namespace, key string) {
	if meta.VersionID == "" || meta.VersionID == nullVersionID {
		return documentNamespace + "/" + bucket, meta.Key
	}
	return versionNamespace + "/" + bucket, meta.VersionID
}

type versioningConfigurationXML struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status"`
}

// versioningStatus is "", Enabled or Suspended.
func (s *Service) versioningStatus(bucket string) (string, error) {
	raw, err := s.metadata.Get(bucketConfigBucket, bucket+"/versioning")
	if err != nil || raw == nil {
		return "", err
	}
	var doc versioningConfigurationXML
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return "", nil
	}
	return doc.Status, nil
}

func (s *Service) lockConfig(bucket string) (bucketLockConfig, error) {
	raw, err := s.metadata.Get(bucketConfigBucket, bucket+"/object-lock")
	if err != nil || raw == nil {
		return bucketLockConfig{}, err
	}
	return parseLockConfig(raw, "/"+bucket)
}

// storeObject writes a new object body for PutObject, CopyObject and
// CompleteMultipartUpload. With versioning Enabled it adds a version and
// keeps the previous ones; otherwise it replaces the current object.
func (s *Service) storeObject(bucket, key string, body io.Reader, contentType string, lock objectLock) (objectMetadata, error) {
	status, err := s.versioningStatus(bucket)
	if err != nil {
		return objectMetadata{}, err
	}
	meta := objectMetadata{Key: key, ContentType: contentType, LastModified: s.now().UTC(), Lock: lock}
	switch status {
	case versioningEnabled:
		if err := s.preserveNullVersion(bucket, key); err != nil {
			return objectMetadata{}, err
		}
		meta.VersionID = newVersionID(s.now())
	case versioningSuspended:
		meta.VersionID = nullVersionID
	}
	namespace, blobKey := blobLocation(bucket, meta)
	result, err := s.blobs.Put(namespace, blobKey, body)
	if err != nil {
		return objectMetadata{}, err
	}
	meta.ETag = `"` + result.MD5Hex + `"`
	meta.ChecksumCRC32 = result.CRC32Base64
	meta.ChecksumSHA256 = result.SHA256Base64
	meta.ContentLength = result.Size
	if meta.VersionID != "" {
		if err := s.putVersionRecord(bucket, meta); err != nil {
			return objectMetadata{}, err
		}
	}
	return meta, s.putObjectMetadata(bucket, meta)
}

// preserveNullVersion records an object written before versioning was
// enabled as the "null" version before a versioned write supersedes it.
func (s *Service) preserveNullVersion(bucket, key string) error {
	current, found, err := s.currentObject(bucket, key)
	if err != nil || !found || current.VersionID != "" {
		return err
	}
	current.VersionID = nullVersionID
	return s.putVersionRecord(bucket, current)
}

func (s *Service) currentObject(bucket, key string) (objectMetadata, bool, error) {
	raw, err := s.metadata.Get(objectMetadataBucket, objectKey(bucket, key))
	if err != nil || raw == nil {
		return objectMetadata{}, false, err
	}
	var meta objectMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return objectMetadata{}, false, err
	}
	return meta, true, nil
}

func (s *Service) putVersionRecord(bucket string, meta objectMetadata) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return s.metadata.Put(objectVersionsBucket, versionRecordKey(bucket, meta.Key, meta.VersionID), raw)
}

// loadVersion finds one version of a key. The "null" version of an object
// written before versioning has no version record; it is the current object.
func (s *Service) loadVersion(bucket, key, versionID string) (objectMetadata, bool, error) {
	raw, err := s.metadata.Get(objectVersionsBucket, versionRecordKey(bucket, key, versionID))
	if err != nil {
		return objectMetadata{}, false, err
	}
	if raw != nil {
		var meta objectMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			return objectMetadata{}, false, err
		}
		return meta, true, nil
	}
	if versionID != nullVersionID {
		return objectMetadata{}, false, nil
	}
	current, found, err := s.currentObject(bucket, key)
	if err != nil || !found || current.VersionID != "" {
		return objectMetadata{}, false, err
	}
	return current, true, nil
}

// requireVersion loads a version named by ?versionId, or the current object
// when versionID is empty.
func (s *Service) requireVersion(bucket, key, versionID string) (objectMetadata, error) {
	if versionID == "" {
		return s.loadObject(bucket, key)
	}
	resource := "/" + bucket + "/" + key
	if err := s.requireBucket(bucket, resource); err != nil {
		return objectMetadata{}, err
	}
	meta, found, err := s.loadVersion(bucket, key, versionID)
	if err != nil {
		return objectMetadata{}, internalError(err, resource)
	}
	if !found {
		return objectMetadata{}, apiError(http.StatusNotFound, "NoSuchVersion", "The specified version does not exist.", resource)
	}
	return meta, nil
}

// saveVersion stores changed lock state on a version and on the current
// object when it is that version.
func (s *Service) saveVersion(bucket string, meta objectMetadata) error {
	if meta.VersionID != "" && s.hasVersionRecord(bucket, meta) {
		if err := s.putVersionRecord(bucket, meta); err != nil {
			return err
		}
	}
	current, found, err := s.currentObject(bucket, meta.Key)
	if err != nil || !found {
		return err
	}
	sameVersion := current.VersionID == meta.VersionID || (current.VersionID == "" && meta.VersionID == nullVersionID)
	if !sameVersion {
		return nil
	}
	current.Lock = meta.Lock
	return s.putObjectMetadata(bucket, current)
}

func (s *Service) hasVersionRecord(bucket string, meta objectMetadata) bool {
	raw, err := s.metadata.Get(objectVersionsBucket, versionRecordKey(bucket, meta.Key, meta.VersionID))
	return err == nil && raw != nil
}

// versionsOf lists a key's version records, newest first.
func (s *Service) versionsOf(bucket, key string) ([]objectMetadata, error) {
	var versions []objectMetadata
	err := s.metadata.Scan(objectVersionsBucket, versionPrefix(bucket, key), func(_, v []byte) error {
		var meta objectMetadata
		if json.Unmarshal(v, &meta) == nil {
			versions = append(versions, meta)
		}
		return nil
	})
	sortNewestFirst(versions)
	return versions, err
}

func sortNewestFirst(versions []objectMetadata) {
	sort.SliceStable(versions, func(i, j int) bool {
		a, b := versions[i], versions[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if !a.LastModified.Equal(b.LastModified) {
			return a.LastModified.After(b.LastModified)
		}
		return versionOrder(a.VersionID) < versionOrder(b.VersionID)
	})
}

// versionOrder makes the "null" version sort after generated IDs written in
// the same instant.
func versionOrder(id string) string {
	if id == nullVersionID {
		return "~"
	}
	return id
}

// deleteObject serves DeleteObject. Without a version ID on a versioned
// bucket it adds a delete marker, which Object Lock never blocks; with one it
// permanently deletes that version, which a lock can refuse.
func (s *Service) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	resource := "/" + bucket + "/" + key
	if err := s.requireBucket(bucket, resource); err != nil {
		return err
	}
	if versionID := r.URL.Query().Get("versionId"); versionID != "" {
		return s.deleteVersion(w, r, bucket, key, versionID)
	}
	status, err := s.versioningStatus(bucket)
	if err != nil {
		return internalError(err, resource)
	}
	if status == "" {
		if err := s.removeUnversioned(bucket, key); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	marker, err := s.addDeleteMarker(bucket, key, status)
	if err != nil {
		return internalError(err, resource)
	}
	w.Header().Set("X-Amz-Delete-Marker", "true")
	w.Header().Set("X-Amz-Version-Id", marker.VersionID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) removeUnversioned(bucket, key string) error {
	if err := s.metadata.Delete(objectMetadataBucket, objectKey(bucket, key)); err != nil {
		return err
	}
	if err := s.metadata.Delete(objectTagsBucket, objectKey(bucket, key)); err != nil {
		return err
	}
	return s.blobs.Delete(documentNamespace+"/"+bucket, key)
}

func (s *Service) addDeleteMarker(bucket, key, status string) (objectMetadata, error) {
	marker := objectMetadata{Key: key, DeleteMarker: true, LastModified: s.now().UTC()}
	if status == versioningEnabled {
		if err := s.preserveNullVersion(bucket, key); err != nil {
			return objectMetadata{}, err
		}
		marker.VersionID = newVersionID(s.now())
	} else {
		// Suspended: the marker replaces the null version.
		if err := s.removeNullVersion(bucket, key); err != nil {
			return objectMetadata{}, err
		}
		marker.VersionID = nullVersionID
	}
	if err := s.putVersionRecord(bucket, marker); err != nil {
		return objectMetadata{}, err
	}
	return marker, s.metadata.Delete(objectMetadataBucket, objectKey(bucket, key))
}

func (s *Service) removeNullVersion(bucket, key string) error {
	if err := s.metadata.Delete(objectVersionsBucket, versionRecordKey(bucket, key, nullVersionID)); err != nil {
		return err
	}
	return s.blobs.Delete(documentNamespace+"/"+bucket, key)
}

// deleteVersion permanently removes one version, subject to Object Lock, and
// promotes the next newest version when the current one goes.
func (s *Service) deleteVersion(w http.ResponseWriter, r *http.Request, bucket, key, versionID string) error {
	resource := "/" + bucket + "/" + key
	meta, found, err := s.loadVersion(bucket, key, versionID)
	if err != nil {
		return internalError(err, resource)
	}
	if !found {
		w.Header().Set("X-Amz-Version-Id", versionID)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if err := meta.Lock.checkDelete(s.now(), bypassGovernance(r), resource); err != nil {
		return err
	}
	if err := s.metadata.Delete(objectVersionsBucket, versionRecordKey(bucket, key, versionID)); err != nil {
		return internalError(err, resource)
	}
	if !meta.DeleteMarker {
		namespace, blobKey := blobLocation(bucket, meta)
		if err := s.blobs.Delete(namespace, blobKey); err != nil {
			return internalError(err, resource)
		}
	}
	if err := s.promoteLatest(bucket, key); err != nil {
		return internalError(err, resource)
	}
	if meta.DeleteMarker {
		w.Header().Set("X-Amz-Delete-Marker", "true")
	}
	w.Header().Set("X-Amz-Version-Id", versionID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// promoteLatest makes the newest remaining version current, or leaves the
// key without a current object when that version is a delete marker or none
// remain. A pre-versioning object with no version record is left alone.
func (s *Service) promoteLatest(bucket, key string) error {
	versions, err := s.versionsOf(bucket, key)
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		current, found, err := s.currentObject(bucket, key)
		if err != nil || (found && current.VersionID == "") {
			return err
		}
		if err := s.metadata.Delete(objectTagsBucket, objectKey(bucket, key)); err != nil {
			return err
		}
		return s.metadata.Delete(objectMetadataBucket, objectKey(bucket, key))
	}
	if versions[0].DeleteMarker {
		return s.metadata.Delete(objectMetadataBucket, objectKey(bucket, key))
	}
	return s.putObjectMetadata(bucket, versions[0])
}

// latestIsDeleteMarker reports whether a key's newest version is a delete
// marker, for the x-amz-delete-marker header on a missing current object.
func (s *Service) latestIsDeleteMarker(bucket, key string) bool {
	versions, err := s.versionsOf(bucket, key)
	return err == nil && len(versions) > 0 && versions[0].DeleteMarker
}

// bucketHasVersions reports whether any version record remains, which keeps
// a bucket from being deleted.
func (s *Service) bucketHasVersions(bucket string) (bool, error) {
	found := false
	err := s.metadata.Scan(objectVersionsBucket, bucket+"/", func(_, _ []byte) error {
		found = true
		return errStopScan
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return false, err
	}
	return found, nil
}

type listVersionsResult struct {
	XMLName         xml.Name            `xml:"ListVersionsResult"`
	XMLNS           string              `xml:"xmlns,attr"`
	Name            string              `xml:"Name"`
	Prefix          string              `xml:"Prefix"`
	KeyMarker       string              `xml:"KeyMarker"`
	VersionIDMarker string              `xml:"VersionIdMarker"`
	MaxKeys         int                 `xml:"MaxKeys"`
	IsTruncated     bool                `xml:"IsTruncated"`
	Versions        []versionEntry      `xml:"Version"`
	DeleteMarkers   []deleteMarkerEntry `xml:"DeleteMarker"`
}

type versionEntry struct {
	Key          string      `xml:"Key"`
	VersionID    string      `xml:"VersionId"`
	IsLatest     bool        `xml:"IsLatest"`
	LastModified string      `xml:"LastModified"`
	ETag         string      `xml:"ETag"`
	Size         int64       `xml:"Size"`
	StorageClass string      `xml:"StorageClass"`
	Owner        bucketOwner `xml:"Owner"`
}

type deleteMarkerEntry struct {
	Key          string      `xml:"Key"`
	VersionID    string      `xml:"VersionId"`
	IsLatest     bool        `xml:"IsLatest"`
	LastModified string      `xml:"LastModified"`
	Owner        bucketOwner `xml:"Owner"`
}

// listObjectVersions serves ListObjectVersions (GET ?versions) for a prefix,
// returning every version in one page.
func (s *Service) listObjectVersions(w http.ResponseWriter, r *http.Request, bucket string) error {
	resource := "/" + bucket
	if err := s.requireBucket(bucket, resource); err != nil {
		return err
	}
	prefix := r.URL.Query().Get("prefix")
	var versions []objectMetadata
	recorded := map[string]bool{}
	err := s.metadata.Scan(objectVersionsBucket, bucket+"/"+prefix, func(_, v []byte) error {
		var meta objectMetadata
		if json.Unmarshal(v, &meta) == nil {
			versions = append(versions, meta)
			if meta.VersionID == nullVersionID {
				recorded[meta.Key] = true
			}
		}
		return nil
	})
	if err != nil {
		return internalError(err, resource)
	}
	// Objects written before versioning are the null version.
	err = s.metadata.Scan(objectMetadataBucket, bucket+"/"+prefix, func(_, v []byte) error {
		var meta objectMetadata
		if json.Unmarshal(v, &meta) == nil && meta.VersionID == "" && !recorded[meta.Key] {
			meta.VersionID = nullVersionID
			versions = append(versions, meta)
		}
		return nil
	})
	if err != nil {
		return internalError(err, resource)
	}
	sortNewestFirst(versions)

	owner := bucketOwner{ID: s.scope.Account, DisplayName: "stratus"}
	result := listVersionsResult{XMLNS: xmlNamespace, Name: bucket, Prefix: prefix, MaxKeys: 1000}
	for i, v := range versions {
		latest := i == 0 || versions[i-1].Key != v.Key
		modified := v.LastModified.UTC().Format(time.RFC3339Nano)
		if v.DeleteMarker {
			result.DeleteMarkers = append(result.DeleteMarkers, deleteMarkerEntry{Key: v.Key, VersionID: v.VersionID, IsLatest: latest, LastModified: modified, Owner: owner})
			continue
		}
		result.Versions = append(result.Versions, versionEntry{
			Key: v.Key, VersionID: v.VersionID, IsLatest: latest, LastModified: modified,
			ETag: v.ETag, Size: v.ContentLength, StorageClass: "STANDARD", Owner: owner,
		})
	}
	writeXML(w, http.StatusOK, result)
	return nil
}

type retentionXML struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

type legalHoldXML struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

// handleObjectRetention serves GetObjectRetention and PutObjectRetention.
func (s *Service) handleObjectRetention(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	meta, resource, err := s.lockTarget(r, bucket, key)
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		if meta.Lock.Mode == "" {
			return noObjectLockConfiguration(resource)
		}
		writeXML(w, http.StatusOK, retentionXML{XMLNS: xmlNamespace, Mode: meta.Lock.Mode, RetainUntilDate: meta.Lock.RetainUntil.UTC().Format(time.RFC3339)})
		return nil
	case http.MethodPut:
		body, err := readConfig(r, "Retention", resource)
		if err != nil {
			return err
		}
		var doc retentionXML
		if err := xml.Unmarshal(body, &doc); err != nil {
			return malformedXML(resource)
		}
		next := retention{Mode: doc.Mode}
		if doc.RetainUntilDate != "" {
			if next.RetainUntil, err = time.Parse(time.RFC3339, doc.RetainUntilDate); err != nil {
				return malformedXML(resource)
			}
		}
		if (next.Mode == "") != next.RetainUntil.IsZero() {
			return malformedXML(resource)
		}
		now := s.now()
		if err := validateRetention(next, now, resource); err != nil {
			return err
		}
		if err := meta.Lock.checkRetentionChange(next, now, bypassGovernance(r), resource); err != nil {
			return err
		}
		meta.Lock.Mode, meta.Lock.RetainUntil = next.Mode, next.RetainUntil.UTC()
		if err := s.saveVersion(bucket, meta); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusOK)
		return nil
	default:
		return methodNotAllowed(resource)
	}
}

// handleObjectLegalHold serves GetObjectLegalHold and PutObjectLegalHold.
func (s *Service) handleObjectLegalHold(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	meta, resource, err := s.lockTarget(r, bucket, key)
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		if !meta.Lock.LegalHold {
			// S3 reports OFF once a hold has been set; Stratus does not
			// remember whether one ever was, so it always answers OFF.
			writeXML(w, http.StatusOK, legalHoldXML{XMLNS: xmlNamespace, Status: legalHoldOff})
			return nil
		}
		writeXML(w, http.StatusOK, legalHoldXML{XMLNS: xmlNamespace, Status: legalHoldOn})
		return nil
	case http.MethodPut:
		body, err := readConfig(r, "LegalHold", resource)
		if err != nil {
			return err
		}
		var doc legalHoldXML
		if err := xml.Unmarshal(body, &doc); err != nil || (doc.Status != legalHoldOn && doc.Status != legalHoldOff) {
			return malformedXML(resource)
		}
		meta.Lock.LegalHold = doc.Status == legalHoldOn
		if err := s.saveVersion(bucket, meta); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusOK)
		return nil
	default:
		return methodNotAllowed(resource)
	}
}

// lockTarget resolves the version a retention or legal-hold request names
// and requires the bucket to have Object Lock.
func (s *Service) lockTarget(r *http.Request, bucket, key string) (objectMetadata, string, error) {
	resource := "/" + bucket + "/" + key
	if err := s.requireBucket(bucket, resource); err != nil {
		return objectMetadata{}, resource, err
	}
	cfg, err := s.lockConfig(bucket)
	if err != nil {
		return objectMetadata{}, resource, internalError(err, resource)
	}
	if !cfg.Enabled {
		return objectMetadata{}, resource, missingLockConfiguration(resource)
	}
	meta, err := s.requireVersion(bucket, key, r.URL.Query().Get("versionId"))
	if err != nil {
		return objectMetadata{}, resource, err
	}
	if meta.DeleteMarker {
		return objectMetadata{}, resource, methodNotAllowed(resource)
	}
	return meta, resource, nil
}

// guardVersioningChange stops versioning being suspended on an Object Lock
// bucket.
func (s *Service) guardVersioningChange(bucket string, body []byte, resource string) error {
	var doc versioningConfigurationXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return malformedXML(resource)
	}
	if doc.Status != "" && doc.Status != versioningEnabled && doc.Status != versioningSuspended {
		return malformedXML(resource)
	}
	cfg, err := s.lockConfig(bucket)
	if err != nil {
		return internalError(err, resource)
	}
	if cfg.Enabled && doc.Status != versioningEnabled {
		return apiError(http.StatusConflict, "InvalidBucketState", "An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.", resource)
	}
	return nil
}

// guardLockConfiguration validates PutObjectLockConfiguration, which needs
// versioning Enabled.
func (s *Service) guardLockConfiguration(bucket string, body []byte, resource string) error {
	if _, err := parseLockConfig(body, resource); err != nil {
		return err
	}
	status, err := s.versioningStatus(bucket)
	if err != nil {
		return internalError(err, resource)
	}
	if status != versioningEnabled {
		return apiError(http.StatusConflict, "InvalidBucketState", "Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration", resource)
	}
	return nil
}

func noObjectLockConfiguration(resource string) error {
	return apiError(http.StatusNotFound, "NoSuchObjectLockConfiguration", "The specified object does not have a ObjectLock configuration", resource)
}

// parseCopySource splits x-amz-copy-source into bucket, key and version ID.
func parseCopySource(header string) (bucket, key, versionID string, ok bool) {
	source := strings.TrimPrefix(header, "/")
	if i := strings.Index(source, "?versionId="); i >= 0 {
		versionID = source[i+len("?versionId="):]
		source = source[:i]
	}
	parts := strings.SplitN(source, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], versionID, true
}
