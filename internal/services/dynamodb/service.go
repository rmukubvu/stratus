package dynamodb

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	tablesBucket = "dynamodb-tables"
	itemsBucket  = "dynamodb-items"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
}

type attributeDefinition struct {
	AttributeName string `json:"AttributeName"`
	AttributeType string `json:"AttributeType"`
}

type keySchemaElement struct {
	AttributeName string `json:"AttributeName"`
	KeyType       string `json:"KeyType"`
}

type tableRecord struct {
	AttributeDefinitions []attributeDefinition `json:"attribute_definitions"`
	BillingMode          string                `json:"billing_mode"`
	CreatedAt            time.Time             `json:"created_at"`
	HashKey              string                `json:"hash_key"`
	HashKeyType          string                `json:"hash_key_type"`
	TableName            string                `json:"table_name"`
	TableStatus          string                `json:"table_status"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "CreateTable":
		return s.createTable(w, r)
	case "ListTables":
		return s.listTables(w)
	case "PutItem":
		return s.putItem(w, r)
	case "GetItem":
		return s.getItem(w, r)
	case "DeleteTable":
		return s.deleteTable(w, r)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "dynamodb operation is not implemented",
		}
	}
}

func (s *Service) createTable(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		AttributeDefinitions []attributeDefinition `json:"AttributeDefinitions"`
		BillingMode          string                `json:"BillingMode"`
		KeySchema            []keySchemaElement    `json:"KeySchema"`
		TableName            string                `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	if input.TableName == "" {
		return badRequest("ValidationException", "TableName is required")
	}
	if _, err := s.loadTable(input.TableName); err == nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceInUseException",
			Message:    "Cannot create preexisting table",
		}
	}
	if len(input.KeySchema) != 1 || input.KeySchema[0].KeyType != "HASH" {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "only HASH primary keys are supported",
		}
	}

	hashKey := input.KeySchema[0].AttributeName
	hashType := ""
	for _, attr := range input.AttributeDefinitions {
		if attr.AttributeName == hashKey {
			hashType = attr.AttributeType
			break
		}
	}
	if hashType == "" {
		return badRequest("ValidationException", "HASH key attribute definition is required")
	}
	if input.BillingMode == "" {
		input.BillingMode = "PAY_PER_REQUEST"
	}
	record := tableRecord{
		AttributeDefinitions: input.AttributeDefinitions,
		BillingMode:          input.BillingMode,
		CreatedAt:            s.now().UTC(),
		HashKey:              hashKey,
		HashKeyType:          hashType,
		TableName:            input.TableName,
		TableStatus:          "ACTIVE",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(tablesBucket, input.TableName, raw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"TableDescription": s.tableDescription(record),
	})
	return nil
}

func (s *Service) listTables(w http.ResponseWriter) error {
	var tables []string
	if err := s.metadata.Scan(tablesBucket, "", func(k, _ []byte) error {
		tables = append(tables, string(k))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Strings(tables)
	writeJSON(w, http.StatusOK, map[string]any{"TableNames": tables})
	return nil
}

func (s *Service) putItem(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Item      map[string]any `json:"Item"`
		TableName string         `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	table, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	keyValue, ok := input.Item[table.HashKey]
	if !ok {
		return badRequest("ValidationException", "item is missing HASH key")
	}
	keyRaw, err := json.Marshal(keyValue)
	if err != nil {
		return internal(err)
	}
	itemRaw, err := json.Marshal(input.Item)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(itemsBucket, table.TableName+"|"+string(keyRaw), itemRaw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) getItem(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Key       map[string]any `json:"Key"`
		TableName string         `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	table, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	keyValue, ok := input.Key[table.HashKey]
	if !ok {
		return badRequest("ValidationException", "key is missing HASH key")
	}
	keyRaw, err := json.Marshal(keyValue)
	if err != nil {
		return internal(err)
	}
	itemRaw, err := s.metadata.Get(itemsBucket, table.TableName+"|"+string(keyRaw))
	if err != nil {
		return internal(err)
	}
	if itemRaw == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	}
	var item map[string]any
	if err := json.Unmarshal(itemRaw, &item); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Item": item})
	return nil
}

func (s *Service) deleteTable(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName string `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	table, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(tablesBucket, input.TableName); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(itemsBucket, input.TableName+"|"); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"TableDescription": s.tableDescription(table)})
	return nil
}

func (s *Service) loadTable(name string) (tableRecord, error) {
	raw, err := s.metadata.Get(tablesBucket, name)
	if err != nil {
		return tableRecord{}, internal(err)
	}
	if raw == nil {
		return tableRecord{}, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceNotFoundException",
			Message:    "Requested resource not found",
		}
	}
	var record tableRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return tableRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) tableDescription(record tableRecord) map[string]any {
	keySchema := []map[string]string{{
		"AttributeName": record.HashKey,
		"KeyType":       "HASH",
	}}
	attrs := make([]map[string]string, 0, len(record.AttributeDefinitions))
	for _, attr := range record.AttributeDefinitions {
		attrs = append(attrs, map[string]string{
			"AttributeName": attr.AttributeName,
			"AttributeType": attr.AttributeType,
		})
	}
	return map[string]any{
		"AttributeDefinitions": attrs,
		"BillingModeSummary": map[string]any{
			"BillingMode": record.BillingMode,
		},
		"ItemCount":             0,
		"KeySchema":             keySchema,
		"ProvisionedThroughput": map[string]any{"NumberOfDecreasesToday": 0, "ReadCapacityUnits": 0, "WriteCapacityUnits": 0},
		"TableArn":              "arn:aws:dynamodb:us-east-1:000000000000:table/" + record.TableName,
		"TableName":             record.TableName,
		"TableStatus":           record.TableStatus,
		"TableSizeBytes":        0,
	}
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerError", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
