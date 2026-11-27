package advise

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/stratus/internal/store"
)

// Bucket is the metadata bucket decisions live in. Its "advise-" prefix
// is registered in the control catalog, so reset and snapshots cover it.
const Bucket = "advise-decisions"

// Repository persists decisions.
type Repository interface {
	Save(d Decision) error
	Get(id string) (Decision, bool, error)
	List() ([]Decision, error)
}

// StoreRepository keeps decisions in the metadata store as JSON.
type StoreRepository struct{ meta store.Store }

// NewStoreRepository returns a repository over meta.
func NewStoreRepository(meta store.Store) StoreRepository { return StoreRepository{meta: meta} }

// Save writes a decision, replacing any with the same ID.
func (r StoreRepository) Save(d Decision) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode decision %s: %w", d.ID, err)
	}
	return r.meta.Put(Bucket, d.ID, raw)
}

// Get reads a decision; ok is false when there is none with that ID.
func (r StoreRepository) Get(id string) (Decision, bool, error) {
	raw, err := r.meta.Get(Bucket, id)
	if err != nil || raw == nil {
		return Decision{}, false, err
	}
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Decision{}, false, fmt.Errorf("decode decision %s: %w", id, err)
	}
	return d, true, nil
}

// List returns every decision, newest first.
func (r StoreRepository) List() ([]Decision, error) {
	var out []Decision
	err := r.meta.Scan(Bucket, "", func(_, v []byte) error {
		var d Decision
		if err := json.Unmarshal(v, &d); err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, err
}
