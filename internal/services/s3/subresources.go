package s3

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stratus/internal/tags"
)

const (
	bucketConfigBucket = "s3-bucket-configs"
	objectTagsBucket   = "s3-object-tags"

	// maxConfigSize bounds a stored configuration document.
	maxConfigSize = 1 << 20
	// maxObjectTags is S3's limit on tags per object (buckets take 50).
	maxObjectTags = 10
)

// bucketConfig describes one bucket configuration sub-resource (?tagging,
// ?cors, ...) that S3 stores as a document: PUT stores it, GET returns it and
// DELETE removes it. When none is stored, GET either fails with
// missingCode or returns defaultDoc, matching what S3 answers for a new bucket.
type bucketConfig struct {
	rootElement    string
	missingCode    string
	missingMessage string
	defaultDoc     string
	deletable      bool
	putStatus      int
	validate       func(body []byte, resource string) error
}

// bucketConfigs are the sub-resources served from stored documents.
var bucketConfigs = map[string]bucketConfig{
	"tagging": {
		rootElement: "Tagging", missingCode: "NoSuchTagSet", missingMessage: "The TagSet does not exist",
		deletable: true, putStatus: http.StatusNoContent, validate: validateBucketTagging,
	},
	"versioning": {
		rootElement: "VersioningConfiguration", defaultDoc: `<VersioningConfiguration xmlns="` + xmlNamespace + `"/>`,
		putStatus: http.StatusOK,
	},
	"lifecycle": {
		rootElement: "LifecycleConfiguration", missingCode: "NoSuchLifecycleConfiguration",
		missingMessage: "The lifecycle configuration does not exist", deletable: true, putStatus: http.StatusOK,
	},
	"cors": {
		rootElement: "CORSConfiguration", missingCode: "NoSuchCORSConfiguration",
		missingMessage: "The CORS configuration does not exist", deletable: true, putStatus: http.StatusOK,
	},
	"website": {
		rootElement: "WebsiteConfiguration", missingCode: "NoSuchWebsiteConfiguration",
		missingMessage: "The specified bucket does not have a website configuration", deletable: true, putStatus: http.StatusOK,
	},
	"replication": {
		rootElement: "ReplicationConfiguration", missingCode: "ReplicationConfigurationNotFoundError",
		missingMessage: "The replication configuration was not found", deletable: true, putStatus: http.StatusOK,
	},
	"object-lock": {
		rootElement: "ObjectLockConfiguration", missingCode: "ObjectLockConfigurationNotFoundError",
		missingMessage: "Object Lock configuration does not exist for this bucket", putStatus: http.StatusOK,
	},
	"encryption": {
		rootElement: "ServerSideEncryptionConfiguration", deletable: true, putStatus: http.StatusOK,
		defaultDoc: `<ServerSideEncryptionConfiguration xmlns="` + xmlNamespace + `"><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>false</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`,
	},
	"ownershipControls": {
		rootElement: "OwnershipControls", deletable: true, putStatus: http.StatusOK,
		defaultDoc: `<OwnershipControls xmlns="` + xmlNamespace + `"><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`,
	},
	"publicAccessBlock": {
		rootElement: "PublicAccessBlockConfiguration", deletable: true, putStatus: http.StatusOK,
		defaultDoc: `<PublicAccessBlockConfiguration xmlns="` + xmlNamespace + `"><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`,
	},
	"notification": {
		rootElement: "NotificationConfiguration", putStatus: http.StatusOK,
		defaultDoc: `<NotificationConfiguration xmlns="` + xmlNamespace + `"/>`,
	},
	"logging": {
		rootElement: "BucketLoggingStatus", putStatus: http.StatusOK,
		defaultDoc: `<BucketLoggingStatus xmlns="` + xmlNamespace + `"/>`,
	},
	"accelerate": {
		rootElement: "AccelerateConfiguration", putStatus: http.StatusOK,
		defaultDoc: `<AccelerateConfiguration xmlns="` + xmlNamespace + `"/>`,
	},
	"requestPayment": {
		rootElement: "RequestPaymentConfiguration", putStatus: http.StatusOK,
		defaultDoc: `<RequestPaymentConfiguration xmlns="` + xmlNamespace + `"><Payer>BucketOwner</Payer></RequestPaymentConfiguration>`,
	},
}

