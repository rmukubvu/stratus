package dynamodb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
)

type indexInput struct {
	IndexName             string             `json:"IndexName"`
	KeySchema             []keySchemaElement `json:"KeySchema"`
	Projection            projection         `json:"Projection"`
	ProvisionedThroughput *throughput        `json:"ProvisionedThroughput"`
}

type streamSpecification struct {
	StreamEnabled  bool   `json:"StreamEnabled"`
	StreamViewType string `json:"StreamViewType"`
}

type createTableInput struct {
	TableName                 string                `json:"TableName"`
	AttributeDefinitions      []attributeDefinition `json:"AttributeDefinitions"`
	KeySchema                 []keySchemaElement    `json:"KeySchema"`
	BillingMode               string                `json:"BillingMode"`
	ProvisionedThroughput     *throughput           `json:"ProvisionedThroughput"`
	GlobalSecondaryIndexes    []indexInput          `json:"GlobalSecondaryIndexes"`
	LocalSecondaryIndexes     []indexInput          `json:"LocalSecondaryIndexes"`
	StreamSpecification       *streamSpecification  `json:"StreamSpecification"`
	Tags                      []tagEntry            `json:"Tags"`
	TableClass                string                `json:"TableClass"`
	DeletionProtectionEnabled bool                  `json:"DeletionProtectionEnabled"`
}

func (s *Service) createTable(r *http.Request) (any, error) {
	var in createTableInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := validateTableName(in.TableName); err != nil {
		return nil, err
	}
	if existing, err := s.findTable(in.TableName); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceInUseException", Message: "Table already exists: " + in.TableName}
	}
	defs := map[string]string{}
	for _, d := range in.AttributeDefinitions {
		if _, dup := defs[d.AttributeName]; dup {
			return nil, validation("Cannot have two attributes with the same name")
		}
		defs[d.AttributeName] = d.AttributeType
	}
	key, err := parseKeySchema(in.KeySchema, defs, "keySchema")
	if err != nil {
		return nil, err
	}
	t := tableRecord{
		AttributeDefinitions: in.AttributeDefinitions, CreatedAt: s.now().UTC(), HashKey: key.Hash, HashKeyType: defs[key.Hash],
		RangeKey: key.Range, TableName: in.TableName, TableStatus: "ACTIVE", TableID: uuid.NewString(),
		TableClass: in.TableClass, DeletionProtection: in.DeletionProtectionEnabled, Tags: in.Tags,
	}
	if err := applyBilling(&t, in.BillingMode, in.ProvisionedThroughput, true); err != nil {
		return nil, err
	}
	used := map[string]bool{key.Hash: true, key.Range: key.Range != ""}
	if len(in.LocalSecondaryIndexes) > 0 && key.Range == "" {
		return nil, validation("One or more parameter values were invalid: Table KeySchema does not have a range key, which is required when specifying a LocalSecondaryIndex")
	}
	for _, spec := range []struct {
		list   []indexInput
		global bool
	}{{in.GlobalSecondaryIndexes, true}, {in.LocalSecondaryIndexes, false}} {
		for _, ix := range spec.list {
			idx, err := newIndex(ix, spec.global, defs, t)
			if err != nil {
				return nil, err
			}
			for _, name := range idx.Key.attributes() {
				used[name] = true
			}
			t.Indexes = append(t.Indexes, idx)
		}
	}
	for name := range defs {
		if !used[name] {
			return nil, validation("One or more parameter values were invalid: Number of attributes in KeySchema does not exactly match number of attributes defined in AttributeDefinitions")
		}
	}
	if in.StreamSpecification != nil {
		if err := s.applyStream(&t, *in.StreamSpecification); err != nil {
			return nil, err
		}
	}
	if err := s.saveTable(t); err != nil {
		return nil, err
	}
	return map[string]any{"TableDescription": s.describe(t)}, nil
}

