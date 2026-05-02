package db

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DatabaseRole identifies the logical database tier for a table.
type DatabaseRole string

const (
	DatabaseRoleCatalog DatabaseRole = "catalog"
	DatabaseRoleHot     DatabaseRole = "hot"
	DatabaseRoleCold    DatabaseRole = "cold"
)

var hotTables = map[string]struct{}{
	"job_runs":         {},
	"task_runs":        {},
	"callback_runs":    {},
	"execution_events": {},
}

// Router owns the catalog, hot-shard, and cold-history database handles.
//
// The default shard count is one, in which case every route returns the catalog
// connection and existing single-database behavior is preserved.
type Router struct {
	catalog *gorm.DB
	hot     []*gorm.DB
	cold    *gorm.DB
}

// NewRouter constructs a Router from already-opened connections. Tests and
// non-dqlite callers can use this directly; production code should normally use
// DefaultRouter().
func NewRouter(catalog *gorm.DB, hot []*gorm.DB, cold *gorm.DB) (*Router, error) {
	if catalog == nil {
		return nil, errors.New("db router requires a catalog connection")
	}
	if len(hot) == 0 {
		hot = []*gorm.DB{catalog}
	}
	for idx, shard := range hot {
		if shard == nil {
			return nil, fmt.Errorf("db router hot shard %d is nil", idx)
		}
	}
	if cold == nil {
		cold = catalog
	}

	return &Router{
		catalog: catalog,
		hot:     append([]*gorm.DB(nil), hot...),
		cold:    cold,
	}, nil
}

// Catalog returns the write-light catalog database.
func (r *Router) Catalog() *gorm.DB {
	if r == nil {
		return nil
	}
	return r.catalog
}

// Cold returns the cold-history database. With one shard it aliases Catalog().
func (r *Router) Cold() *gorm.DB {
	if r == nil {
		return nil
	}
	return r.cold
}

// HotShards returns a copy of the hot shard connection slice.
func (r *Router) HotShards() []*gorm.DB {
	if r == nil {
		return nil
	}
	return append([]*gorm.DB(nil), r.hot...)
}

// ShardCount returns the number of hot shards.
func (r *Router) ShardCount() int {
	if r == nil || len(r.hot) == 0 {
		return 0
	}
	return len(r.hot)
}

// HotShard returns a hot shard by index.
func (r *Router) HotShard(index int) (*gorm.DB, error) {
	if r == nil || len(r.hot) == 0 {
		return nil, errors.New("db router has no hot shards")
	}
	if index < 0 || index >= len(r.hot) {
		return nil, fmt.Errorf("hot shard index %d out of range [0,%d)", index, len(r.hot))
	}
	return r.hot[index], nil
}

// ShardForRunID maps a job run ID to a stable hot-shard index.
func (r *Router) ShardForRunID(runID uuid.UUID) int {
	if r == nil || len(r.hot) <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write(runID[:])
	return int(h.Sum32() % uint32(len(r.hot)))
}

// HotShardForRun returns the hot shard that owns a run's lifecycle rows.
func (r *Router) HotShardForRun(runID uuid.UUID) (*gorm.DB, int, error) {
	shard := r.ShardForRunID(runID)
	conn, err := r.HotShard(shard)
	return conn, shard, err
}

// Database returns the connection for a logical role. Hot routes require a
// non-zero run ID so all rows in one run stay transactionally local.
func (r *Router) Database(role DatabaseRole, runID uuid.UUID) (*gorm.DB, int, error) {
	switch role {
	case DatabaseRoleCatalog:
		return r.Catalog(), -1, nil
	case DatabaseRoleCold:
		return r.Cold(), -1, nil
	case DatabaseRoleHot:
		if runID == uuid.Nil {
			return nil, -1, errors.New("hot database route requires a run ID")
		}
		return r.HotShardForRun(runID)
	default:
		return nil, -1, fmt.Errorf("unknown database role %q", role)
	}
}

// RouteTable returns the database for a table. Hot tables require runID;
// catalog tables ignore it.
func (r *Router) RouteTable(table string, runID uuid.UUID) (*gorm.DB, DatabaseRole, int, error) {
	normalized := normalizeTableName(table)
	if _, ok := hotTables[normalized]; ok {
		conn, shard, err := r.Database(DatabaseRoleHot, runID)
		return conn, DatabaseRoleHot, shard, err
	}
	conn, _, err := r.Database(DatabaseRoleCatalog, uuid.Nil)
	return conn, DatabaseRoleCatalog, -1, err
}

func normalizeTableName(table string) string {
	return strings.ToLower(strings.TrimSpace(table))
}
