package acm

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	certificatesBucket = "acm-certificates"
	accountID          = "000000000000"
	region             = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type certificateRecord struct {
	Arn         string    `json:"arn"`
	CreatedAt   time.Time `json:"created_at"`
	DomainName  string    `json:"domain_name"`
	Status      string    `json:"status"`
	Type        string    `json:"type"`
	Validation  string    `json:"validation"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "RequestCertificate":
		return s.requestCertificate(w, r)
	case "DescribeCertificate":
		return s.describeCertificate(w, r)
	case "ListCertificates":
		return s.listCertificates(w)
	case "DeleteCertificate":
		return s.deleteCertificate(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "acm operation is not implemented"}
	}
}

func (s *Service) requestCertificate(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		DomainName      string `json:"DomainName"`
		ValidationMethod string `json:"ValidationMethod"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.DomainName == "" {
		return validation("DomainName is required")
	}
	if input.ValidationMethod == "" {
		input.ValidationMethod = "DNS"
	}
	record := certificateRecord{
		Arn:        certificateARN(),
		CreatedAt:  s.now().UTC(),
		DomainName: input.DomainName,
		Status:     "ISSUED",
		Type:       "AMAZON_ISSUED",
		Validation: input.ValidationMethod,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(certificatesBucket, record.Arn, raw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"CertificateArn": record.Arn})
	return nil
}

func (s *Service) describeCertificate(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		CertificateArn string `json:"CertificateArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.loadCertificate(input.CertificateArn)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Certificate": map[string]any{
			"CertificateArn": record.Arn,
			"CreatedAt":      record.CreatedAt.Format(time.RFC3339),
			"DomainName":     record.DomainName,
			"DomainValidationOptions": []map[string]any{{
				"DomainName":       record.DomainName,
				"ValidationMethod": record.Validation,
				"ValidationStatus": "SUCCESS",
			}},
			"InUseBy": []string{},
			"IssuedAt": record.CreatedAt.Format(time.RFC3339),
			"KeyAlgorithm": "RSA_2048",
			"Status": record.Status,
			"Type":   record.Type,
		},
	})
	return nil
}

func (s *Service) listCertificates(w http.ResponseWriter) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(certificatesBucket, "", func(_, v []byte) error {
		var record certificateRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"CertificateArn": record.Arn,
			"DomainName":     record.DomainName,
			"Status":         record.Status,
			"Type":           record.Type,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["DomainName"].(string) < items[j]["DomainName"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"CertificateSummaryList": items})
	return nil
}

func (s *Service) deleteCertificate(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		CertificateArn string `json:"CertificateArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if _, err := s.loadCertificate(input.CertificateArn); err != nil {
		return err
	}
	if err := s.metadata.Delete(certificatesBucket, input.CertificateArn); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) loadCertificate(arn string) (certificateRecord, error) {
	raw, err := s.metadata.Get(certificatesBucket, arn)
	if err != nil {
		return certificateRecord{}, internal(err)
	}
	if raw == nil {
		return certificateRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "certificate not found"}
	}
	var record certificateRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return certificateRecord{}, internal(err)
	}
	return record, nil
}

func certificateARN() string {
	return "arn:aws:acm:" + region + ":" + accountID + ":certificate/" + uuid.NewString()
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
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