// newIndex validates one secondary index definition.
func newIndex(in indexInput, global bool, defs map[string]string, t tableRecord) (secondaryIndex, error) {
	if in.IndexName == "" || !tableNamePattern.MatchString(in.IndexName) {
		return secondaryIndex{}, validation("1 validation error detected: Value '" + in.IndexName + "' at 'indexName' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+")
	}
	if _, dup := t.index(in.IndexName); dup {
		return secondaryIndex{}, validation("One or more parameter values were invalid: Duplicate index name: " + in.IndexName)
	}
	key, err := parseKeySchema(in.KeySchema, defs, "indexKeySchema")
	if err != nil {
		return secondaryIndex{}, err
	}
	if !global && key.Hash != t.HashKey {
		return secondaryIndex{}, validation("One or more parameter values were invalid: Index KeySchema does not have the same leading hash key as table KeySchema for index: " + in.IndexName)
	}
	if !global && key.Range == "" {
		return secondaryIndex{}, validation("One or more parameter values were invalid: Index KeySchema does not have a range key for index: " + in.IndexName)
	}
	if err := validateProjection(in.Projection); err != nil {
		return secondaryIndex{}, err
	}
	ix := secondaryIndex{Name: in.IndexName, Global: global, Key: key, Projection: in.Projection}
	if global {
		switch {
		case t.BillingMode == "PROVISIONED" && in.ProvisionedThroughput == nil:
			return secondaryIndex{}, validation("One or more parameter values were invalid: ProvisionedThroughput must be specified for index: " + in.IndexName)
		case t.BillingMode == "PAY_PER_REQUEST" && in.ProvisionedThroughput != nil:
			return secondaryIndex{}, validation("One or more parameter values were invalid: ProvisionedThroughput should not be specified for index: " + in.IndexName + " when BillingMode is PAY_PER_REQUEST")
		}
		ix.Throughput = in.ProvisionedThroughput
	}
	return ix, nil
}

func applyBilling(t *tableRecord, mode string, tp *throughput, creating bool) error {
	if mode == "" {
		if creating {
			mode = "PROVISIONED"
		} else {
			mode = t.BillingMode
		}
	}
	switch mode {
	case "PROVISIONED":
		if tp == nil && (creating || t.Throughput == nil) {
			return validation("One or more parameter values were invalid: ReadCapacityUnits and WriteCapacityUnits must both be specified when BillingMode is PROVISIONED")
		}
		if tp != nil {
			if tp.ReadCapacityUnits < 1 || tp.WriteCapacityUnits < 1 {
				return validation("1 validation error detected: Value '0' at 'provisionedThroughput.readCapacityUnits' failed to satisfy constraint: Member must have value greater than or equal to 1")
			}
			t.Throughput = tp
		}
	case "PAY_PER_REQUEST":
		if tp != nil && creating {
			return validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		t.Throughput = nil
	default:
		return validation("1 validation error detected: Value '" + mode + "' at 'billingMode' failed to satisfy constraint: Member must satisfy enum value set: [PROVISIONED, PAY_PER_REQUEST]")
	}
	t.BillingMode = mode
	return nil
}

func (s *Service) applyStream(t *tableRecord, spec streamSpecification) error {
	if !spec.StreamEnabled {
		if t.StreamEnabled && s.streams != nil {
			_ = s.streams.DeleteStream(t.TableName)
		}
		t.StreamEnabled, t.StreamViewType = false, ""
		return nil
	}
	switch spec.StreamViewType {
	case "NEW_IMAGE", "OLD_IMAGE", "NEW_AND_OLD_IMAGES", "KEYS_ONLY":
	default:
		return validation("One or more parameter values were invalid: StreamViewType is required when StreamEnabled is true")
	}
	if t.StreamEnabled {
		return validation("Table already has an enabled stream: TableName: " + t.TableName)
	}
	if s.streams == nil {
		return validation("DynamoDB Streams are not available")
	}
	arn, err := s.streams.EnsureStream(t.TableName, spec.StreamViewType)
	if err != nil {
		return err
	}
	t.StreamArn, t.StreamEnabled, t.StreamViewType = arn, true, spec.StreamViewType
	return nil
}

func (s *Service) findTable(name string) (*tableRecord, error) {
	raw, err := s.metadata.Get(tablesBucket, name)
	if err != nil {
		return nil, internal(err)
	}
	if raw == nil {
		return nil, nil
	}
	var t tableRecord
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, internal(err)
	}
	if t.BillingMode == "" {
		t.BillingMode = "PAY_PER_REQUEST"
	}
	return &t, nil
}

// loadTable returns the named table or ResourceNotFoundException.
func (s *Service) loadTable(name string) (tableRecord, error) {
	if name == "" {
		return tableRecord{}, validation("1 validation error detected: Value null at 'tableName' failed to satisfy constraint: Member must not be null")
	}
	if !strings.HasPrefix(name, "arn:") {
		if err := validateTableName(name); err != nil {
			return tableRecord{}, err
		}
	}
	t, err := s.findTable(tableNameFromARN(name))
	if err != nil {
		return tableRecord{}, err
	}
	if t == nil {
		return tableRecord{}, resourceNotFound()
	}
	return *t, nil
}

