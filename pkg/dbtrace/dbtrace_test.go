package dbtrace

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecentReturnsStatementsOldestToNewest(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Record("select 1", 1, 10*time.Millisecond, nil)
	Record("select 2", 2, 20*time.Millisecond, errors.New("busy"))
	Record("select 3", 3, 30*time.Millisecond, nil)

	got := Recent(2)
	require.Len(t, got, 2)
	require.Equal(t, "select 2", got[0].SQL)
	require.Equal(t, int64(2), got[0].Rows)
	require.Equal(t, "busy", got[0].Error)
	require.Equal(t, "select 3", got[1].SQL)
}

func TestRecordTruncatesLongSQL(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Record(strings.Repeat("x", maxSQLLength+10), 0, time.Millisecond, nil)

	got := Recent(1)
	require.Len(t, got, 1)
	require.Len(t, got[0].SQL, maxSQLLength+len("...(truncated)"))
	require.True(t, strings.HasSuffix(got[0].SQL, "...(truncated)"))
}