// unsupportedSubresources are sub-resources Stratus does not serve. A request
// naming one is refused with NotImplemented instead of being mistaken for a
// plain bucket or object request (a PUT ?acl must never create a bucket or
// overwrite an object).
var unsupportedSubresources = []string{
	"acl", "analytics", "attributes", "intelligent-tiering", "inventory", "legal-hold",
	"metadataConfiguration", "metadataTable", "metrics", "restore", "retention",
	"select", "torrent", "versions", "abac",
}

// bucketSubresource names the sub-resource a bucket-level request targets.
func bucketSubresource(query url.Values) string {
	if query.Has("location") {
		return "location"
	}
	for name := range bucketConfigs {
		if query.Has(name) {
			return name
		}
	}
	return unsupportedSubresource(query)
}

func unsupportedSubresource(query url.Values) string {
	for _, name := range unsupportedSubresources {
		if query.Has(name) {
			return name
		}
	}
	return ""
}

func notImplementedSubresource(name, resource string) error {
	return apiError(http.StatusNotImplemented, "NotImplemented", "S3 ?"+name+" is not implemented", resource)
}

// handleBucketSubresource serves a bucket-level request that names a
// sub-resource.
func (s *Service) handleBucketSubresource(w http.ResponseWriter, r *http.Request, bucket, name string) error {
	resource := "/" + bucket
	if err := s.requireBucket(bucket, resource); err != nil {
		return err
	}
	if name == "location" {
		return s.getBucketLocation(w, r, bucket)
	}
	config, ok := bucketConfigs[name]
	if !ok {
		return notImplementedSubresource(name, resource)
	}
	key := bucket + "/" + name
	switch r.Method {
	case http.MethodPut:
		body, err := readConfig(r, config.rootElement, resource)
		if err != nil {
			return err
		}
		if config.validate != nil {
			if err := config.validate(body, resource); err != nil {
				return err
			}
		}
		if err := s.metadata.Put(bucketConfigBucket, key, body); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(config.putStatus)
		return nil
	case http.MethodGet:
		raw, err := s.metadata.Get(bucketConfigBucket, key)
		if err != nil {
			return internalError(err, resource)
		}
		if raw == nil {
			if config.defaultDoc == "" {
				return apiError(http.StatusNotFound, config.missingCode, config.missingMessage, resource)
			}
			raw = []byte(config.defaultDoc)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return nil
	case http.MethodDelete:
		if !config.deletable {
			return methodNotAllowed(resource)
		}
		if err := s.metadata.Delete(bucketConfigBucket, key); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return methodNotAllowed(resource)
	}
}

func (s *Service) getBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != http.MethodGet {
		return methodNotAllowed("/" + bucket)
	}
	raw, err := s.metadata.Get(bucketMetadataBucket, bucket)
	if err != nil {
		return internalError(err, "/"+bucket)
	}
	var meta bucketMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return internalError(err, "/"+bucket)
	}
	region := meta.Region
	if region == "us-east-1" {
		region = "" // S3 reports the original region as an empty constraint.
	}
	writeXML(w, http.StatusOK, struct {
		XMLName xml.Name `xml:"LocationConstraint"`
		XMLNS   string   `xml:"xmlns,attr"`
		Region  string   `xml:",chardata"`
	}{XMLNS: xmlNamespace, Region: region})
	return nil
}

// deleteBucketConfigs removes every stored configuration of a deleted bucket.
func (s *Service) deleteBucketConfigs(bucket string) error {
	for name := range bucketConfigs {
		if err := s.metadata.Delete(bucketConfigBucket, bucket+"/"+name); err != nil {
			return err
		}
	}
	return nil
}

// readConfig reads a configuration document and checks it is well-formed XML
// whose root element is root.
func readConfig(r *http.Request, root, resource string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigSize+1))
	if err != nil {
		return nil, internalError(err, resource)
	}
	if len(body) > maxConfigSize {
		return nil, apiError(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size", resource)
	}
	if rootElement(body) != root {
		return nil, malformedXML(resource)
	}
	return body, nil
}

// rootElement is the local name of a document's first element, or "" when
// the document is not well-formed XML.
func rootElement(body []byte) string {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	root := ""
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return root
		}
		if err != nil {
			return ""
		}
		if start, ok := tok.(xml.StartElement); ok && root == "" {
			root = start.Name.Local
		}
	}
}

