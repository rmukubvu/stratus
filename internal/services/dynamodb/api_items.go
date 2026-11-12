package dynamodb

import (
	"encoding/json"
	"net/http"
)

// writeCommon are the parameters every single-item write takes.
type writeCommon struct {
	TableName                           string            `json:"TableName"`
	ConditionExpression                 string            `json:"ConditionExpression"`
	ExpressionAttributeNames            map[string]string `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues           map[string]any    `json:"ExpressionAttributeValues"`
	ReturnValues                        string            `json:"ReturnValues"`
	ReturnConsumedCapacity              string            `json:"ReturnConsumedCapacity"`
	ReturnItemCollectionMetrics         string            `json:"ReturnItemCollectionMetrics"`
	ReturnValuesOnConditionCheckFailure string            `json:"ReturnValuesOnConditionCheckFailure"`
	Expected                            json.RawMessage   `json:"Expected"`
}

func (w writeCommon) validateLegacy() error {
	if len(w.Expected) > 0 && string(w.Expected) != "null" {
		if w.ConditionExpression != "" {
			return validation("Can not use both expression and non-expression parameters in the same request: Non-expression parameters: {Expected} Expression parameters: {ConditionExpression}")
		}
		return validation("The legacy Expected parameter is not supported; use ConditionExpression")
	}
	return validReturnConsumed(w.ReturnConsumedCapacity)
}

// writeOutcome is a single-item write ready to commit: the item before and
// after (nil when absent).
type writeOutcome struct {
	table    tableRecord
	key      item
	old, new item
}

func (s *Service) commitWrite(o writeOutcome) error {
	if o.new == nil {
		if o.old == nil {
			return nil
		}
		if err := s.removeItem(o.table, o.key); err != nil {
			return err
		}
		s.emitStream(o.table, o.old, nil, "REMOVE")
		return nil
	}
	if err := s.storeItem(o.table, o.new); err != nil {
		return err
	}
	event := "MODIFY"
	if o.old == nil {
		event = "INSERT"
	}
	s.emitStream(o.table, o.old, o.new, event)
	return nil
}

// currentItem loads the item a write targets, treating an expired TTL item
// as absent.
func (s *Service) currentItem(t tableRecord, key item) (item, error) {
	it, err := s.loadItem(t, key)
	if err != nil || it == nil {
		return it, err
	}
	kept, err := s.dropExpired(t, []item{it})
	if err != nil || len(kept) == 0 {
		return nil, err
	}
	return it, nil
}

// ---- PutItem ----

type putInput struct {
	writeCommon
	Item item `json:"Item"`
}

func (s *Service) preparePut(in putInput) (writeOutcome, error) {
	if err := in.validateLegacy(); err != nil {
		return writeOutcome{}, err
	}
	t, err := s.loadTable(in.TableName)
	if err != nil {
		return writeOutcome{}, err
	}
	if in.Item == nil {
		return writeOutcome{}, validation("1 validation error detected: Value null at 'item' failed to satisfy constraint: Member must not be null")
	}
	if err := validateItem(t, in.Item); err != nil {
		return writeOutcome{}, err
	}
	switch in.ReturnValues {
	case "", "NONE", "ALL_OLD":
	default:
		return writeOutcome{}, validation("Return values set to invalid value")
	}
	c, err := compileExpressions(expressionInputs{names: in.ExpressionAttributeNames, values: in.ExpressionAttributeValues, condition: in.ConditionExpression})
	if err != nil {
		return writeOutcome{}, err
	}
	key := keyOnly(t, in.Item)
	old, err := s.currentItem(t, key)
	if err != nil {
		return writeOutcome{}, err
	}
	if ok, err := check(c.condition, old, "ConditionExpression"); err != nil {
		return writeOutcome{}, err
	} else if !ok {
		return writeOutcome{}, conditionFailed()
	}
	return writeOutcome{table: t, key: key, old: old, new: cloneItem(in.Item)}, nil
}

func (s *Service) putItem(r *http.Request) (any, error) {
	var in putInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	o, err := s.preparePut(in)
	if err != nil {
		return nil, err
	}
	if err := s.commitWrite(o); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if in.ReturnValues == "ALL_OLD" && o.old != nil {
		out["Attributes"] = o.old
	}
	if cc := consumed(o.table.TableName, in.ReturnConsumedCapacity, writeUnits(maxInt(itemSize(o.new), itemSize(o.old))), false); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- GetItem ----

type getInput struct {
	TableName                string            `json:"TableName"`
	Key                      item              `json:"Key"`
	ProjectionExpression     string            `json:"ProjectionExpression"`
	ExpressionAttributeNames map[string]string `json:"ExpressionAttributeNames"`
	AttributesToGet          []string          `json:"AttributesToGet"`
	ConsistentRead           bool              `json:"ConsistentRead"`
	ReturnConsumedCapacity   string            `json:"ReturnConsumedCapacity"`
}

func (s *Service) readOne(in getInput) (tableRecord, item, error) {
	if err := validReturnConsumed(in.ReturnConsumedCapacity); err != nil {
		return tableRecord{}, nil, err
	}
	t, err := s.loadTable(in.TableName)
	if err != nil {
		return tableRecord{}, nil, err
	}
	if err := checkKeyArgument(t, in.Key); err != nil {
		return tableRecord{}, nil, err
	}
	paths, err := projectionOf(in.ProjectionExpression, in.AttributesToGet, in.ExpressionAttributeNames)
	if err != nil {
		return tableRecord{}, nil, err
	}
	it, err := s.currentItem(t, in.Key)
	if err != nil {
		return tableRecord{}, nil, err
	}
	return t, project(it, paths), nil
}

// projectionOf compiles a ProjectionExpression, or the legacy
// AttributesToGet list; nil means every attribute.
func projectionOf(expr string, attributes []string, names map[string]string) ([]docPath, error) {
	if expr != "" && len(attributes) > 0 {
		return nil, validation("Can not use both expression and non-expression parameters in the same request: Non-expression parameters: {AttributesToGet} Expression parameters: {ProjectionExpression}")
	}
	if len(attributes) > 0 {
		var paths []docPath
		for _, a := range attributes {
			paths = append(paths, docPath{{name: a}})
		}
		return paths, nil
	}
	c, err := compileExpressions(expressionInputs{names: names, projection: expr})
	if err != nil {
		return nil, err
	}
	return c.projection, nil
}

func (s *Service) getItem(r *http.Request) (any, error) {
	var in getInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	t, it, err := s.readOne(in)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if it != nil {
		out["Item"] = it
	}
	if cc := consumed(t.TableName, in.ReturnConsumedCapacity, readUnits(itemSize(it), in.ConsistentRead), true); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- UpdateItem ----

type updateInput struct {
	writeCommon
	Key              item            `json:"Key"`
	UpdateExpression string          `json:"UpdateExpression"`
	AttributeUpdates json.RawMessage `json:"AttributeUpdates"`
}

func (s *Service) prepareUpdate(in updateInput) (writeOutcome, *updateExpr, error) {
	if err := in.validateLegacy(); err != nil {
		return writeOutcome{}, nil, err
	}
	if len(in.AttributeUpdates) > 0 && string(in.AttributeUpdates) != "null" {
		return writeOutcome{}, nil, validation("The legacy AttributeUpdates parameter is not supported; use UpdateExpression")
	}
	t, err := s.loadTable(in.TableName)
	if err != nil {
		return writeOutcome{}, nil, err
	}
	if err := checkKeyArgument(t, in.Key); err != nil {
		return writeOutcome{}, nil, err
	}
	switch in.ReturnValues {
	case "", "NONE", "ALL_OLD", "UPDATED_OLD", "ALL_NEW", "UPDATED_NEW":
	default:
		return writeOutcome{}, nil, validation("Return values set to invalid value")
	}
	c, err := compileExpressions(expressionInputs{names: in.ExpressionAttributeNames, values: in.ExpressionAttributeValues,
		condition: in.ConditionExpression, update: in.UpdateExpression})
	if err != nil {
		return writeOutcome{}, nil, err
	}
	if c.update != nil {
		for _, p := range c.update.paths() {
			if contains(t.key().attributes(), p[0].name) {
				return writeOutcome{}, nil, validation("One or more parameter values were invalid: Cannot update attribute " + p[0].name + ". This attribute is part of the key")
			}
		}
	}
	old, err := s.currentItem(t, in.Key)
	if err != nil {
		return writeOutcome{}, nil, err
	}
	if ok, err := check(c.condition, old, "ConditionExpression"); err != nil {
		return writeOutcome{}, nil, err
	} else if !ok {
		return writeOutcome{}, nil, conditionFailed()
	}
	base := old
	if base == nil {
		base = cloneItem(in.Key)
	}
	updated := cloneItem(base)
	if c.update != nil {
		if updated, err = c.update.apply(base); err != nil {
			if e, ok := err.(*exprError); ok {
				return writeOutcome{}, nil, validation(e.msg)
			}
			return writeOutcome{}, nil, err
		}
	}
	if itemSize(updated) > maxItemSize {
		return writeOutcome{}, nil, validation("Item size to update has exceeded the maximum allowed size")
	}
	if err := validateItem(t, updated); err != nil {
		return writeOutcome{}, nil, err
	}
	return writeOutcome{table: t, key: keyOnly(t, in.Key), old: old, new: updated}, c.update, nil
}

func (s *Service) updateItem(r *http.Request) (any, error) {
	var in updateInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	o, u, err := s.prepareUpdate(in)
	if err != nil {
		return nil, err
	}
	if err := s.commitWrite(o); err != nil {
		return nil, err
	}
	out := map[string]any{}
	var attrs item
	switch in.ReturnValues {
	case "ALL_OLD":
		attrs = o.old
	case "ALL_NEW":
		attrs = o.new
	case "UPDATED_OLD", "UPDATED_NEW":
		source := o.new
		if in.ReturnValues == "UPDATED_OLD" {
			source = o.old
		}
		if u != nil && source != nil {
			attrs = project(source, u.paths())
		}
	}
	if len(attrs) > 0 {
		out["Attributes"] = attrs
	}
	if cc := consumed(o.table.TableName, in.ReturnConsumedCapacity, writeUnits(maxInt(itemSize(o.new), itemSize(o.old))), false); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- DeleteItem ----

type deleteInput struct {
	writeCommon
	Key item `json:"Key"`
}

func (s *Service) prepareDelete(in deleteInput) (writeOutcome, error) {
	if err := in.validateLegacy(); err != nil {
		return writeOutcome{}, err
	}
	t, err := s.loadTable(in.TableName)
	if err != nil {
		return writeOutcome{}, err
	}
	if err := checkKeyArgument(t, in.Key); err != nil {
		return writeOutcome{}, err
	}
	switch in.ReturnValues {
	case "", "NONE", "ALL_OLD":
	default:
		return writeOutcome{}, validation("Return values set to invalid value")
	}
	c, err := compileExpressions(expressionInputs{names: in.ExpressionAttributeNames, values: in.ExpressionAttributeValues, condition: in.ConditionExpression})
	if err != nil {
		return writeOutcome{}, err
	}
	old, err := s.currentItem(t, in.Key)
	if err != nil {
		return writeOutcome{}, err
	}
	if ok, err := check(c.condition, old, "ConditionExpression"); err != nil {
		return writeOutcome{}, err
	} else if !ok {
		return writeOutcome{}, conditionFailed()
	}
	return writeOutcome{table: t, key: in.Key, old: old}, nil
}

func (s *Service) deleteItem(r *http.Request) (any, error) {
	var in deleteInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	o, err := s.prepareDelete(in)
	if err != nil {
		return nil, err
	}
	if err := s.commitWrite(o); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if in.ReturnValues == "ALL_OLD" && o.old != nil {
		out["Attributes"] = o.old
	}
	if cc := consumed(o.table.TableName, in.ReturnConsumedCapacity, writeUnits(itemSize(o.old)), false); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}
