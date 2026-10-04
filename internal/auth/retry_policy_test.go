package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestRetryPoliciesKeepNarrowClassificationAndFixedBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"lock", errors.New("database is locked"), 5},
		{"table lock", errors.New("DATABASE TABLE IS LOCKED"), 5},
		{"busy", errors.New("wrapped: database is busy"), 5},
		{"schema lock", errors.New("database schema is locked"), 1},
		{"checkpoint", errors.New("checkpoint in progress"), 1},
		{"typed busy", sqlite3.Error{Code: sqlite3.ErrBusy}, 1},
		{"poisoned", errors.New("cannot start a transaction within a transaction"), 1},
	} {
		for _, write := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/read", true: "/write"}[write], func(t *testing.T) {
				var sleeps []time.Duration
				svc := NewService(nil, WithSleep(func(d time.Duration) { sleeps = append(sleeps, d) }))
				calls := 0
				var err error
				if write {
					var rows int64
					rows, err = svc.withBootstrapRetry(func() (int64, error) { calls++; return 99, tc.err })
					require.Zero(t, rows)
				} else {
					err = svc.withReadRetry(func() error { calls++; return tc.err })
				}
				require.ErrorIs(t, err, tc.err)
				require.Equal(t, tc.attempts, calls)
				require.Len(t, sleeps, tc.attempts-1)
				for _, d := range sleeps {
					require.Equal(t, 10*time.Millisecond, d)
				}
			})
		}
	}
}