func malformedXML(resource string) error {
	return apiError(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema", resource)
}

type taggingXML struct {
	XMLName xml.Name    `xml:"Tagging"`
	XMLNS   string      `xml:"xmlns,attr,omitempty"`
	TagSet  []tagXMLDoc `xml:"TagSet>Tag"`
}

type tagXMLDoc struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// parseTagging reads a <Tagging> document into a validated tag list of at
// most max tags.
func parseTagging(body []byte, max int, resource string) (tags.List, error) {
	var doc taggingXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, malformedXML(resource)
	}
	list := make(tags.List, 0, len(doc.TagSet))
	seen := map[string]bool{}
	for _, t := range doc.TagSet {
		if seen[t.Key] {
			return nil, apiError(http.StatusBadRequest, "InvalidTag", "Cannot provide multiple Tags with the same key", resource)
		}
		seen[t.Key] = true
		list = append(list, tags.Tag{Key: t.Key, Value: t.Value})
	}
	if err := list.Validate(max); err != nil {
		var verr *tags.ValidationError
		if errors.As(err, &verr) && verr.Reason == tags.TooMany {
			return nil, apiError(http.StatusBadRequest, "BadRequest", "Object tags cannot be greater than 10", resource)
		}
		return nil, apiError(http.StatusBadRequest, "InvalidTag", err.Error(), resource)
	}
	return list, nil
}

func validateBucketTagging(body []byte, resource string) error {
	_, err := parseTagging(body, tags.MaxPerResource, resource)
	return err
}

// handleObjectTagging serves PutObjectTagging, GetObjectTagging and
// DeleteObjectTagging.
func (s *Service) handleObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	resource := "/" + bucket + "/" + key
	if err := s.requireBucket(bucket, resource); err != nil {
		return err
	}
	if _, err := s.loadObject(bucket, key); err != nil {
		return err
	}
	switch r.Method {
	case http.MethodPut:
		body, err := readConfig(r, "Tagging", resource)
		if err != nil {
			return err
		}
		list, err := parseTagging(body, maxObjectTags, resource)
		if err != nil {
			return err
		}
		if err := s.putObjectTags(bucket, key, list); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusOK)
		return nil
	case http.MethodGet:
		list, err := s.objectTags(bucket, key)
		if err != nil {
			return internalError(err, resource)
		}
		doc := taggingXML{XMLNS: xmlNamespace, TagSet: []tagXMLDoc{}}
		for _, t := range list {
			doc.TagSet = append(doc.TagSet, tagXMLDoc{Key: t.Key, Value: t.Value})
		}
		writeXML(w, http.StatusOK, doc)
		return nil
	case http.MethodDelete:
		if err := s.metadata.Delete(objectTagsBucket, objectKey(bucket, key)); err != nil {
			return internalError(err, resource)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return methodNotAllowed(resource)
	}
}

// objectTagsFromHeader reads the URL-encoded x-amz-tagging header of
// PutObject, CopyObject and CreateMultipartUpload.
func objectTagsFromHeader(r *http.Request, resource string) (tags.List, error) {
	raw := r.Header.Get("X-Amz-Tagging")
	if raw == "" {
		return nil, nil
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "InvalidArgument", "The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.", resource)
	}
	list := tags.List{}
	for key, vals := range values {
		if len(vals) != 1 {
			return nil, apiError(http.StatusBadRequest, "InvalidArgument", "The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.", resource)
		}
		list = append(list, tags.Tag{Key: key, Value: vals[0]})
	}
	if err := list.Validate(maxObjectTags); err != nil {
		return nil, apiError(http.StatusBadRequest, "InvalidTag", err.Error(), resource)
	}
	return list, nil
}

func (s *Service) putObjectTags(bucket, key string, list tags.List) error {
	if len(list) == 0 {
		return s.metadata.Delete(objectTagsBucket, objectKey(bucket, key))
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.metadata.Put(objectTagsBucket, objectKey(bucket, key), raw)
}

func (s *Service) objectTags(bucket, key string) (tags.List, error) {
	raw, err := s.metadata.Get(objectTagsBucket, objectKey(bucket, key))
	if err != nil || raw == nil {
		return nil, err
	}
	var list tags.List
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}
