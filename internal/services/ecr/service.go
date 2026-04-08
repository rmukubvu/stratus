package ecr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	repositoriesBucket = "ecr-repositories"
	imagesBucket       = "ecr-images"
	accountID          = "000000000000"
	region             = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type repositoryRecord struct {
	Arn          string            `json:"arn"`
	CreatedAt    time.Time         `json:"created_at"`
	Name         string            `json:"name"`
	RegistryID   string            `json:"registry_id"`
	RepositoryURI string           `json:"repository_uri"`
	Tags         map[string]string `json:"tags,omitempty"`
}

type imageRecord struct {
	Digest     string    `json:"digest"`
	Manifest   string    `json:"manifest"`
	MediaType  string    `json:"media_type,omitempty"`
	PushedAt   time.Time `json:"pushed_at"`
	Repository string    `json:"repository"`
	Tag        string    `json:"tag,omitempty"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateRepository":
		return s.createRepository(w, r)
	case "DescribeRepositories":
		return s.describeRepositories(w, r)
	case "PutImage":
		return s.putImage(w, r)
	case "ListImages":
		return s.listImages(w, r)
	case "BatchGetImage":
		return s.batchGetImage(w, r)
	case "DeleteRepository":
		return s.deleteRepository(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "ecr operation is not implemented"}
	}
}

func (s *Service) createRepository(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		RepositoryName string `json:"repositoryName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.RepositoryName == "" {
		return validation("repositoryName is required")
	}
	if repo, err := s.loadRepository(input.RepositoryName); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"repository": repositoryResponse(repo)})
		return nil
	}
	record := repositoryRecord{
		Arn:           repositoryARN(input.RepositoryName),
		CreatedAt:     s.now().UTC(),
		Name:          input.RepositoryName,
		RegistryID:    accountID,
		RepositoryURI: repositoryURI(input.RepositoryName),
	}
	if err := s.putRepository(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": repositoryResponse(record)})
	return nil
}

func (s *Service) describeRepositories(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		RepositoryNames []string `json:"repositoryNames"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if len(input.RepositoryNames) > 0 {
		for _, name := range input.RepositoryNames {
			repo, err := s.loadRepository(name)
			if err != nil {
				return err
			}
			items = append(items, repositoryResponse(repo))
		}
	} else {
		if err := s.metadata.Scan(repositoriesBucket, "", func(_, v []byte) error {
			var repo repositoryRecord
			if err := json.Unmarshal(v, &repo); err != nil {
				return nil
			}
			items = append(items, repositoryResponse(repo))
			return nil
		}); err != nil {
			return internal(err)
		}
		sort.Slice(items, func(i, j int) bool { return items[i]["repositoryName"].(string) < items[j]["repositoryName"].(string) })
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": items})
	return nil
}

func (s *Service) putImage(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ImageManifest         string `json:"imageManifest"`
		ImageManifestMediaType string `json:"imageManifestMediaType"`
		ImageTag              string `json:"imageTag"`
		RepositoryName        string `json:"repositoryName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.RepositoryName == "" || input.ImageManifest == "" {
		return validation("repositoryName and imageManifest are required")
	}
	if _, err := s.loadRepository(input.RepositoryName); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(input.ImageManifest))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	record := imageRecord{
		Digest:     digest,
		Manifest:   input.ImageManifest,
		MediaType:  input.ImageManifestMediaType,
		PushedAt:   s.now().UTC(),
		Repository: input.RepositoryName,
		Tag:        input.ImageTag,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(imagesBucket, imageKey(input.RepositoryName, input.ImageTag, digest), raw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"image": map[string]any{
			"imageId": map[string]any{
				"imageDigest": digest,
				"imageTag":    input.ImageTag,
			},
			"imageManifest": input.ImageManifest,
			"registryId":    accountID,
			"repositoryName": input.RepositoryName,
		},
	})
	return nil
}

