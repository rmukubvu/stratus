package fis

import (
	"encoding/json"
	"errors"

	"github.com/stratus/internal/store"
)

// Buckets share the "fis-" prefix, so the control API resets and snapshots
// FIS state with the rest of the service.
const (
	templatesBucket   = "fis-experiment-templates"
	experimentsBucket = "fis-experiments"
)

// errNotFound is returned by the repository for a missing record.
var errNotFound = errors.New("not found")

// repository persists templates and experiments in the metadata store.
type repository struct {
	store store.Store
}

func (r repository) putTemplate(t Template) error { return r.put(templatesBucket, t.ID, t) }

func (r repository) template(id string) (Template, error) {
	var t Template
	return t, r.get(templatesBucket, id, &t)
}

func (r repository) deleteTemplate(id string) error { return r.store.Delete(templatesBucket, id) }

func (r repository) templates() ([]Template, error) { return list[Template](r.store, templatesBucket) }

func (r repository) putExperiment(e experimentRecord) error {
	return r.put(experimentsBucket, e.Experiment.ID, e)
}

func (r repository) experiment(id string) (experimentRecord, error) {
	var e experimentRecord
	return e, r.get(experimentsBucket, id, &e)
}

func (r repository) experiments() ([]experimentRecord, error) {
	return list[experimentRecord](r.store, experimentsBucket)
}

func (r repository) put(bucket, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.store.Put(bucket, key, raw)
}

func (r repository) get(bucket, key string, v any) error {
	raw, err := r.store.Get(bucket, key)
	if err != nil {
		return err
	}
	if raw == nil {
		return errNotFound
	}
	return json.Unmarshal(raw, v)
}

func list[T any](s store.Store, bucket string) ([]T, error) {
	var out []T
	err := s.Scan(bucket, "", func(_, v []byte) error {
		var item T
		if err := json.Unmarshal(v, &item); err != nil {
			return nil // skip records we cannot read rather than fail the listing
		}
		out = append(out, item)
		return nil
	})
	return out, err
}
