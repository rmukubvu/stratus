package dynamodb

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type attributeDefinition struct {
	AttributeName string `json:"AttributeName"`
	AttributeType string `json:"AttributeType"`
}

type keySchemaElement struct {
	AttributeName string `json:"AttributeName"`
	KeyType       string `json:"KeyType"`
}

type projection struct {
	ProjectionType   string   `json:"ProjectionType"`
	NonKeyAttributes []string `json:"NonKeyAttributes,omitempty"`
}

type throughput struct {
	ReadCapacityUnits  int64 `json:"ReadCapacityUnits"`
	WriteCapacityUnits int64 `json:"WriteCapacityUnits"`
}

// keyDef is a primary key or index key: a partition key and an optional
// sort key.
type keyDef struct {
	Hash  string `json:"hash"`
	Range string `json:"range,omitempty"`
}

func (k keyDef) attributes() []string {
	if k.Range == "" {
		return []string{k.Hash}
	}
	return []string{k.Hash, k.Range}
}

func (k keyDef) schema() []keySchemaElement {
	out := []keySchemaElement{{AttributeName: k.Hash, KeyType: "HASH"}}
	if k.Range != "" {
		out = append(out, keySchemaElement{AttributeName: k.Range, KeyType: "RANGE"})
	}
	return out
}

// secondaryIndex is a global or local secondary index.
type secondaryIndex struct {
	Name       string      `json:"name"`
	Global     bool        `json:"global"`
	Key        keyDef      `json:"key"`
	Projection projection  `json:"projection"`
	Throughput *throughput `json:"throughput,omitempty"`
}

// projects reports whether the index holds attribute name of an item.
func (ix secondaryIndex) projects(name string, table keyDef) bool {
	switch {
	case name == ix.Key.Hash || name == ix.Key.Range || name == table.Hash || name == table.Range:
		return true
	case ix.Projection.ProjectionType == "ALL":
		return true
	case ix.Projection.ProjectionType == "INCLUDE":
		for _, a := range ix.Projection.NonKeyAttributes {
			if a == name {
				return true
			}
		}
	}
	return false
}

// tableRecord is a stored table. The hash_key fields keep the layout older
// Stratus versions (and the CloudFormation provider) wrote.
type tableRecord struct {
	AttributeDefinitions []attributeDefinition `json:"attribute_definitions"`
	BillingMode          string                `json:"billing_mode"`
	CreatedAt            time.Time             `json:"created_at"`
	HashKey              string                `json:"hash_key"`
	HashKeyType          string                `json:"hash_key_type"`
	RangeKey             string                `json:"range_key,omitempty"`
	Throughput           *throughput           `json:"throughput,omitempty"`
	Indexes              []secondaryIndex      `json:"indexes,omitempty"`
	StreamArn            string                `json:"stream_arn,omitempty"`
	StreamEnabled        bool                  `json:"stream_enabled,omitempty"`
	StreamViewType       string                `json:"stream_view_type,omitempty"`
	TableName            string                `json:"table_name"`
	TableStatus          string                `json:"table_status"`
	TableID              string                `json:"table_id,omitempty"`
	TableClass           string                `json:"table_class,omitempty"`
	DeletionProtection   bool                  `json:"deletion_protection,omitempty"`
	TTLAttribute         string                `json:"ttl_attribute,omitempty"`
	TTLEnabled           bool                  `json:"ttl_enabled,omitempty"`
	PITREnabled          bool                  `json:"pitr_enabled,omitempty"`
	Tags                 []tagEntry            `json:"tags,omitempty"`
}

type tagEntry struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

func (t tableRecord) key() keyDef { return keyDef{Hash: t.HashKey, Range: t.RangeKey} }

func (t tableRecord) index(name string) (secondaryIndex, bool) {
	for _, ix := range t.Indexes {
		if ix.Name == name {
			return ix, true
		}
	}
	return secondaryIndex{}, false
}

func (t tableRecord) attributeType(name string) string {
	for _, d := range t.AttributeDefinitions {
		if d.AttributeName == name {
			return d.AttributeType
		}
	}
	if name == t.HashKey {
		return t.HashKeyType // records written before attribute definitions were stored
	}
	return ""
}

var tableNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,255}$`)

func validateTableName(name string) error {
	if name == "" {
		return validation("1 validation error detected: Value null at 'tableName' failed to satisfy constraint: Member must not be null")
	}
	if !tableNamePattern.MatchString(name) {
		return validation(fmt.Sprintf("1 validation error detected: Value '%s' at 'tableName' failed to satisfy constraint: "+
			"Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+", name))
	}
	return nil
}

// parseKeySchema validates a KeySchema: a HASH element, optionally followed
// by a RANGE element, each defined in AttributeDefinitions as S, N or B.
func parseKeySchema(schema []keySchemaElement, defs map[string]string, what string) (keyDef, error) {
	if len(schema) == 0 || len(schema) > 2 {
		return keyDef{}, validation(fmt.Sprintf("1 validation error detected: Value '%v' at '%s' failed to satisfy constraint: Member must have length less than or equal to 2", schema, what))
	}
	var k keyDef
	for i, e := range schema {
		switch {
		case i == 0 && e.KeyType == "HASH":
			k.Hash = e.AttributeName
		case i == 1 && e.KeyType == "RANGE":
			k.Range = e.AttributeName
		case e.KeyType == "HASH" || e.KeyType == "RANGE":
			return keyDef{}, validation("Invalid KeySchema: The first KeySchemaElement is not a HASH key type")
		default:
			return keyDef{}, validation(fmt.Sprintf("1 validation error detected: Value '%s' at '%s.%d.member.keyType' failed to satisfy constraint: Member must satisfy enum value set: [HASH, RANGE]", e.KeyType, what, i+1))
		}
	}
	if k.Hash == k.Range {
		return keyDef{}, validation("Both the Hash Key and the Range Key element in the KeySchema have the same name")
	}
	for _, name := range k.attributes() {
		t, ok := defs[name]
		if !ok {
			return keyDef{}, validation("One or more parameter values were invalid: Some index key attributes are not defined in AttributeDefinitions. Keys: [" + strings.Join(k.attributes(), ", ") + "], AttributeDefinitions: [" + strings.Join(sortedKeys(defs), ", ") + "]")
		}
		if t != "S" && t != "N" && t != "B" {
			return keyDef{}, validation(fmt.Sprintf("1 validation error detected: Value '%s' at 'attributeDefinitions' failed to satisfy constraint: Member must satisfy enum value set: [B, N, S]", t))
		}
	}
	return k, nil
}

func validateProjection(p projection) error {
	switch p.ProjectionType {
	case "ALL", "KEYS_ONLY":
		if len(p.NonKeyAttributes) > 0 {
			return validation("One or more parameter values were invalid: ProjectionType is " + p.ProjectionType + ", but NonKeyAttributes is specified")
		}
	case "INCLUDE":
		if len(p.NonKeyAttributes) == 0 {
			return validation("One or more parameter values were invalid: ProjectionType is INCLUDE, but NonKeyAttributes is not specified")
		}
	case "":
		return validation("One or more parameter values were invalid: Unknown ProjectionType: null")
	default:
		return validation("One or more parameter values were invalid: Unknown ProjectionType: " + p.ProjectionType)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
