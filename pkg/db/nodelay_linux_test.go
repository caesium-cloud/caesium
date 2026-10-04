//go:build linux

package db

import (
	"context"
	"database/sql"
	"github.com/caesium-cloud/caesium/internal/testutil"
	"slices"
	"strings"
	"testing"
	"time"

	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/stretchr/testify/require"
)

// freeLoopbackAddress returns a 127.0.0.1 address with a port the kernel just
// handed out, so this package's node never collides with the fixed ports the
// pkg/dqlite tests bind while the two packages run in parallel.
func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	return testutil.FreeLoopbackAddress(t)
}

func startNoDelayTestNode(t *testing.T) (*dqliteapp.App, int) {
	t.Helper()
	address := freeLoopbackAddress(t)
	port, err := nodeAddressPort(address)
	require.NoError(t, err)
	app, err := dqliteapp.New(t.TempDir(), dqliteapp.WithAddress(address))
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, app.Ready(ctx))
	return app, port
}

func openNoDelayTestDB(t *testing.T, app *dqliteapp.App) *sql.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := app.Open(ctx, "nodelay")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// medianQuery times query on db n times and returns the median wall clock.
func medianQuery(t *testing.T, db *sql.DB, query string, n int) (time.Duration, int) {
	t.Helper()
	var (
		samples []time.Duration
		bytes   int
	)
	for range n {
		began := time.Now()
		rows, err := db.QueryContext(context.Background(), query)
		require.NoError(t, err)
		bytes = 0
		for rows.Next() {
			var payload string
			require.NoError(t, rows.Scan(&payload))
			bytes += len(payload)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		samples = append(samples, time.Since(began))
	}
	slices.Sort(samples)
	return samples[len(samples)/2], bytes
}

// TestSetNodeNoDelayCoversListenerAndAcceptedSockets proves the #588 fix on a
// real dqlite node: setNodeNoDelay reaches the node's listener and the
// connection it had already accepted, a connection accepted AFTER the call
// inherits TCP_NODELAY from the listener, and a multi-part result (16 rows of
// 4 KiB, well over dqlite's one-page response buffer) is no longer held back
// by Nagle's algorithm waiting for the client's delayed acknowledgement.
func TestSetNodeNoDelayCoversListenerAndAcceptedSockets(t *testing.T) {
	app, port := startNoDelayTestNode(t)
	db := openNoDelayTestDB(t, app)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx := context.Background()
	_, err := db.ExecContext(ctx, "CREATE TABLE payloads (id INTEGER PRIMARY KEY, body TEXT NOT NULL)")
	require.NoError(t, err)
	const rows, rowBytes = 16, 4096
	body := strings.Repeat("x", rowBytes)
	for i := range rows {
		_, err := db.ExecContext(ctx, "INSERT INTO payloads (id, body) VALUES (?, ?)", i, body)
		require.NoError(t, err)
	}
	const query = "SELECT body FROM payloads ORDER BY id"

	// Upstream behaviour, recorded rather than asserted (it belongs to the C
	// library and the kernel): the node leaves Nagle on for what it accepts.
	listener, accepted, err := nodeSocketNoDelay(port)
	require.NoError(t, err)
	require.NotNil(t, listener, "the node's listener must be visible on port %d", port)
	require.NotEmpty(t, accepted, "the test connection must have been accepted on port %d", port)
	before, bytes := medianQuery(t, db, query, 3)
	require.Equal(t, rows*rowBytes, bytes)
	t.Logf("before: listener nodelay=%v accepted=%v median %s for %d bytes", *listener, accepted, before, bytes)

	applied, err := setNodeNoDelay(port)
	require.NoError(t, err)
	require.GreaterOrEqual(t, applied, 1+len(accepted), "listener plus every accepted socket")

	listener, accepted, err = nodeSocketNoDelay(port)
	require.NoError(t, err)
	require.NotNil(t, listener)
	require.True(t, *listener, "the listener must carry TCP_NODELAY")
	for fd, on := range accepted {
		require.Truef(t, on, "already-accepted server socket fd %d must carry TCP_NODELAY", fd)
	}

	// A connection accepted after the call: nothing touches its socket, so it
	// carries TCP_NODELAY only if it inherited it from the listener.
	known := make(map[int]bool, len(accepted))
	for fd := range accepted {
		known[fd] = true
	}
	fresh := openNoDelayTestDB(t, app)
	fresh.SetMaxOpenConns(1)
	after, bytes := medianQuery(t, fresh, query, 9)
	require.Equal(t, rows*rowBytes, bytes)
	_, accepted, err = nodeSocketNoDelay(port)
	require.NoError(t, err)
	var inherited []int
	for fd, on := range accepted {
		if known[fd] {
			continue
		}
		require.Truef(t, on, "server socket fd %d accepted after the fix must inherit TCP_NODELAY", fd)
		inherited = append(inherited, fd)
	}
	require.NotEmpty(t, inherited, "the fresh connection must have been accepted after the fix")
	t.Logf("after: new sockets %v median %s", inherited, after)

	// With Nagle on, the second response part waits for the client's delayed
	// ACK, so every multi-part query costs at least the 40 ms Linux delayed-ACK
	// minimum (measured 41.6 ms for this query before the fix, 0.8 ms after,
	// under -race). Half the stall is the bound: a slow CI host has a 20x
	// margin over the unstalled read, and the stalled read cannot pass.
	require.Lessf(t, after, 20*time.Millisecond,
		"a %d-byte result must not wait on a delayed acknowledgement (median %s, before the fix %s)", bytes, after, before)
}

func TestSetNodeNoDelayIgnoresOtherPorts(t *testing.T) {
	// A port nothing in this process listens on visits no socket.
	address := freeLoopbackAddress(t)
	port, err := nodeAddressPort(address)
	require.NoError(t, err)
	applied, err := setNodeNoDelay(port)
	require.NoError(t, err)
	require.Zero(t, applied)
}
