package dynamodb

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/services/dynamodbstreams"
	"github.com/stratus/internal/store"
)

const (
	tablesBucket = "dynamodb-tables"
	itemsBucket  = "dynamodb-items"
)

type Service struct {
	metadata store.Store
	streams  *dynamodbstreams.Service
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
	StreamArn            string                `json:"stream_arn,omitempty"`
	StreamEnabled        bool                  `json:"stream_enabled,omitempty"`
	StreamViewType       string                `json:"stream_view_type,omitempty"`
	TableName            string                `json:"table_name"`
	TableStatus          string                `json:"table_status"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) SetStreams(streams *dynamodbstreams.Service) {
	s.streams = streams
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "CreateTable":
		return s.createTable(w, r)
	case "DescribeTable":
		return s.describeTable(w, r)
	case "DescribeContinuousBackups":
		return s.describeContinuousBackups(w, r)
	case "DescribeTimeToLive":
		return s.describeTimeToLive(w, r)
	case "ListTables":
		return s.listTables(w)
	case "ListTagsOfResource":
		return s.listTagsOfResource(w, r)
	case "PutItem":
		return s.putItem(w, r)
	case "GetItem":
		return s.getItem(w, r)
	case "UpdateItem":
		return s.updateItem(w, r)
	case "Query":
		return s.query(w, r)
	case "Scan":
		return s.scan(w, r)
	case "BatchGetItem":
		return s.batchGetItem(w, r)
	case "BatchWriteItem":
		return s.batchWriteItem(w, r)
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
		StreamSpecification  struct {
			StreamEnabled  bool   `json:"StreamEnabled"`
			StreamViewType string `json:"StreamViewType"`
		} `json:"StreamSpecification"`
		TableName string `json:"TableName"`
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
	if input.StreamSpecification.StreamEnabled {
		viewType := input.StreamSpecification.StreamViewType
		if viewType == "" {
			return badRequest("ValidationException", "StreamViewType is required when streams are enabled")
		}
		if s.streams == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "dynamodb streams are not configured"}
		}
		streamArn, err := s.streams.EnsureStream(input.TableName, viewType)
		if err != nil {
			return err
		}
		record.StreamArn = streamArn
		record.StreamEnabled = true
		record.StreamViewType = viewType
	}
	if err := s.putTable(record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"TableDescription": s.tableDescription(record),
	})
	return nil
}

func (s *Service) describeTable(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName string `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	record, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"Table": s.tableDescription(record)})
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

func (s *Service) describeContinuousBackups(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName string `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	record, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ContinuousBackupsDescription": map[string]any{
			"ContinuousBackupsStatus": "DISABLED",
			"PointInTimeRecoveryDescription": map[string]any{
				"PointInTimeRecoveryStatus": "DISABLED",
			},
			"TableName": record.TableName,
		},
	})
	return nil
}

func (s *Service) describeTimeToLive(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName string `json:"TableName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	if _, err := s.loadTable(input.TableName); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"TimeToLiveDescription": map[string]any{
			"TimeToLiveStatus": "DISABLED",
		},
	})
	return nil
}

func (s *Service) listTagsOfResource(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ResourceArn string `json:"ResourceArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	tableName := tableNameFromARN(input.ResourceArn)
	record, err := s.loadTable(tableName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Tags":        []map[string]string{},
		"ResourceArn": "arn:aws:dynamodb:us-east-1:000000000000:table/" + record.TableName,
	})
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
	existing, err := s.loadItem(table, map[string]any{table.HashKey: input.Item[table.HashKey]})
	if err != nil {
		return err
	}
	if err := s.putStoredItem(table, input.Item); err != nil {
		return err
	}
	_ = s.emitStreamRecord(table, existing, input.Item, ternary(existing == nil, "INSERT", "MODIFY"))
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
	item, err := s.loadItem(table, input.Key)
	if err != nil {
		return err
	}
	if item == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"Item": item})
	return nil
}

func (s *Service) updateItem(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Key                       map[string]any    `json:"Key"`
		TableName                 string            `json:"TableName"`
		UpdateExpression          string            `json:"UpdateExpression"`
		ExpressionAttributeNames  map[string]string `json:"ExpressionAttributeNames"`
		ExpressionAttributeValues map[string]any    `json:"ExpressionAttributeValues"`
		ReturnValues              string            `json:"ReturnValues"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	table, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	item, err := s.loadItem(table, input.Key)
	if err != nil {
		return err
	}
	existed := item != nil
	if item == nil {
		item = cloneAnyMap(input.Key)
	}
	before := cloneAnyMap(item)
	if err := applyUpdateExpression(item, input.UpdateExpression, input.ExpressionAttributeNames, input.ExpressionAttributeValues); err != nil {
		return err
	}
	if err := s.putStoredItem(table, item); err != nil {
		return err
	}
	if !existed {
		before = nil
	}
	_ = s.emitStreamRecord(table, before, item, ternary(existed, "MODIFY", "INSERT"))

	response := map[string]any{}
	switch input.ReturnValues {
	case "", "NONE":
	case "ALL_NEW", "UPDATED_NEW":
		response["Attributes"] = item
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "ReturnValues mode is not implemented"}
	}
	writeJSON(w, http.StatusOK, response)
	return nil
}

