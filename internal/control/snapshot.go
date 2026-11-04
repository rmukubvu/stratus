package control

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SnapshotID names a snapshot. Callers may choose it ("baseline",
// "after-seed") so scripts and agents can refer to snapshots by intent.
type SnapshotID string

var snapshotIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ParseSnapshotID validates a caller-supplied snapshot name.
func ParseSnapshotID(raw string) (SnapshotID, error) {
	id := strings.TrimSpace(raw)
	if !snapshotIDPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", &Error{
			Kind:    KindInvalid,
			Code:    "InvalidSnapshotName",
			Message: fmt.Sprintf("invalid snapshot name %q", raw),
			Hint:    "use 1-63 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit",
		}
	}
	return SnapshotID(id), nil
}

// NewSnapshotID generates a unique, sortable snapshot name.
func NewSnapshotID(now time.Time) SnapshotID {
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	return SnapshotID(fmt.Sprintf("snap-%s-%s", now.UTC().Format("20060102t150405"), hex.EncodeToString(suffix[:])))
}

func (id SnapshotID) String() string { return string(id) }

// Snapshot is a point-in-time copy of all emulator state.
type Snapshot struct {
	ID        SnapshotID `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	// Services lists the services that held state when the snapshot was taken.
	Services []string `json:"services"`
	// SizeBytes is the logical size of the snapshot. Blobs are hard-linked
	// where possible, so the real disk cost is usually much lower.
	SizeBytes int64 `json:"size_bytes"`
}

func snapshotNotFound(id SnapshotID) *Error {
	return &Error{
		Kind:    KindNotFound,
		Code:    "SnapshotNotFound",
		Message: fmt.Sprintf("snapshot %q does not exist", id),
		Hint:    "list snapshots with GET /_stratus/control/v1/snapshots",
	}
}

func snapshotExists(id SnapshotID) *Error {
	return &Error{
		Kind:    KindConflict,
		Code:    "SnapshotAlreadyExists",
		Message: fmt.Sprintf("snapshot %q already exists", id),
		Hint:    "choose another name or delete the existing snapshot first",
	}
}
