package dqlite

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/pkg/dbtrace"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	dqliteapp "github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	_ "github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

const (
	// DriverName is the default driver name for dqlite.
	DriverName = "dqlite"

	defaultDatabaseName = "caesium"
	dqliteBusyTimeout   = 5 * time.Second
)

var (
	appMu      sync.Mutex
	currentApp atomic.Pointer[dqliteapp.App]

	ErrNoNativeApp = errors.New("dqlite: native app is not active")
)

type Dialector struct {
	DriverName string
	DSN        string
	Conn       gorm.ConnPool
}

func Open(dsn string) gorm.Dialector {
	return &Dialector{DSN: dsn}
}

func (dialector Dialector) Name() string {
	return DriverName
}

func (dialector Dialector) Initialize(db *gorm.DB) (err error) {
	if dialector.DriverName == "" {
		dialector.DriverName = DriverName
	}
	databaseName := databaseNameFromDSN(dialector.DSN)

	supportsSQLPragmas := true
	if dialector.Conn != nil {
		db.ConnPool = dialector.Conn
	} else {
		// go-dqlite rejects SQL PRAGMA statements with SQLITE_AUTH; configure
		// the busy timeout through its native node option instead.
		supportsSQLPragmas = false
		logFunc := func(l client.LogLevel, format string, a ...interface{}) {
			// log info by default
			fn := log.Info

			switch l {
			case client.LogDebug:
				fn = log.Debug
			case client.LogInfo:
			case client.LogWarn:
				fn = log.Warn
			case client.LogError:
				fn = log.Error
			}

			msg := fmt.Sprintf(format, a...)
			fn("dqlite", dqliteLogFields(l, msg)...)
		}

		dqApp, err := nativeApp(context.Background(), logFunc)
		if err != nil {
			return err
		}

		conn, err := dqApp.Open(context.Background(), databaseName)
		if err != nil {
			return err
		}

		db.ConnPool = conn
	}

	if supportsSQLPragmas {
		if err := setConnectionPragmas(context.Background(), db.ConnPool); err != nil {
			return err
		}
	}

	var version string
	if err := db.ConnPool.QueryRowContext(context.Background(), "select sqlite_version()").Scan(&version); err != nil {
		return err
	}
	// https://www.sqlite.org/releaselog/3_35_0.html
	if compareVersion(version, "3.35.0") >= 0 {
		callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{
			CreateClauses:        []string{"INSERT", "VALUES", "ON CONFLICT", "RETURNING"},
			UpdateClauses:        []string{"UPDATE", "SET", "WHERE", "RETURNING"},
			DeleteClauses:        []string{"DELETE", "FROM", "WHERE", "RETURNING"},
			LastInsertIDReversed: true,
		})
	} else {
		callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{
			LastInsertIDReversed: true,
		})
	}

	for k, v := range dialector.ClauseBuilders() {
		db.ClauseBuilders[k] = v
	}
	return
}

func databaseNameFromDSN(dsn string) string {
	name := strings.TrimSpace(dsn)
	if name == "" {
		return defaultDatabaseName
	}
	return name
}

func nativeApp(ctx context.Context, logFunc func(client.LogLevel, string, ...interface{})) (*dqliteapp.App, error) {
	if dqApp := currentApp.Load(); dqApp != nil {
		return dqApp, nil
	}

	appMu.Lock()
	defer appMu.Unlock()

	if dqApp := currentApp.Load(); dqApp != nil {
		return dqApp, nil
	}

	vars := env.Variables()
	dqApp, err := dqliteapp.New(
		vars.DatabasePath,
		dqliteapp.WithAddress(vars.NodeAddress),
		dqliteapp.WithCluster(vars.DatabaseNodes),
		dqliteapp.WithVoters(vars.DatabaseVoters),
		dqliteapp.WithStandBys(vars.DatabaseStandbys),
		dqliteapp.WithLogFunc(logFunc),
		dqliteapp.WithBusyTimeout(dqliteBusyTimeout),
	)
	if err != nil {
		return nil, err
	}

	if err := dqApp.Ready(ctx); err != nil {
		return nil, err
	}

	currentApp.Store(dqApp)
	return dqApp, nil
}

func dqliteLogFields(level client.LogLevel, msg string) []interface{} {
	fields := []interface{}{"msg", msg, "source", "dqlite"}
	if level == client.LogWarn && isUnknownDataTypeWarning(msg) {
		fields = append(fields, "recent_db_statements", dbtrace.Recent(8))
	}
	return fields
}

func isUnknownDataTypeWarning(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "unknown data type: 0")
}