func (s *Service) query(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName                 string            `json:"TableName"`
		KeyConditionExpression    string            `json:"KeyConditionExpression"`
		ExpressionAttributeNames  map[string]string `json:"ExpressionAttributeNames"`
		ExpressionAttributeValues map[string]any    `json:"ExpressionAttributeValues"`
		Limit                     int               `json:"Limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	table, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	keyName, keyValue, err := parseKeyCondition(input.KeyConditionExpression, input.ExpressionAttributeNames, input.ExpressionAttributeValues)
	if err != nil {
		return err
	}
	if keyName != table.HashKey {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "only HASH key equality queries are supported"}
	}
	item, err := s.loadItem(table, map[string]any{table.HashKey: keyValue})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	if item != nil {
		items = append(items, item)
	}
	if input.Limit > 0 && len(items) > input.Limit {
		items = items[:input.Limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Count":        len(items),
		"Items":        items,
		"ScannedCount": len(items),
	})
	return nil
}

func (s *Service) scan(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName string `json:"TableName"`
		Limit     int    `json:"Limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	table, err := s.loadTable(input.TableName)
	if err != nil {
		return err
	}
	items, err := s.listItems(table)
	if err != nil {
		return err
	}
	if input.Limit > 0 && len(items) > input.Limit {
		items = items[:input.Limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Count":        len(items),
		"Items":        items,
		"ScannedCount": len(items),
	})
	return nil
}

