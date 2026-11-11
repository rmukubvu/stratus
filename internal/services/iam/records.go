package iam

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

// getRecord loads a JSON record, reporting NoSuchEntity with the given
// description when it is missing.
func getRecord[T any](st store.Store, bucket, key, what string) (T, error) {
	var record T
	raw, err := st.Get(bucket, key)
	if err != nil {
		return record, internal(err)
	}
	if raw == nil {
		return record, noSuchEntity(what)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, internal(err)
	}
	return record, nil
}

func putRecord(st store.Store, bucket, key string, record any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := st.Put(bucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

func exists(st store.Store, bucket, key string) (bool, error) {
	raw, err := st.Get(bucket, key)
	if err != nil {
		return false, internal(err)
	}
	return raw != nil, nil
}

// scanRecords decodes every record under prefix, skipping corrupt ones.
func scanRecords[T any](st store.Store, bucket, prefix string) ([]T, error) {
	var out []T
	err := st.Scan(bucket, prefix, func(_, v []byte) error {
		var record T
		if json.Unmarshal(v, &record) == nil {
			out = append(out, record)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

func sortedBy[T any](items []T, key func(T) string) []T {
	sort.Slice(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
	return items
}

func noSuchEntity(what string) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NoSuchEntity", Message: fmt.Sprintf("The %s cannot be found.", what)}
}

func alreadyExists(what string) error {
	return &apierror.Error{StatusCode: http.StatusConflict, Code: "EntityAlreadyExists", Message: what + " already exists."}
}

func deleteConflict(message string) error {
	return &apierror.Error{StatusCode: http.StatusConflict, Code: "DeleteConflict", Message: message}
}

// entityARN builds an IAM ARN such as arn:aws:iam::123456789012:user/path/name.
func entityARN(account, kind, path, name string) string {
	if path == "" {
		path = "/"
	}
	return "arn:aws:iam::" + account + ":" + kind + path + name
}

func newEntityID(prefix string) string {
	return prefix + strings.ToUpper(randomAlnum(17))
}