// loadTableForControl is loadTable for control-plane operations, whose
// not-found message names the table.
func (s *Service) loadTableForControl(name string) (tableRecord, error) {
	t, err := s.loadTable(name)
	if apiErr, ok := err.(*apierror.Error); ok && apiErr.Code == "ResourceNotFoundException" {
		return t, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException",
			Message: "Requested resource not found: Table: " + tableNameFromARN(name) + " not found"}
	}
	return t, err
}

func (s *Service) saveTable(t tableRecord) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(tablesBucket, t.TableName, raw); err != nil {
		return internal(err)
	}
	return nil
}

// describe renders a TableDescription, with live item counts and sizes.
func (s *Service) describe(t tableRecord) map[string]any {
	items, _ := s.scanItems(t.TableName + "|")
	size := 0
	for _, it := range items {
		size += itemSize(it)
	}
	tp := map[string]any{"NumberOfDecreasesToday": 0, "ReadCapacityUnits": 0, "WriteCapacityUnits": 0}
	if t.Throughput != nil {
		tp["ReadCapacityUnits"], tp["WriteCapacityUnits"] = t.Throughput.ReadCapacityUnits, t.Throughput.WriteCapacityUnits
	}
	out := map[string]any{
		"AttributeDefinitions":      t.AttributeDefinitions,
		"TableName":                 t.TableName,
		"KeySchema":                 t.key().schema(),
		"TableStatus":               t.TableStatus,
		"CreationDateTime":          float64(t.CreatedAt.UnixMilli()) / 1000,
		"ProvisionedThroughput":     tp,
		"TableSizeBytes":            size,
		"ItemCount":                 len(items),
		"TableArn":                  s.tableARN(t.TableName),
		"TableId":                   t.TableID,
		"DeletionProtectionEnabled": t.DeletionProtection,
	}
	if t.BillingMode == "PAY_PER_REQUEST" {
		out["BillingModeSummary"] = map[string]any{"BillingMode": "PAY_PER_REQUEST", "LastUpdateToPayPerRequestDateTime": float64(t.CreatedAt.UnixMilli()) / 1000}
	} else {
		out["BillingModeSummary"] = map[string]any{"BillingMode": "PROVISIONED"}
	}
	if t.TableClass != "" {
		out["TableClassSummary"] = map[string]any{"TableClass": t.TableClass}
	}
	var gsis, lsis []map[string]any
	for _, ix := range t.Indexes {
		count, bytes := 0, 0
		for _, it := range items {
			if indexHolds(ix, it) {
				count++
				bytes += itemSize(it)
			}
		}
		d := map[string]any{
			"IndexName": ix.Name, "KeySchema": ix.Key.schema(), "Projection": ix.Projection,
			"IndexSizeBytes": bytes, "ItemCount": count, "IndexArn": s.tableARN(t.TableName) + "/index/" + ix.Name,
		}
		if ix.Global {
			d["IndexStatus"] = "ACTIVE"
			itp := map[string]any{"NumberOfDecreasesToday": 0, "ReadCapacityUnits": 0, "WriteCapacityUnits": 0}
			if ix.Throughput != nil {
				itp["ReadCapacityUnits"], itp["WriteCapacityUnits"] = ix.Throughput.ReadCapacityUnits, ix.Throughput.WriteCapacityUnits
			}
			d["ProvisionedThroughput"] = itp
			gsis = append(gsis, d)
		} else {
			lsis = append(lsis, d)
		}
	}
	if gsis != nil {
		out["GlobalSecondaryIndexes"] = gsis
	}
	if lsis != nil {
		out["LocalSecondaryIndexes"] = lsis
	}
	if t.StreamEnabled {
		out["StreamSpecification"] = map[string]any{"StreamEnabled": true, "StreamViewType": t.StreamViewType}
		out["LatestStreamArn"] = t.StreamArn
		if i := len(t.StreamArn) - 23; i > 0 {
			out["LatestStreamLabel"] = t.StreamArn[i:]
		}
	}
	return out
}

// indexHolds reports whether an item appears in the index: it has every
// index key attribute.
func indexHolds(ix secondaryIndex, it item) bool {
	for _, name := range ix.Key.attributes() {
		if _, ok := it[name]; !ok {
			return false
		}
	}
	return true
}

