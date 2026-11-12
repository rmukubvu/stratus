package dynamodb

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Items are stored under "<table>|<partition>|<sort>", each key part in its
// canonical encoding, so a partition is one key prefix.

const maxItemSize = 400 * 1024

func partitionPrefix(table, hash string) string { return table + "|" + hash + "|" }

// storageKey names the item with the given primary key attributes. It
// refuses a key that does not match the table's key schema.
func storageKey(t tableRecord, key item) (string, error) {
	k := t.key()
	for _, name := range k.attributes() {
		v, ok := key[name]
		if !ok {
			return "", validation("The provided key element does not match the schema")
		}
		typ, _, _ := attrType(v)
		if want := t.attributeType(name); want != "" && typ != want {
			return "", validation("The provided key element does not match the schema")
		}
		if typ == "S" || typ == "B" {
			if size, _ := sizeOf(v); size == 0 {
				return "", validation(fmt.Sprintf("One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an empty %s value. Key: %s",
					map[string]string{"S": "string", "B": "binary"}[typ], name))
			}
		}
	}
	rangePart := ""
	if k.Range != "" {
		rangePart = canonicalKey(key[k.Range])
	}
	return partitionPrefix(t.TableName, canonicalKey(key[k.Hash])) + rangePart, nil
}

// keyOnly reduces it to its primary key attributes.
func keyOnly(t tableRecord, it item) item {
	out := item{}
	for _, name := range t.key().attributes() {
		if v, ok := it[name]; ok {
			out[name] = v
		}
	}
	return out
}

// checkKeyArgument validates a Key parameter: exactly the key attributes.
func checkKeyArgument(t tableRecord, key item) error {
	if len(key) != len(t.key().attributes()) {
		return validation("The provided key element does not match the schema")
	}
	for name, v := range key {
		if err := validateValue(v); err != nil {
			return validation(err.Error())
		}
		if !contains(t.key().attributes(), name) {
			return validation("The provided key element does not match the schema")
		}
	}
	return nil
}

// validateItem checks an item before it is written: well-formed values,
// key attributes of the declared types, index keys of their declared types
// and the 400 KB limit.
func validateItem(t tableRecord, it item) error {
	for _, v := range it {
		if err := validateValue(v); err != nil {
			return valueError("One or more parameter values were invalid: ", errors.New(strings.TrimPrefix(err.Error(), "One or more parameter values were invalid: ")))
		}
	}
	for _, name := range t.key().attributes() {
		v, ok := it[name]
		if !ok {
			return validation("One or more parameter values were invalid: Missing the key " + name + " in the item")
		}
		typ, _, _ := attrType(v)
		if want := t.attributeType(name); typ != want {
			return validation(fmt.Sprintf("One or more parameter values were invalid: Type mismatch for key %s expected: %s actual: %s", name, want, typ))
		}
	}
	for _, ix := range t.Indexes {
		for _, name := range ix.Key.attributes() {
			v, ok := it[name]
			if !ok {
				continue
			}
			typ, _, _ := attrType(v)
			if want := t.attributeType(name); want != "" && typ != want {
				return validation(fmt.Sprintf("One or more parameter values were invalid: Type mismatch for Index Key %s Expected: %s Actual: %s IndexName: %s", name, want, typ, ix.Name))
			}
			if size, _ := sizeOf(v); (typ == "S" || typ == "B") && size == 0 {
				return validation(fmt.Sprintf("One or more parameter values are not valid. A value specified for a secondary index key is not supported. The AttributeValue for a key attribute cannot contain an empty %s value. IndexName: %s, IndexKey: %s",
					map[string]string{"S": "string", "B": "binary"}[typ], ix.Name, name))
			}
		}
	}
	if itemSize(it) > maxItemSize {
		return validation("Item size has exceeded the maximum allowed size")
	}
	return nil
}

func (s *Service) loadItem(t tableRecord, key item) (item, error) {
	k, err := storageKey(t, key)
	if err != nil {
		return nil, err
	}
	raw, err := s.metadata.Get(itemsBucket, k)
	if err != nil {
		return nil, internal(err)
	}
	if raw == nil {
		return nil, nil
	}
	var it item
	if err := json.Unmarshal(raw, &it); err != nil {
		return nil, internal(err)
	}
	return it, nil
}

func (s *Service) storeItem(t tableRecord, it item) error {
	k, err := storageKey(t, it)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(it)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(itemsBucket, k, raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) removeItem(t tableRecord, key item) error {
	k, err := storageKey(t, key)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(itemsBucket, k); err != nil {
		return internal(err)
	}
	return nil
}

// scanItems returns the items stored under prefix (a table or a partition).
func (s *Service) scanItems(prefix string) ([]item, error) {
	var out []item
	err := s.metadata.Scan(itemsBucket, prefix, func(_, v []byte) error {
		var it item
		if err := json.Unmarshal(v, &it); err == nil {
			out = append(out, it)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

// tableItems returns every item in the table, expiring TTL'd items first.
func (s *Service) tableItems(t tableRecord) ([]item, error) {
	items, err := s.scanItems(t.TableName + "|")
	if err != nil {
		return nil, err
	}
	return s.dropExpired(t, items)
}

// dropExpired deletes items whose TTL attribute is in the past, as
// DynamoDB's TTL sweeper would have, and returns the rest.
func (s *Service) dropExpired(t tableRecord, items []item) ([]item, error) {
	if !t.TTLEnabled || t.TTLAttribute == "" {
		return items, nil
	}
	now := s.now().Unix()
	kept := items[:0]
	for _, it := range items {
		if n, ok := numberOf(it[t.TTLAttribute]); ok && n.IsInt() && n.Num().IsInt64() {
			if exp := n.Num().Int64(); exp > 0 && exp < now && exp > now-5*365*24*3600 {
				if err := s.removeItem(t, it); err != nil {
					return nil, err
				}
				s.emitStream(t, it, nil, "REMOVE")
				continue
			}
		}
		kept = append(kept, it)
	}
	return kept, nil
}

// sortByKey orders items by partition then sort key, the order Query
// returns and Scan pages through.
func sortByKey(items []item, k keyDef) {
	sort.SliceStable(items, func(i, j int) bool {
		hi, hj := canonicalKey(items[i][k.Hash]), canonicalKey(items[j][k.Hash])
		if hi != hj {
			return hi < hj
		}
		if k.Range == "" {
			return false
		}
		c, ok := compareValues(items[i][k.Range], items[j][k.Range])
		return ok && c < 0
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortStrings(s []string) { sort.Strings(s) }
