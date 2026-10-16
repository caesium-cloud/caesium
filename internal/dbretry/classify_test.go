package dbretry

import (
	"errors"
	"fmt"
	"testing"
)

type sqlStateError struct{ code string }

func (e sqlStateError) Error() string    { return "pg error " + e.code }
func (e sqlStateError) SQLState() string { return e.code }

func TestIsSerializationConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("40001"), false},
		{"serialization failure", sqlStateError{"40001"}, true},
		{"deadlock", sqlStateError{"40P01"}, true},
		{"wrapped", fmt.Errorf("commit: %w", sqlStateError{"40001"}), true},
		{"unique violation", sqlStateError{"23505"}, false},
		{"lock not available", sqlStateError{"55P03"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSerializationConflict(tc.err); got != tc.want {
				t.Fatalf("IsSerializationConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsLockContention(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("syntax error near SELECT"), false},
		{"not found", errors.New("record not found"), false},
		{"locked", errors.New("database is locked"), true},
		{"table locked", errors.New("DATABASE TABLE IS LOCKED"), true},
		{"schema locked", errors.New("database schema is locked"), true},
		{"busy", errors.New("wrapped: database is busy"), true},
		{"checkpoint", fmt.Errorf("exec: %w", errors.New("checkpoint in progress")), true},
		{"sqlite busy text", errors.New("SQLITE_BUSY: x"), true},
		{"sqlite locked text", errors.New("SQLITE_LOCKED: x"), true},
		{"poisoned", errors.New("cannot start a transaction within a transaction"), true},
		{"serialization is not lock contention", sqlStateError{"40001"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLockContention(tc.err); got != tc.want {
				t.Fatalf("IsLockContention(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