func (s *Service) batchGetItem(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		RequestItems map[string]struct {
			Keys []map[string]any `json:"Keys"`
		} `json:"RequestItems"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	responses := map[string]any{}
	for tableName, request := range input.RequestItems {
		table, err := s.loadTable(tableName)
		if err != nil {
			return err
		}
		items := make([]map[string]any, 0, len(request.Keys))
		for _, key := range request.Keys {
			item, err := s.loadItem(table, key)
			if err != nil {
				return err
			}
			if item != nil {
				items = append(items, item)
			}
		}
		responses[tableName] = items
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Responses":       responses,
		"UnprocessedKeys": map[string]any{},
	})
	return nil
}

func (s *Service) batchWriteItem(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		RequestItems map[string][]struct {
			DeleteRequest *struct {
				Key map[string]any `json:"Key"`
			} `json:"DeleteRequest"`
			PutRequest *struct {
				Item map[string]any `json:"Item"`
			} `json:"PutRequest"`
		} `json:"RequestItems"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("ValidationException", "request body is not valid JSON")
	}
	for tableName, writes := range input.RequestItems {
		table, err := s.loadTable(tableName)
		if err != nil {
			return err
		}
		for _, write := range writes {
			switch {
			case write.PutRequest != nil:
				existing, err := s.loadItem(table, map[string]any{table.HashKey: write.PutRequest.Item[table.HashKey]})
				if err != nil {
					return err
				}
				if err := s.putStoredItem(table, write.PutRequest.Item); err != nil {
					return err
				}
				_ = s.emitStreamRecord(table, existing, write.PutRequest.Item, ternary(existing == nil, "INSERT", "MODIFY"))
			case write.DeleteRequest != nil:
				existing, err := s.loadItem(table, write.DeleteRequest.Key)
				if err != nil {
					return err
				}
				if err := s.deleteItem(table, write.DeleteRequest.Key); err != nil {
					return err
				}
				_ = s.emitStreamRecord(table, existing, nil, "REMOVE")
			default:
				return badRequest("ValidationException", "batch write request must contain PutRequest or DeleteRequest")
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"UnprocessedItems": map[string]any{}})
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
	if table.StreamEnabled && s.streams != nil {
		_ = s.streams.DeleteStream(input.TableName)
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

func (s *Service) putTable(record tableRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(tablesBucket, record.TableName, raw); err != nil {
		return internal(err)
	}
	return nil
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

func tableNameFromARN(arn string) string {
	const marker = ":table/"
	if idx := strings.Index(arn, marker); idx >= 0 {
		return arn[idx+len(marker):]
	}
	return arn
}

func (s *Service) emitStreamRecord(table tableRecord, oldImage, newImage map[string]any, eventName string) error {
	if !table.StreamEnabled || s.streams == nil {
		return nil
	}
	keys := map[string]any{}
	if newImage != nil {
		keys[table.HashKey] = newImage[table.HashKey]
	} else if oldImage != nil {
		keys[table.HashKey] = oldImage[table.HashKey]
	}
	return s.streams.AddRecord(table.TableName, keys, oldImage, newImage, eventName)
}

func (s *Service) putStoredItem(table tableRecord, item map[string]any) error {
	keyRaw, err := s.tableKeyRaw(table, item)
	if err != nil {
		return err
	}
	itemRaw, err := json.Marshal(item)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(itemsBucket, table.TableName+"|"+string(keyRaw), itemRaw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) loadItem(table tableRecord, key map[string]any) (map[string]any, error) {
	keyRaw, err := s.tableKeyRaw(table, key)
	if err != nil {
		return nil, err
	}
	itemRaw, err := s.metadata.Get(itemsBucket, table.TableName+"|"+string(keyRaw))
	if err != nil {
		return nil, internal(err)
	}
	if itemRaw == nil {
		return nil, nil
	}
	var item map[string]any
	if err := json.Unmarshal(itemRaw, &item); err != nil {
		return nil, internal(err)
	}
	return item, nil
}

func (s *Service) deleteItem(table tableRecord, key map[string]any) error {
	keyRaw, err := s.tableKeyRaw(table, key)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(itemsBucket, table.TableName+"|"+string(keyRaw)); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) listItems(table tableRecord) ([]map[string]any, error) {
	var items []map[string]any
	if err := s.metadata.Scan(itemsBucket, table.TableName+"|", func(_, v []byte) error {
		var item map[string]any
		if err := json.Unmarshal(v, &item); err != nil {
			return nil
		}
		items = append(items, item)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool {
		return itemSortKey(items[i]) < itemSortKey(items[j])
	})
	return items, nil
}

func (s *Service) tableKeyRaw(table tableRecord, attrs map[string]any) ([]byte, error) {
	keyValue, ok := attrs[table.HashKey]
	if !ok {
		return nil, badRequest("ValidationException", "item is missing HASH key")
	}
	keyRaw, err := json.Marshal(keyValue)
	if err != nil {
		return nil, internal(err)
	}
	return keyRaw, nil
}

func parseKeyCondition(expression string, names map[string]string, values map[string]any) (string, any, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return "", nil, badRequest("ValidationException", "KeyConditionExpression is required")
	}
	parts := strings.Split(expression, "=")
	if len(parts) != 2 {
		return "", nil, &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "only HASH key equality queries are supported"}
	}
	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])
	if names[left] != "" {
		left = names[left]
	}
	value, ok := values[right]
	if !ok {
		return "", nil, badRequest("ValidationException", "ExpressionAttributeValues entry is required")
	}
	return left, value, nil
}

func applyUpdateExpression(item map[string]any, expression string, names map[string]string, values map[string]any) error {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return badRequest("ValidationException", "UpdateExpression is required")
	}
	upper := strings.ToUpper(expression)
	setIdx := strings.Index(upper, "SET ")
	removeIdx := strings.Index(upper, "REMOVE ")
	if setIdx == -1 && removeIdx == -1 {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "only SET and REMOVE update expressions are supported"}
	}

	if setIdx >= 0 {
		start := setIdx + len("SET ")
		end := len(expression)
		if removeIdx > setIdx {
			end = removeIdx
		}
		assignments := splitCSV(expression[start:end])
		for _, assignment := range assignments {
			parts := strings.SplitN(assignment, "=", 2)
			if len(parts) != 2 {
				return badRequest("ValidationException", "invalid SET update expression")
			}
			name := resolveAttributeToken(strings.TrimSpace(parts[0]), names)
			valueToken := strings.TrimSpace(parts[1])
			value, ok := values[valueToken]
			if !ok {
				return badRequest("ValidationException", "missing ExpressionAttributeValues entry for "+valueToken)
			}
			item[name] = value
		}
	}

	if removeIdx >= 0 {
		start := removeIdx + len("REMOVE ")
		end := len(expression)
		if setIdx > removeIdx {
			end = setIdx
		}
		removals := splitCSV(expression[start:end])
		for _, removal := range removals {
			delete(item, resolveAttributeToken(strings.TrimSpace(removal), names))
		}
	}
	return nil
}

func resolveAttributeToken(token string, names map[string]string) string {
	if names[token] != "" {
		return names[token]
	}
	return token
}

func splitCSV(input string) []string {
	parts := strings.Split(input, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func itemSortKey(item map[string]any) string {
	raw, _ := json.Marshal(item)
	return string(raw)
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func ternary(cond bool, ifTrue, ifFalse string) string {
	if cond {
		return ifTrue
	}
	return ifFalse
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
