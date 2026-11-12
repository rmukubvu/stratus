package dynamodb

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/stratus/internal/apierror"
)

// ---- BatchGetItem ----

const maxBatchGetResponse = 16 * 1024 * 1024

func (s *Service) batchGetItem(r *http.Request) (any, error) {
	var in struct {
		RequestItems map[string]struct {
			Keys                     []item            `json:"Keys"`
			ProjectionExpression     string            `json:"ProjectionExpression,omitempty"`
			ExpressionAttributeNames map[string]string `json:"ExpressionAttributeNames,omitempty"`
			AttributesToGet          []string          `json:"AttributesToGet,omitempty"`
			ConsistentRead           bool              `json:"ConsistentRead,omitempty"`
		} `json:"RequestItems"`
		ReturnConsumedCapacity string `json:"ReturnConsumedCapacity"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if len(in.RequestItems) == 0 {
		return nil, validation("1 validation error detected: Value null at 'requestItems' failed to satisfy constraint: Member must have length greater than or equal to 1")
	}
	if err := checkRequestItemNames(in.RequestItems); err != nil {
		return nil, err
	}
	total := 0
	for _, req := range in.RequestItems {
		total += len(req.Keys)
	}
	if total > 100 {
		return nil, validation("Too many items requested for the BatchGetItem call")
	}
	responses := map[string][]item{}
	var capacity []map[string]any
	names := make([]string, 0, len(in.RequestItems))
	for name := range in.RequestItems {
		names = append(names, name)
	}
	sort.Strings(names)
	unprocessed := map[string]any{}
	responseBytes := 0
	for _, name := range names {
		req := in.RequestItems[name]
		var deferred []item
		seen := map[string]bool{}
		responses[name] = []item{}
		units := 0.0
		for _, key := range req.Keys {
			t, it, err := s.readOne(getInput{TableName: name, Key: key, ProjectionExpression: req.ProjectionExpression,
				ExpressionAttributeNames: req.ExpressionAttributeNames, AttributesToGet: req.AttributesToGet, ConsistentRead: req.ConsistentRead})
			if err != nil {
				return nil, err
			}
			k, _ := storageKey(t, key)
			if seen[k] {
				return nil, validation("Provided list of item keys contains duplicates")
			}
			seen[k] = true
			if it == nil {
				units += readUnits(0, req.ConsistentRead)
				continue
			}
			// A response holds at most 16 MB; the rest come back unprocessed.
			if size := itemSize(it); responseBytes+size > maxBatchGetResponse {
				deferred = append(deferred, key)
				continue
			} else {
				responseBytes += size
			}
			units += readUnits(itemSize(it), req.ConsistentRead)
			responses[name] = append(responses[name], it)
		}
		if len(deferred) > 0 {
			retry := req
			retry.Keys = deferred
			unprocessed[name] = retry
		}
		if cc := consumed(name, in.ReturnConsumedCapacity, units, true); cc != nil {
			capacity = append(capacity, cc)
		}
	}
	out := map[string]any{"Responses": responses, "UnprocessedKeys": unprocessed}
	if capacity != nil {
		out["ConsumedCapacity"] = capacity
	}
	return out, nil
}

// checkRequestItemNames validates RequestItems' map keys as table names (or
// ARNs) before any table is looked up.
func checkRequestItemNames[V any](requestItems map[string]V) error {
	for name := range requestItems {
		if strings.HasPrefix(name, "arn:") || tableNamePattern.MatchString(name) {
			continue
		}
		raw, _ := json.Marshal(requestItems)
		return validation("1 validation error detected: Value '" + string(raw) + "' at 'requestItems' failed to satisfy constraint: " +
			"Map keys must satisfy constraint: [Member must have length less than or equal to 255, " +
			"Member must have length greater than or equal to 3, Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+]")
	}
	return nil
}

// ---- BatchWriteItem ----

func (s *Service) batchWriteItem(r *http.Request) (any, error) {
	var in struct {
		RequestItems map[string][]struct {
			PutRequest *struct {
				Item item `json:"Item"`
			} `json:"PutRequest"`
			DeleteRequest *struct {
				Key item `json:"Key"`
			} `json:"DeleteRequest"`
		} `json:"RequestItems"`
		ReturnConsumedCapacity string `json:"ReturnConsumedCapacity"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if len(in.RequestItems) == 0 {
		return nil, validation("1 validation error detected: Value null at 'requestItems' failed to satisfy constraint: Member must have length greater than or equal to 1")
	}
	if err := checkRequestItemNames(in.RequestItems); err != nil {
		return nil, err
	}
	total := 0
	for _, reqs := range in.RequestItems {
		total += len(reqs)
	}
	if total > 25 {
		return nil, validation("1 validation error detected: Value at 'requestItems' failed to satisfy constraint: Map value must satisfy constraint: [Member must have length less than or equal to 25, Member must have length greater than or equal to 1]")
	}
	// Validate everything first: a batch with an invalid request writes nothing.
	var outcomes []writeOutcome
	units := map[string]float64{}
	for name, reqs := range in.RequestItems {
		t, err := s.loadTable(name)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, req := range reqs {
			var o writeOutcome
			switch {
			case req.PutRequest != nil && req.DeleteRequest == nil:
				// BatchWriteItem reports every key defect with the generic message.
				if _, err := storageKey(t, req.PutRequest.Item); err != nil {
					return nil, validation("The provided key element does not match the schema")
				}
				if err := validateItem(t, req.PutRequest.Item); err != nil {
					return nil, err
				}
				key := keyOnly(t, req.PutRequest.Item)
				old, err := s.currentItem(t, key)
				if err != nil {
					return nil, err
				}
				o = writeOutcome{table: t, key: key, old: old, new: cloneItem(req.PutRequest.Item)}
			case req.DeleteRequest != nil && req.PutRequest == nil:
				if err := checkKeyArgument(t, req.DeleteRequest.Key); err != nil {
					return nil, err
				}
				old, err := s.currentItem(t, req.DeleteRequest.Key)
				if err != nil {
					return nil, err
				}
				o = writeOutcome{table: t, key: req.DeleteRequest.Key, old: old}
			default:
				return nil, validation("Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes")
			}
			k, _ := storageKey(t, o.key)
			if seen[k] {
				return nil, validation("Provided list of item keys contains duplicates")
			}
			seen[k] = true
			units[name] += writeUnits(maxInt(itemSize(o.new), itemSize(o.old)))
			outcomes = append(outcomes, o)
		}
	}
	for _, o := range outcomes {
		if err := s.commitWrite(o); err != nil {
			return nil, err
		}
	}
	out := map[string]any{"UnprocessedItems": map[string]any{}}
	var capacity []map[string]any
	for name, u := range units {
		if cc := consumed(name, in.ReturnConsumedCapacity, u, false); cc != nil {
			capacity = append(capacity, cc)
		}
	}
	if capacity != nil {
		out["ConsumedCapacity"] = capacity
	}
	return out, nil
}

// ---- transactions ----

type transactCondition struct {
	TableName                           string            `json:"TableName"`
	Key                                 item              `json:"Key"`
	ConditionExpression                 string            `json:"ConditionExpression"`
	ExpressionAttributeNames            map[string]string `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues           map[string]any    `json:"ExpressionAttributeValues"`
	ReturnValuesOnConditionCheckFailure string            `json:"ReturnValuesOnConditionCheckFailure"`
}

type transactWriteItem struct {
	ConditionCheck *transactCondition `json:"ConditionCheck"`
	Put            *putInput          `json:"Put"`
	Update         *updateInput       `json:"Update"`
	Delete         *deleteInput       `json:"Delete"`
}

// cancellation is one TransactionCanceledException reason.
type cancellation struct {
	Code    string `json:"Code"`
	Message string `json:"Message,omitempty"`
	Item    item   `json:"Item,omitempty"`
}

func (s *Service) transactWriteItems(r *http.Request) (any, error) {
	var in struct {
		TransactItems          []transactWriteItem `json:"TransactItems"`
		ClientRequestToken     string              `json:"ClientRequestToken"`
		ReturnConsumedCapacity string              `json:"ReturnConsumedCapacity"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if len(in.ClientRequestToken) > 36 {
		return nil, validation("1 validation error detected: Value at 'ClientRequestToken' failed to satisfy constraint: Member must have length less than or equal to 36")
	}
	if err := checkTransactCount(len(in.TransactItems)); err != nil {
		return nil, err
	}
	if err := validReturnConsumed(in.ReturnConsumedCapacity); err != nil {
		return nil, err
	}
	fingerprint := requestFingerprint(in.TransactItems)
	if out, replay, err := s.idempotency.lookup(in.ClientRequestToken, fingerprint, s.now()); err != nil || replay {
		return out, err
	}
	outcomes := make([]writeOutcome, len(in.TransactItems))
	reasons := make([]cancellation, len(in.TransactItems))
	failed := false
	targets := map[string]bool{}
	for i, ti := range in.TransactItems {
		o, err := s.prepareTransactItem(ti)
		if err == nil {
			k, _ := storageKey(o.table, o.key)
			if targets[k] {
				return nil, validation("Transaction request cannot include multiple operations on one item")
			}
			targets[k] = true
			outcomes[i] = o
			reasons[i] = cancellation{Code: "None"}
			continue
		}
		var apiErr *apierror.Error
		if ok := asAPIError(err, &apiErr); ok && apiErr.Code == "ConditionalCheckFailedException" {
			failed = true
			reasons[i] = cancellation{Code: "ConditionalCheckFailed", Message: "The conditional request failed"}
			if returnsOld(ti) && o.old != nil {
				reasons[i].Item = o.old
			}
			continue
		}
		return nil, err
	}
	if failed {
		return nil, transactionCanceled(reasons)
	}
	units := map[string]float64{}
	for _, o := range outcomes {
		if err := s.commitWrite(o); err != nil {
			return nil, err
		}
		// Transactional writes cost twice a standard write.
		units[o.table.TableName] += 2 * writeUnits(maxInt(itemSize(o.new), itemSize(o.old)))
	}
	out := map[string]any{}
	if capacity := capacityList(units, in.ReturnConsumedCapacity, false); capacity != nil {
		out["ConsumedCapacity"] = capacity
	}
	s.idempotency.record(in.ClientRequestToken, fingerprint, out, s.now())
	return out, nil
}

func checkTransactCount(n int) error {
	if n == 0 || n > 100 {
		return validation("1 validation error detected: Value at 'TransactItems' failed to satisfy constraint: Member must have length less than or equal to 100")
	}
	return nil
}

// capacityList renders per-table ConsumedCapacity, sorted by table name.
func capacityList(units map[string]float64, mode string, read bool) []map[string]any {
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []map[string]any
	for _, name := range names {
		if cc := consumed(name, mode, units[name], read); cc != nil {
			out = append(out, cc)
		}
	}
	return out
}

func returnsOld(ti transactWriteItem) bool {
	switch {
	case ti.ConditionCheck != nil:
		return ti.ConditionCheck.ReturnValuesOnConditionCheckFailure == "ALL_OLD"
	case ti.Put != nil:
		return ti.Put.ReturnValuesOnConditionCheckFailure == "ALL_OLD"
	case ti.Update != nil:
		return ti.Update.ReturnValuesOnConditionCheckFailure == "ALL_OLD"
	case ti.Delete != nil:
		return ti.Delete.ReturnValuesOnConditionCheckFailure == "ALL_OLD"
	}
	return false
}

// prepareTransactItem validates one transaction action and evaluates its
// condition. On a failed condition it returns the outcome (for the old
// item) together with ConditionalCheckFailedException.
func (s *Service) prepareTransactItem(ti transactWriteItem) (writeOutcome, error) {
	set := 0
	for _, present := range []bool{ti.ConditionCheck != nil, ti.Put != nil, ti.Update != nil, ti.Delete != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return writeOutcome{}, validation("TransactItems can only contain one of Check, Put, Update or Delete")
	}
	switch {
	case ti.Put != nil:
		if ti.Put.ReturnValues != "" && ti.Put.ReturnValues != "NONE" {
			return writeOutcome{}, validation("ReturnValues is not supported in a transaction")
		}
		o, err := s.preparePut(*ti.Put)
		return s.withOld(o, err, ti.Put.TableName, keyOf(ti.Put.TableName, ti.Put.Item, s))
	case ti.Update != nil:
		o, _, err := s.prepareUpdate(*ti.Update)
		return s.withOld(o, err, ti.Update.TableName, ti.Update.Key)
	case ti.Delete != nil:
		o, err := s.prepareDelete(*ti.Delete)
		return s.withOld(o, err, ti.Delete.TableName, ti.Delete.Key)
	}
	cc := ti.ConditionCheck
	if cc.ConditionExpression == "" {
		return writeOutcome{}, validation("1 validation error detected: Value null at 'transactItems.1.member.conditionCheck.conditionExpression' failed to satisfy constraint: Member must not be null")
	}
	t, err := s.loadTable(cc.TableName)
	if err != nil {
		return writeOutcome{}, err
	}
	if err := checkKeyArgument(t, cc.Key); err != nil {
		return writeOutcome{}, err
	}
	c, err := compileExpressions(expressionInputs{names: cc.ExpressionAttributeNames, values: cc.ExpressionAttributeValues, condition: cc.ConditionExpression})
	if err != nil {
		return writeOutcome{}, err
	}
	old, err := s.currentItem(t, cc.Key)
	if err != nil {
		return writeOutcome{}, err
	}
	// A condition check writes nothing: its outcome leaves the item as it is.
	o := writeOutcome{table: t, key: keyOnly(t, cc.Key), old: old, new: old}
	if ok, err := check(c.condition, old, "ConditionExpression"); err != nil {
		return writeOutcome{}, err
	} else if !ok {
		return o, conditionFailed()
	}
	// With neither an old nor a new item, committing the outcome is a no-op.
	return writeOutcome{table: t, key: o.key}, nil
}

func keyOf(table string, it item, s *Service) item {
	t, err := s.loadTable(table)
	if err != nil {
		return nil
	}
	return keyOnly(t, it)
}

// withOld attaches the current item to a failed condition, for
// ReturnValuesOnConditionCheckFailure.
func (s *Service) withOld(o writeOutcome, err error, table string, key item) (writeOutcome, error) {
	if err == nil {
		return o, nil
	}
	var apiErr *apierror.Error
	if asAPIError(err, &apiErr) && apiErr.Code == "ConditionalCheckFailedException" {
		if t, lerr := s.loadTable(table); lerr == nil {
			if old, lerr := s.currentItem(t, key); lerr == nil {
				return writeOutcome{table: t, key: key, old: old}, err
			}
		}
	}
	return o, err
}

func transactionCanceled(reasons []cancellation) error {
	codes := make([]string, len(reasons))
	for i, r := range reasons {
		codes[i] = r.Code
	}
	return &transactionCanceledError{
		apiErr:  apierror.Error{StatusCode: http.StatusBadRequest, Code: "TransactionCanceledException", Message: "Transaction cancelled, please refer cancellation reasons for specific reasons [" + strings.Join(codes, ", ") + "]"},
		reasons: reasons,
	}
}

// transactionCanceledError carries CancellationReasons, which the shared
// error writer cannot express, so Handle writes it itself.
type transactionCanceledError struct {
	apiErr  apierror.Error
	reasons []cancellation
}

func (e *transactionCanceledError) Error() string { return e.apiErr.Error() }
func (e *transactionCanceledError) Unwrap() error { return &e.apiErr }
func (e *transactionCanceledError) WriteErrorResponse(w http.ResponseWriter) {
	writeJSON(w, e.apiErr.StatusCode, e.body())
}

func (e *transactionCanceledError) body() map[string]any {
	return map[string]any{"__type": errorTypePrefix + e.apiErr.Code, "Message": e.apiErr.Message, "CancellationReasons": e.reasons}
}

func asAPIError(err error, target **apierror.Error) bool {
	switch e := err.(type) {
	case *apierror.Error:
		*target = e
		return true
	case *transactionCanceledError:
		*target = &e.apiErr
		return true
	}
	return false
}

func (s *Service) transactGetItems(r *http.Request) (any, error) {
	var in struct {
		TransactItems []struct {
			Get *getInput `json:"Get"`
		} `json:"TransactItems"`
		ReturnConsumedCapacity string `json:"ReturnConsumedCapacity"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := checkTransactCount(len(in.TransactItems)); err != nil {
		return nil, err
	}
	units := map[string]float64{}
	responses := make([]map[string]any, 0, len(in.TransactItems))
	seen := map[string]bool{}
	for _, ti := range in.TransactItems {
		if ti.Get == nil {
			return nil, validation("TransactItems can only contain Get")
		}
		t, it, err := s.readOne(*ti.Get)
		if err != nil {
			return nil, err
		}
		k, _ := storageKey(t, ti.Get.Key)
		if seen[k] {
			return nil, validation("Transaction request cannot include multiple operations on one item")
		}
		seen[k] = true
		entry := map[string]any{}
		if it != nil {
			entry["Item"] = it
		}
		responses = append(responses, entry)
		units[t.TableName] += 2 * readUnits(itemSize(it), true)
	}
	out := map[string]any{"Responses": responses}
	if capacity := capacityList(units, in.ReturnConsumedCapacity, true); capacity != nil {
		out["ConsumedCapacity"] = capacity
	}
	return out, nil
}