func (s *Service) listImages(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		RepositoryName string `json:"repositoryName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if _, err := s.loadRepository(input.RepositoryName); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(imagesBucket, input.RepositoryName+"|", func(_, v []byte) error {
		var image imageRecord
		if err := json.Unmarshal(v, &image); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"imageDigest": image.Digest,
			"imageTag":    image.Tag,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool {
		left := items[i]["imageTag"].(string)
		right := items[j]["imageTag"].(string)
		return left < right
	})
	writeJSON(w, http.StatusOK, map[string]any{"imageIds": items})
	return nil
}

func (s *Service) batchGetImage(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ImageIDs []struct {
			ImageDigest string `json:"imageDigest"`
			ImageTag    string `json:"imageTag"`
		} `json:"imageIds"`
		RepositoryName string `json:"repositoryName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if _, err := s.loadRepository(input.RepositoryName); err != nil {
		return err
	}
	images := make([]map[string]any, 0, len(input.ImageIDs))
	failures := make([]map[string]any, 0)
	for _, imageID := range input.ImageIDs {
		record, err := s.loadImage(input.RepositoryName, imageID.ImageTag, imageID.ImageDigest)
		if err != nil {
			failures = append(failures, map[string]any{
				"failureCode": "ImageNotFound",
				"failureReason": err.Error(),
				"imageId": map[string]any{
					"imageDigest": imageID.ImageDigest,
					"imageTag":    imageID.ImageTag,
				},
			})
			continue
		}
		images = append(images, map[string]any{
			"imageId": map[string]any{
				"imageDigest": record.Digest,
				"imageTag":    record.Tag,
			},
			"imageManifest":         record.Manifest,
			"imageManifestMediaType": record.MediaType,
			"registryId":            accountID,
			"repositoryName":        record.Repository,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": images, "failures": failures})
	return nil
}

func (s *Service) deleteRepository(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Force          bool   `json:"force"`
		RepositoryName string `json:"repositoryName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	repo, err := s.loadRepository(input.RepositoryName)
	if err != nil {
		return err
	}
	hasImages := false
	if err := s.metadata.Scan(imagesBucket, input.RepositoryName+"|", func(_, _ []byte) error {
		hasImages = true
		return nil
	}); err != nil {
		return internal(err)
	}
	if hasImages && !input.Force {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "RepositoryNotEmptyException", Message: "repository contains images"}
	}
	if err := s.metadata.DeletePrefix(imagesBucket, input.RepositoryName+"|"); err != nil {
		return internal(err)
	}
	if err := s.metadata.Delete(repositoriesBucket, input.RepositoryName); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": repositoryResponse(repo)})
	return nil
}

func (s *Service) loadRepository(name string) (repositoryRecord, error) {
	raw, err := s.metadata.Get(repositoriesBucket, name)
	if err != nil {
		return repositoryRecord{}, internal(err)
	}
	if raw == nil {
		return repositoryRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "RepositoryNotFoundException", Message: "repository not found"}
	}
	var repo repositoryRecord
	if err := json.Unmarshal(raw, &repo); err != nil {
		return repositoryRecord{}, internal(err)
	}
	return repo, nil
}

func (s *Service) putRepository(record repositoryRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(repositoriesBucket, record.Name, raw)
}

func (s *Service) loadImage(repository, tag, digest string) (imageRecord, error) {
	if tag != "" {
		raw, err := s.metadata.Get(imagesBucket, imageKey(repository, tag, ""))
		if err != nil {
			return imageRecord{}, internal(err)
		}
		if raw != nil {
			var image imageRecord
			if err := json.Unmarshal(raw, &image); err != nil {
				return imageRecord{}, internal(err)
			}
			return image, nil
		}
	}
	if digest != "" {
		var found imageRecord
		matched := false
		if err := s.metadata.Scan(imagesBucket, repository+"|", func(_, v []byte) error {
			var image imageRecord
			if err := json.Unmarshal(v, &image); err != nil {
				return nil
			}
			if image.Digest == digest {
				found = image
				matched = true
			}
			return nil
		}); err != nil {
			return imageRecord{}, internal(err)
		}
		if matched {
			return found, nil
		}
	}
	return imageRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ImageNotFoundException", Message: "image not found"}
}

func repositoryResponse(repo repositoryRecord) map[string]any {
	return map[string]any{
		"createdAt":     repo.CreatedAt.Format(time.RFC3339),
		"registryId":    repo.RegistryID,
		"repositoryArn": repo.Arn,
		"repositoryName": repo.Name,
		"repositoryUri": repo.RepositoryURI,
	}
}

func repositoryARN(name string) string {
	return "arn:aws:ecr:" + region + ":" + accountID + ":repository/" + name
}

func repositoryURI(name string) string {
	return accountID + ".dkr.ecr." + region + ".amazonaws.com/" + name
}

func imageKey(repository, tag, digest string) string {
	if tag != "" {
		return repository + "|" + tag
	}
	return repository + "|" + strings.TrimPrefix(digest, "sha256:")
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "ServerException", Message: err.Error()}
}

func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