type tableNameInput struct {
	TableName string `json:"TableName"`
}

func (s *Service) describeTable(r *http.Request) (any, error) {
	var in tableNameInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Table": s.describe(t)}, nil
}

type updateTableInput struct {
	TableName                   string                `json:"TableName"`
	AttributeDefinitions        []attributeDefinition `json:"AttributeDefinitions"`
	BillingMode                 string                `json:"BillingMode"`
	ProvisionedThroughput       *throughput           `json:"ProvisionedThroughput"`
	StreamSpecification         *streamSpecification  `json:"StreamSpecification"`
	DeletionProtectionEnabled   *bool                 `json:"DeletionProtectionEnabled"`
	TableClass                  string                `json:"TableClass"`
	GlobalSecondaryIndexUpdates []struct {
		Create *indexInput `json:"Create"`
		Update *struct {
			IndexName             string      `json:"IndexName"`
			ProvisionedThroughput *throughput `json:"ProvisionedThroughput"`
		} `json:"Update"`
		Delete *struct {
			IndexName string `json:"IndexName"`
		} `json:"Delete"`
	} `json:"GlobalSecondaryIndexUpdates"`
}

func (s *Service) updateTable(r *http.Request) (any, error) {
	var in updateTableInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	changed := false
	if in.BillingMode != "" || in.ProvisionedThroughput != nil {
		if in.BillingMode == "PAY_PER_REQUEST" && in.ProvisionedThroughput != nil {
			return nil, validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		if in.BillingMode == "" && t.BillingMode == "PAY_PER_REQUEST" && in.ProvisionedThroughput != nil {
			return nil, validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		if t.Throughput != nil && in.ProvisionedThroughput != nil && *t.Throughput == *in.ProvisionedThroughput && in.BillingMode == "" {
			return nil, validation("The provisioned throughput for the table will not change. The requested value equals the current value. Current ReadCapacityUnits provisioned for the table: " +
				fmt.Sprint(t.Throughput.ReadCapacityUnits) + ". Requested ReadCapacityUnits: " + fmt.Sprint(in.ProvisionedThroughput.ReadCapacityUnits) +
				". Current WriteCapacityUnits provisioned for the table: " + fmt.Sprint(t.Throughput.WriteCapacityUnits) + ". Requested WriteCapacityUnits: " +
				fmt.Sprint(in.ProvisionedThroughput.WriteCapacityUnits) + ". Refer to the Amazon DynamoDB Developer Guide for current limits and how to request higher limits.")
		}
		if err := applyBilling(&t, in.BillingMode, in.ProvisionedThroughput, false); err != nil {
			return nil, err
		}
		changed = true
	}
	defs := map[string]string{}
	for _, d := range t.AttributeDefinitions {
		defs[d.AttributeName] = d.AttributeType
	}
	for _, d := range in.AttributeDefinitions {
		if existing, ok := defs[d.AttributeName]; ok && existing != d.AttributeType {
			return nil, validation("Cannot change the type of an existing attribute: " + d.AttributeName)
		}
		if _, ok := defs[d.AttributeName]; !ok {
			defs[d.AttributeName] = d.AttributeType
			t.AttributeDefinitions = append(t.AttributeDefinitions, d)
		}
	}
	for _, u := range in.GlobalSecondaryIndexUpdates {
		switch {
		case u.Create != nil:
			ix, err := newIndex(*u.Create, true, defs, t)
			if err != nil {
				return nil, err
			}
			t.Indexes = append(t.Indexes, ix)
		case u.Delete != nil:
			i := indexPosition(t, u.Delete.IndexName, true)
			if i < 0 {
				return nil, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "Requested resource not found: Index: " + u.Delete.IndexName + " not found"}
			}
			t.Indexes = append(t.Indexes[:i], t.Indexes[i+1:]...)
		case u.Update != nil:
			i := indexPosition(t, u.Update.IndexName, true)
			if i < 0 {
				return nil, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "Requested resource not found: Index: " + u.Update.IndexName + " not found"}
			}
			t.Indexes[i].Throughput = u.Update.ProvisionedThroughput
		}
		changed = true
	}
	if in.StreamSpecification != nil {
		if err := s.applyStream(&t, *in.StreamSpecification); err != nil {
			return nil, err
		}
		changed = true
	}
	if in.DeletionProtectionEnabled != nil {
		t.DeletionProtection = *in.DeletionProtectionEnabled
		changed = true
	}
	if in.TableClass != "" {
		t.TableClass = in.TableClass
		changed = true
	}
	if !changed {
		return nil, validation("At least one of ProvisionedThroughput, BillingMode, UpdateStreamEnabled, GlobalSecondaryIndexUpdates or SSESpecification or ReplicaUpdates is required")
	}
	t.AttributeDefinitions = usedDefinitions(t)
	if err := s.saveTable(t); err != nil {
		return nil, err
	}
	return map[string]any{"TableDescription": s.describe(t)}, nil
}

