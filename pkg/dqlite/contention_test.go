package dqlite

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestIsContentionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("syntax error near SELECT"), false},
		{"record not found is not contention", errors.New("record not found"), false},
		{"database is locked", errors.New("database is locked"), true},
		{"database table is locked", errors.New("database table is locked"), true},
		{"database is busy", errors.New("database is busy"), true},
		{"checkpoint in progress", errors.New("checkpoint in progress"), true},
		{"sqlite_busy text", errors.New("SQLITE_BUSY: database is locked"), true},
		{"transaction within a transaction", errors.New("cannot start a transaction within a transaction"), true},
		{"wrapped checkpoint", fmt.Errorf("exec failed: %w", errors.New("checkpoint in progress")), true},
		{"sqlite3 busy code", sqlite3.Error{Code: sqlite3.ErrBusy}, true},
		{"sqlite3 locked code", sqlite3.Error{Code: sqlite3.ErrLocked}, true},
		{"sqlite3 other code", sqlite3.Error{Code: sqlite3.ErrConstraint}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsContentionError(tc.err))
		})
	}
}

func TestIsConnPoolPoisonedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"locked is not poisoned", errors.New("database is locked"), false},
		{"direct", errors.New("cannot start a transaction within a transaction"), true},
		{"wrapped", fmt.Errorf("begin: %w", errors.New("cannot start a transaction within a transaction")), true},
		{"case-insensitive", errors.New("CANNOT start a Transaction WITHIN a transaction"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsConnPoolPoisonedError(tc.err))
		})
	}
}
