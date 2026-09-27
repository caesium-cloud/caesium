package dqlite

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	godqlite "github.com/canonical/go-dqlite/v3"
	"github.com/canonical/go-dqlite/v3/client"
	"github.com/stretchr/testify/require"
)

func TestSnapshotParamsDefaultsAndValidation(t *testing.T) {
	static := func(threshold, trailing uint64) godqlite.SnapshotParams {
		return godqlite.SnapshotParams{Threshold: threshold, Trailing: trailing, Strategy: godqlite.TrailingStrategyStatic}
	}

	params, err := snapshotParams(env.DefaultDatabaseSnapshotThreshold, env.DefaultDatabaseSnapshotTrailing)
	require.NoError(t, err)
	require.Equal(t, static(1024, 2048), params)

	// A zero Environment gets the same bounded defaults, never dqlite's own
	// trailing 8192.
	params, err = snapshotParams(0, 0)
	require.NoError(t, err)
	require.Equal(t, static(1024, 2048), params)

	params, err = snapshotParams(512, 512)
	require.NoError(t, err)
	require.Equal(t, static(512, 512), params)

	for _, tc := range []struct {
		name                string
		threshold, trailing int
		want                string
	}{
		{"zero threshold", 0, 2048, "CAESIUM_DATABASE_SNAPSHOT_THRESHOLD"},
		{"negative threshold", -1, 2048, "CAESIUM_DATABASE_SNAPSHOT_THRESHOLD"},
		{"trailing below dqlite floor", 1, 3, "CAESIUM_DATABASE_SNAPSHOT_TRAILING"},
		{"trailing below threshold", 1024, 512, "greater than or equal to CAESIUM_DATABASE_SNAPSHOT_THRESHOLD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := snapshotParams(tc.threshold, tc.trailing)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNativeAppOptionsRejectInvalidSnapshotParams(t *testing.T) {
	_, err := nativeAppOptions(env.Environment{
		NodeAddress:               "127.0.0.1:9501",
		DatabaseVoters:            3,
		DatabaseSnapshotThreshold: 2048,
		DatabaseSnapshotTrailing:  1024,
	}, func(client.LogLevel, string, ...any) {})
	require.ErrorContains(t, err, "CAESIUM_DATABASE_SNAPSHOT_TRAILING")
}

var (
	closedSegmentName = regexp.MustCompile(`^(\d{16})-(\d{16})$`)
	snapshotName      = regexp.MustCompile(`^snapshot-\d+-(\d+)-\d+$`)
)

type raftRetention struct {
	// snapshots are the indexes of the snapshot data files on disk.
	snapshots []uint64
	// closed are the [first, last] entry indexes of the closed segments.
	closed [][2]uint64
}

// readRaftRetention lists dir without failing the test, so it can run inside
// require.Eventually's condition goroutine.
func readRaftRetention(dir string) (raftRetention, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return raftRetention{}, err
	}
	var out raftRetention
	for _, entry := range entries {
		name := entry.Name()
		if m := snapshotName.FindStringSubmatch(name); m != nil {
			index, err := strconv.ParseUint(m[1], 10, 64)
			if err != nil {
				return raftRetention{}, err
			}
			out.snapshots = append(out.snapshots, index)
			continue
		}
		if m := closedSegmentName.FindStringSubmatch(name); m != nil {
			first, err := strconv.ParseUint(m[1], 10, 64)
			if err != nil {
				return raftRetention{}, err
			}
			last, err := strconv.ParseUint(m[2], 10, 64)
			if err != nil {
				return raftRetention{}, err
			}
			out.closed = append(out.closed, [2]uint64{first, last})
		}
	}
	return out, nil
}

// TestNativeAppOptionsBoundTheRetainedRaftLog is the regression for the F2
// snapshot catch-up memory exhaustion. dqlite keeps every retained Raft entry
// in memory and releases it only when a snapshot moves the retained window past
// it; the same trailing count decides which closed segment files survive on
// disk (raft's logSnapshot and uvSegmentKeepTrailing both take it). Before the
// snapshot parameters were wired, the node ran with dqlite's threshold 1024 and
// trailing 8192, so this workload would take no snapshot at all and keep every
// entry from index 1.
func TestNativeAppOptionsBoundTheRetainedRaftLog(t *testing.T) {
	const (
		threshold = 4
		trailing  = 8
		writes    = 48
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	address := "127.0.0.1:9501"
	opts, err := nativeAppOptions(env.Environment{
		NodeAddress:               address,
		DatabaseVoters:            3,
		DatabaseStandbys:          3,
		DatabaseSnapshotThreshold: threshold,
		DatabaseSnapshotTrailing:  trailing,
	}, func(client.LogLevel, string, ...any) {})
	require.NoError(t, err)

	app, err := openNativeApp(ctx, dir, address, nil, nil, opts...)
	require.NoError(t, err)
	defer func() { _ = app.Close() }()

	db, err := app.Open(ctx, "retention")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, "CREATE TABLE blobs (id INTEGER PRIMARY KEY, body BLOB)")
	require.NoError(t, err)
	// Each statement rewrites ~1 MiB of pages, so each Raft entry is ~1 MiB and
	// an 8 MiB segment closes every handful of entries while the database
	// itself stays ~1 MiB.
	for i := 0; i < writes; i++ {
		_, err = db.ExecContext(ctx, "INSERT OR REPLACE INTO blobs (id, body) VALUES (1, randomblob(1048576))")
		require.NoError(t, err)
	}

	// Snapshots are taken asynchronously. dqlite keeps the newest two; by the
	// time the newest one is on disk, the segment removal for the one before it
	// has finished, so bound the retained segments against that older one.
	var retention raftRetention
	var older uint64
	require.Eventually(t, func() bool {
		var err error
		if retention, err = readRaftRetention(dir); err != nil || len(retention.snapshots) < 2 {
			return false
		}
		newest := uint64(0)
		older = 0
		for _, index := range retention.snapshots {
			if index > newest {
				older, newest = newest, index
			} else if index > older {
				older = index
			}
		}
		return newest >= writes-2*threshold
	}, 30*time.Second, 100*time.Millisecond, "no snapshot covered the writes: the threshold was not applied")

	require.NotEmpty(t, retention.closed, "no closed segment to bound")
	require.Greater(t, older, uint64(trailing))
	retainFrom := older - trailing + 1
	lowest := retention.closed[0][0]
	for _, segment := range retention.closed {
		require.GreaterOrEqual(t, segment[1], retainFrom,
			"closed segment %d-%d is older than trailing %d behind snapshot %d: the trailing count was not applied",
			segment[0], segment[1], trailing, older)
		lowest = min(lowest, segment[0])
	}
	require.Greater(t, lowest, uint64(1), "no Raft entry was ever released")
}