// usedDefinitions keeps the attribute definitions some key still uses, so
// deleting an index drops its attributes as DynamoDB does.
func usedDefinitions(t tableRecord) []attributeDefinition {
	used := map[string]bool{}
	for _, name := range t.key().attributes() {
		used[name] = true
	}
	for _, ix := range t.Indexes {
		for _, name := range ix.Key.attributes() {
			used[name] = true
		}
	}
	var out []attributeDefinition
	for _, d := range t.AttributeDefinitions {
		if used[d.AttributeName] {
			out = append(out, d)
		}
	}
	return out
}

func indexPosition(t tableRecord, name string, global bool) int {
	for i, ix := range t.Indexes {
		if ix.Name == name && ix.Global == global {
			return i
		}
	}
	return -1
}

func (s *Service) deleteTable(r *http.Request) (any, error) {
	var in tableNameInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	if t.DeletionProtection {
		return nil, validation("Resource cannot be deleted as it is currently protected against deletion. Disable deletion protection first.")
	}
	desc := s.describe(t)
	desc["TableStatus"] = "DELETING"
	if err := s.metadata.Delete(tablesBucket, t.TableName); err != nil {
		return nil, internal(err)
	}
	if err := s.metadata.DeletePrefix(itemsBucket, t.TableName+"|"); err != nil {
		return nil, internal(err)
	}
	if t.StreamEnabled && s.streams != nil {
		_ = s.streams.DeleteStream(t.TableName)
	}
	return map[string]any{"TableDescription": desc}, nil
}

