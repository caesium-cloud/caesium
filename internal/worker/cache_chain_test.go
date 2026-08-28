package worker

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cache_chain_test.go covers the DISTRIBUTED executor's half of infra-deploy A3.
//
// The correctness risk this guards is lane divergence: the worker rebuilds the
// cache identity from the scheduler-set TaskRun columns rather than from the job
// definition, so a column it forgets to read makes the SAME unit of work hash
// differently depending on which executor ran it — a permanent cache miss that
// looks like cache flakiness. The chain mode is exactly such a column.

// executeWithChain seeds one instance row, stamps the resolved chain/ttl columns
// on it the way internal/run/store.go's scheduler snapshot does, runs the real
// executor against a fake engine, and returns the persisted row.
func executeWithChain(t *testing.T, alias, chain string) models.TaskRun {
	t.Helper()
	f := seedFanOutTaskRun(t, alias, `{"from":"producer"}`, pkgtask.Partition{Key: "shard-1"}, 1, true)

	f.taskRun.CacheChain = chain
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("id = ?", f.taskRun.ID).
		Update("cache_chain", chain).Error)

	executeCapturingEnv(t, f)

	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	return row
}

// TestWorkerCacheIdentityCarriesChain: the mode must reach the worker's
// HashInput. If it did not, a values-mode step scheduled onto a worker would
// fold predecessor hashes back in and never hit the entry the local lane wrote.
func TestWorkerCacheIdentityCarriesChain(t *testing.T) {
	transitive := executeWithChain(t, "chain-worker-transitive", jobdefschema.CacheChainTransitive)
	values := executeWithChain(t, "chain-worker-values", jobdefschema.CacheChainValues)

	require.NotEmpty(t, transitive.Hash)
	require.NotEmpty(t, values.Hash)
	assert.NotEqual(t, transitive.Hash, values.Hash,
		"the chain mode is part of the identity, so the worker must fold it in")

	assert.Contains(t, string(values.HashInputBlob), `"chain":"values"`,
		"the persisted blob must name the mode so `caesium why` can explain the exclusion")
	assert.NotContains(t, string(transitive.HashInputBlob), `"chain"`,
		"transitive blobs must stay byte-identical to the pre-chain era")
}

// TestWorkerUnsetChainMatchesTransitive pins the upgrade path: every TaskRun row
// written before the cache_chain column existed carries "", and those rows must
// keep the identity they were scheduled with.
func TestWorkerUnsetChainMatchesTransitive(t *testing.T) {
	unset := executeWithChain(t, "chain-worker-unset", "")
	explicit := executeWithChain(t, "chain-worker-unset", jobdefschema.CacheChainTransitive)
	assert.Equal(t, unset.Hash, explicit.Hash,
		"an empty cache_chain column must hash identically to an explicit transitive")
}

// TestEntryExpiry_TTLNever is the `ttl: never` half of A3, on the decision every
// cache-entry writer (local, fan-out publisher, worker) shares. A nil ExpiresAt
// is what cache.Store.Get already treats as "never expires".
func TestEntryExpiry_TTLNever(t *testing.T) {
	created := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	assert.Nil(t, cache.EntryExpiry(created, 24*time.Hour, true),
		"ttl: never must beat an inherited TTL default; an apply keyed on a fingerprint must not expire on a wall clock")
	assert.Nil(t, cache.EntryExpiry(created, 0, false),
		"no TTL at all still means no expiry, as before")

	expiry := cache.EntryExpiry(created, 24*time.Hour, false)
	require.NotNil(t, expiry)
	assert.Equal(t, created.Add(24*time.Hour), *expiry)
}
