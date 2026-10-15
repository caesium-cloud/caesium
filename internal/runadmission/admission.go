// Package runadmission holds the run-start protocol values shared by the
// execution store and read-only connector correlation. It imports no database,
// execution engine, container client, or CGO dependency.
package runadmission

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// MaxIdempotencyKeyLength bounds a run-start idempotency key.
const MaxIdempotencyKeyLength = 255

// ErrInvalidIdempotencyKey rejects a key that is too long or carries control
// characters. The execution store reexports this same sentinel.
var ErrInvalidIdempotencyKey = errors.New("run: invalid idempotency key")

// StartOutcome is how an accepted run start was admitted.
type StartOutcome string

const (
	StartOutcomeCreated StartOutcome = "created"
	StartOutcomeQueued  StartOutcome = "queued"
	StartOutcomeSkipped StartOutcome = "skipped"
	StartOutcomeDropped StartOutcome = "dropped"
)

// StartAdmission is a read-only projection of the accepted start protocol. It
// deliberately excludes request hashes, params, and task/run payloads.
type StartAdmission struct {
	ID             uuid.UUID
	JobID          uuid.UUID
	IdempotencyKey string
	Outcome        StartOutcome
	Reason         string
	QueueID        *uuid.UUID
	RunID          *uuid.UUID
}

// ValidateIdempotencyKey trims key and reports whether it is usable. An empty
// key is valid and means "not idempotent".
func ValidateIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > MaxIdempotencyKeyLength {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidIdempotencyKey, MaxIdempotencyKeyLength)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: contains a control character", ErrInvalidIdempotencyKey)
		}
	}
	return key, nil
}