func (s *Service) listTables(r *http.Request) (any, error) {
	var in struct {
		ExclusiveStartTableName string `json:"ExclusiveStartTableName"`
		Limit                   *int   `json:"Limit"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	limit := 100
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 100 {
			return nil, validation(fmt.Sprintf("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100", *in.Limit))
		}
		limit = *in.Limit
	}
	var names []string
	if err := s.metadata.Scan(tablesBucket, "", func(k, _ []byte) error {
		if string(k) > in.ExclusiveStartTableName {
			names = append(names, string(k))
		}
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Strings(names)
	out := map[string]any{}
	if len(names) > limit {
		names = names[:limit]
		out["LastEvaluatedTableName"] = names[limit-1]
	}
	out["TableNames"] = append([]string{}, names...)
	return out, nil
}

func (s *Service) describeContinuousBackups(r *http.Request) (any, error) {
	var in tableNameInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ContinuousBackupsDescription": continuousBackups(t, s)}, nil
}

func continuousBackups(t tableRecord, s *Service) map[string]any {
	pitr := map[string]any{"PointInTimeRecoveryStatus": "DISABLED"}
	if t.PITREnabled {
		now := float64(s.now().Unix())
		pitr = map[string]any{"PointInTimeRecoveryStatus": "ENABLED", "EarliestRestorableDateTime": float64(t.CreatedAt.Unix()),
			"LatestRestorableDateTime": now, "RecoveryPeriodInDays": 35}
	}
	return map[string]any{"ContinuousBackupsStatus": "ENABLED", "PointInTimeRecoveryDescription": pitr}
}

func (s *Service) updateContinuousBackups(r *http.Request) (any, error) {
	var in struct {
		TableName                        string `json:"TableName"`
		PointInTimeRecoverySpecification *struct {
			PointInTimeRecoveryEnabled bool `json:"PointInTimeRecoveryEnabled"`
		} `json:"PointInTimeRecoverySpecification"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	if in.PointInTimeRecoverySpecification == nil {
		return nil, validation("1 validation error detected: Value null at 'pointInTimeRecoverySpecification' failed to satisfy constraint: Member must not be null")
	}
	t.PITREnabled = in.PointInTimeRecoverySpecification.PointInTimeRecoveryEnabled
	if err := s.saveTable(t); err != nil {
		return nil, err
	}
	return map[string]any{"ContinuousBackupsDescription": continuousBackups(t, s)}, nil
}

func (s *Service) describeTimeToLive(r *http.Request) (any, error) {
	var in tableNameInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	desc := map[string]any{"TimeToLiveStatus": "DISABLED"}
	if t.TTLEnabled {
		desc = map[string]any{"TimeToLiveStatus": "ENABLED", "AttributeName": t.TTLAttribute}
	}
	return map[string]any{"TimeToLiveDescription": desc}, nil
}

func (s *Service) updateTimeToLive(r *http.Request) (any, error) {
	var in struct {
		TableName               string `json:"TableName"`
		TimeToLiveSpecification *struct {
			AttributeName string `json:"AttributeName"`
			Enabled       bool   `json:"Enabled"`
		} `json:"TimeToLiveSpecification"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.loadTableForControl(in.TableName)
	if err != nil {
		return nil, err
	}
	spec := in.TimeToLiveSpecification
	if spec == nil || spec.AttributeName == "" {
		return nil, validation("1 validation error detected: Value null at 'timeToLiveSpecification.attributeName' failed to satisfy constraint: Member must not be null")
	}
	switch {
	case spec.Enabled && t.TTLEnabled:
		return nil, validation("TimeToLive is already enabled")
	case !spec.Enabled && !t.TTLEnabled:
		return nil, validation("TimeToLive is already disabled")
	case !spec.Enabled && spec.AttributeName != t.TTLAttribute:
		return nil, validation("TimeToLive is active on a different AttributeName: current AttributeName is " + t.TTLAttribute)
	}
	t.TTLEnabled, t.TTLAttribute = spec.Enabled, spec.AttributeName
	if err := s.saveTable(t); err != nil {
		return nil, err
	}
	return map[string]any{"TimeToLiveSpecification": map[string]any{"AttributeName": spec.AttributeName, "Enabled": spec.Enabled}}, nil
}

type tagInput struct {
	ResourceArn string     `json:"ResourceArn"`
	Tags        []tagEntry `json:"Tags"`
	TagKeys     []string   `json:"TagKeys"`
	NextToken   string     `json:"NextToken"`
}

func (s *Service) taggedTable(arn string) (tableRecord, error) {
	if arn == "" {
		return tableRecord{}, validation("1 validation error detected: Value null at 'resourceArn' failed to satisfy constraint: Member must not be null")
	}
	t, err := s.findTable(tableNameFromARN(arn))
	if err != nil {
		return tableRecord{}, err
	}
	if t == nil || s.tableARN(t.TableName) != arn {
		return tableRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "Requested resource not found: ResourcArn: " + arn + " not found"}
	}
	return *t, nil
}

func (s *Service) tagResource(r *http.Request) (any, error) {
	var in tagInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.taggedTable(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	for _, tag := range in.Tags {
		replaced := false
		for i := range t.Tags {
			if t.Tags[i].Key == tag.Key {
				t.Tags[i].Value, replaced = tag.Value, true
			}
		}
		if !replaced {
			t.Tags = append(t.Tags, tag)
		}
	}
	if len(t.Tags) > 50 {
		return nil, validation("One or more parameter values were invalid: The number of tags exceeds the limit of 50")
	}
	return map[string]any{}, s.saveTable(t)
}

func (s *Service) untagResource(r *http.Request) (any, error) {
	var in tagInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.taggedTable(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	var kept []tagEntry
	for _, tag := range t.Tags {
		if !contains(in.TagKeys, tag.Key) {
			kept = append(kept, tag)
		}
	}
	t.Tags = kept
	return map[string]any{}, s.saveTable(t)
}

func (s *Service) listTagsOfResource(r *http.Request) (any, error) {
	var in tagInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, err := s.taggedTable(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Tags": append([]tagEntry{}, t.Tags...)}, nil
}

func (s *Service) describeEndpoints(*http.Request) (any, error) {
	return map[string]any{"Endpoints": []map[string]any{{"Address": "dynamodb." + s.scope.Region + ".amazonaws.com", "CachePeriodInMinutes": 1440}}}, nil
}

func (s *Service) describeLimits(*http.Request) (any, error) {
	return map[string]any{"AccountMaxReadCapacityUnits": 80000, "AccountMaxWriteCapacityUnits": 80000,
		"TableMaxReadCapacityUnits": 40000, "TableMaxWriteCapacityUnits": 40000}, nil
}