func setConnectionPragmas(ctx context.Context, conn gorm.ConnPool) error {
	for _, stmt := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// ClusterNode is a dqlite cluster member visible to the current native app.
type ClusterNode struct {
	ID       uint64
	Address  string
	Role     string
	IsLeader bool
}

// Cluster returns the current dqlite cluster membership from the leader.
func Cluster(ctx context.Context) ([]ClusterNode, error) {
	dqApp := currentApp.Load()
	if dqApp == nil {
		return nil, ErrNoNativeApp
	}

	cli, err := dqApp.FindLeader(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()

	leader, err := cli.Leader(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := cli.Cluster(ctx)
	if err != nil {
		return nil, err
	}

	cluster := make([]ClusterNode, 0, len(nodes))
	for _, node := range nodes {
		cluster = append(cluster, ClusterNode{
			ID:       node.ID,
			Address:  node.Address,
			Role:     node.Role.String(),
			IsLeader: leader != nil && node.ID == leader.ID,
		})
	}
	return cluster, nil
}

// IsLocalLeader reports whether this process hosts the current dqlite leader.
func IsLocalLeader(ctx context.Context) (bool, error) {
	dqApp := currentApp.Load()
	if dqApp == nil {
		return false, ErrNoNativeApp
	}

	cli, err := dqApp.FindLeader(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = cli.Close() }()

	leader, err := cli.Leader(ctx)
	if err != nil {
		return false, err
	}
	return leader != nil && leader.Address == dqApp.Address(), nil
}

func (dialector Dialector) ClauseBuilders() map[string]clause.ClauseBuilder {
	return map[string]clause.ClauseBuilder{
		"INSERT": func(c clause.Clause, builder clause.Builder) {
			if insert, ok := c.Expression.(clause.Insert); ok {
				if stmt, ok := builder.(*gorm.Statement); ok {
					if _, err := stmt.WriteString("INSERT "); err != nil {
						_ = stmt.AddError(err)
						return
					}
					if insert.Modifier != "" {
						if _, err := stmt.WriteString(insert.Modifier); err != nil {
							_ = stmt.AddError(err)
							return
						}
						if err := stmt.WriteByte(' '); err != nil {
							_ = stmt.AddError(err)
							return
						}
					}

					if _, err := stmt.WriteString("INTO "); err != nil {
						_ = stmt.AddError(err)
						return
					}
					if insert.Table.Name == "" {
						stmt.WriteQuoted(stmt.Table)
					} else {
						stmt.WriteQuoted(insert.Table)
					}
					return
				}
			}

			c.Build(builder)
		},
		"LIMIT": func(c clause.Clause, builder clause.Builder) {
			if limit, ok := c.Expression.(clause.Limit); ok && limit.Limit != nil {
				if *limit.Limit > 0 || limit.Offset > 0 {
					if *limit.Limit <= 0 {
						i := -1
						limit.Limit = &i
					}
					if _, err := builder.WriteString("LIMIT " + strconv.Itoa(*limit.Limit)); err != nil {
						_ = builder.AddError(err)
						return
					}
				}
				if limit.Offset > 0 {
					if _, err := builder.WriteString(" OFFSET " + strconv.Itoa(limit.Offset)); err != nil {
						_ = builder.AddError(err)
						return
					}
				}
			}
		},
		"FOR": func(c clause.Clause, builder clause.Builder) {
			if _, ok := c.Expression.(clause.Locking); ok {
				// SQLite3 does not support row-level locking.
				return
			}
			c.Build(builder)
		},
	}
}

func (dialector Dialector) DefaultValueOf(field *schema.Field) clause.Expression {
	if field.AutoIncrement {
		return clause.Expr{SQL: "NULL"}
	}

	// doesn't work, will raise error
	return clause.Expr{SQL: "DEFAULT"}
}

func (dialector Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{migrator.Migrator{Config: migrator.Config{
		DB:                          db,
		Dialector:                   dialector,
		CreateIndexAfterCreateTable: true,
	}}}
}

func (dialector Dialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, v interface{}) {
	if err := writer.WriteByte('?'); err != nil {
		panic(err)
	}
}

func (dialector Dialector) QuoteTo(writer clause.Writer, str string) {
	if err := writer.WriteByte('`'); err != nil {
		panic(err)
	}
	if strings.Contains(str, ".") {
		for idx, str := range strings.Split(str, ".") {
			if idx > 0 {
				if _, err := writer.WriteString(".`"); err != nil {
					panic(err)
				}
			}
			if _, err := writer.WriteString(str); err != nil {
				panic(err)
			}
			if err := writer.WriteByte('`'); err != nil {
				panic(err)
			}
		}
	} else {
		if _, err := writer.WriteString(str); err != nil {
			panic(err)
		}
		if err := writer.WriteByte('`'); err != nil {
			panic(err)
		}
	}
}

func (dialector Dialector) Explain(sql string, vars ...interface{}) string {
	return logger.ExplainSQL(sql, nil, `"`, vars...)
}

func (dialector Dialector) DataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "numeric"
	case schema.Int, schema.Uint:
		if field.AutoIncrement && !field.PrimaryKey {
			// https://www.sqlite.org/autoinc.html
			return "integer PRIMARY KEY AUTOINCREMENT"
		} else {
			return "integer"
		}
	case schema.Float:
		return "real"
	case schema.String:
		return "text"
	case schema.Time:
		return "datetime"
	case schema.Bytes:
		return "blob"
	}

	return string(field.DataType)
}

func (dialectopr Dialector) SavePoint(tx *gorm.DB, name string) error {
	tx.Exec("SAVEPOINT " + name)
	return nil
}

func (dialectopr Dialector) RollbackTo(tx *gorm.DB, name string) error {
	tx.Exec("ROLLBACK TO SAVEPOINT " + name)
	return nil
}

func compareVersion(version1, version2 string) int {
	n, m := len(version1), len(version2)
	i, j := 0, 0
	for i < n || j < m {
		x := 0
		for ; i < n && version1[i] != '.'; i++ {
			x = x*10 + int(version1[i]-'0')
		}
		i++
		y := 0
		for ; j < m && version2[j] != '.'; j++ {
			y = y*10 + int(version2[j]-'0')
		}
		j++
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
	}
	return 0
}

// ClusterNodes returns the current list of nodes in the dqlite cluster.
func ClusterNodes(ctx context.Context) ([]client.NodeInfo, error) {
	v := env.Variables()
	if v.DatabaseType != "internal" && v.DatabaseType != DriverName {
		return nil, nil
	}

	c, err := client.New(ctx, v.NodeAddress)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	return c.Cluster(ctx)
}
