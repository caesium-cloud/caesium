package database

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"gorm.io/gorm"
)

const (
	defaultQueryLimit = 200
	maxQueryLimit     = 1000
	queryTimeout      = 10 * time.Second
)

var (
	ErrEmptyQuery         = errors.New("query is required")
	ErrMultipleStatements = errors.New("only a single SQL statement is allowed")
	ErrUnsafeQuery        = errors.New("only read-only SQL statements are allowed")

	forbiddenReadOnlyPattern = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|ALTER|DROP|CREATE|REPLACE|TRUNCATE|ATTACH|DETACH|VACUUM|REINDEX|MERGE|COPY|CALL|GRANT|REVOKE|COMMENT|CLUSTER|ANALYZE|REFRESH|DO)\b`)
	selectIntoPattern        = regexp.MustCompile(`(?i)\bSELECT\s+INTO\b`)
)

type SchemaResponse struct {
	Dialect  string        `json:"dialect"`
	Version  string        `json:"version,omitempty"`
	ReadOnly bool          `json:"read_only"`
	Tables   []TableSchema `json:"tables"`
}

type TableSchema struct {
	Name     string         `json:"name"`
	RowCount *int64         `json:"row_count,omitempty"`
	Columns  []ColumnSchema `json:"columns"`
}

type ColumnSchema struct {
	Name         string  `json:"name"`
	DataType     string  `json:"data_type"`
	Nullable     bool    `json:"nullable"`
	PrimaryKey   bool    `json:"primary_key"`
	DefaultValue *string `json:"default_value,omitempty"`
}

type QueryRequest struct {
	SQL   string `json:"sql"`
	Limit int    `json:"limit"`
}

type QueryResponse struct {
	Dialect       string        `json:"dialect"`
	ReadOnly      bool          `json:"read_only"`
	StatementType string        `json:"statement_type"`
	Query         string        `json:"query"`
	Limit         int           `json:"limit"`
	DurationMs    int64         `json:"duration_ms"`
	RowCount      int           `json:"row_count"`
	Truncated     bool          `json:"truncated"`
	Columns       []QueryColumn `json:"columns"`
	Rows          [][]any       `json:"rows"`
}

type QueryColumn struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}

type Service struct {
	ctx context.Context
	db  *gorm.DB
}

func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

func NewWithDB(ctx context.Context, gdb *gorm.DB) *Service {
	return &Service{ctx: ctx, db: gdb}
}

func (s *Service) Schema() (*SchemaResponse, error) {
	tables, err := s.getTables()
	if err != nil {
		return nil, err
	}
	sort.Strings(tables)

	version, err := s.version()
	if err != nil {
		return nil, err
	}

	resp := &SchemaResponse{
		Dialect:  s.db.Name(),
		Version:  version,
		ReadOnly: true,
		Tables:   make([]TableSchema, 0, len(tables)),
	}

	for _, tableName := range tables {
		columns, err := s.getTableColumns(tableName)
		if err != nil {
			return nil, err
		}

		table := TableSchema{
			Name:    tableName,
			Columns: columns,
		}

		resp.Tables = append(resp.Tables, table)
	}

	return resp, nil
}

func (s *Service) Query(req QueryRequest) (resp *QueryResponse, err error) {
	query := strings.TrimSpace(req.SQL)
	if query == "" {
		return nil, ErrEmptyQuery
	}
	if err := validateReadOnlyQuery(query); err != nil {
		return nil, err
	}

	limit := normalizeLimit(req.Limit)
	ctx, cancel := context.WithTimeout(s.ctx, queryTimeout)
	defer cancel()

	sqlDB, err := s.db.DB()
	if err != nil {
		return nil, err
	}

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := conn.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	started := time.Now()
	rows, cleanup, err := s.queryRows(ctx, conn, query)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanupErr := cleanup(); err == nil && cleanupErr != nil {
			err = cleanupErr
		}
	}()

	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}

	resp = &QueryResponse{
		Dialect:       s.db.Name(),
		ReadOnly:      true,
		StatementType: queryStatementType(query),
		Query:         query,
		Limit:         limit,
		Columns:       make([]QueryColumn, 0, len(columnTypes)),
		Rows:          make([][]any, 0),
	}

	for _, columnType := range columnTypes {
		resp.Columns = append(resp.Columns, QueryColumn{
			Name:     columnType.Name(),
			DataType: columnType.DatabaseTypeName(),
		})
	}

	for rows.Next() {
		if len(resp.Rows) >= limit {
			resp.Truncated = true
			break
		}

		rawValues := make([]any, len(columnTypes))
		scanTargets := make([]any, len(columnTypes))
		for i := range rawValues {
			scanTargets[i] = &rawValues[i]
		}

		if err := rows.Scan(scanTargets...); err != nil {
			return nil, err
		}

		row := make([]any, len(rawValues))
		for i, value := range rawValues {
			row[i] = normalizeQueryValue(value)
		}
		resp.Rows = append(resp.Rows, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	resp.RowCount = len(resp.Rows)
	resp.DurationMs = time.Since(started).Milliseconds()
	return resp, nil
}

func (s *Service) queryRows(ctx context.Context, conn *sql.Conn, query string) (*sql.Rows, func() error, error) {
	if s.db.Name() == "postgres" {
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, nil, err
		}

		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			_ = tx.Rollback()
			return nil, nil, err
		}

		return rows, func() error {
			rowsErr := rows.Close()
			txErr := tx.Rollback()
			if rowsErr != nil {
				return rowsErr
			}
			if txErr != nil && !errors.Is(txErr, sql.ErrTxDone) {
				return txErr
			}
			return nil
		}, nil
	}

	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return nil, nil, err
	}

	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		_, _ = conn.ExecContext(ctx, "PRAGMA query_only = OFF")
		return nil, nil, err
	}

	return rows, func() error {
		rowsErr := rows.Close()
		_, pragmaErr := conn.ExecContext(context.Background(), "PRAGMA query_only = OFF")
		if rowsErr != nil {
			return rowsErr
		}
		return pragmaErr
	}, nil
}

func (s *Service) version() (string, error) {
	var version string
	switch s.db.Name() {
	case "postgres":
		if err := s.db.WithContext(s.ctx).Raw("SHOW server_version").Scan(&version).Error; err != nil {
			return "", err
		}
	default:
		if err := s.db.WithContext(s.ctx).Raw("SELECT sqlite_version()").Scan(&version).Error; err != nil {
			return "", err
		}
	}
	return version, nil
}

func (s *Service) getTables() ([]string, error) {
	switch s.db.Name() {
	case "sqlite", dqlite.DriverName:
		var tableList []string
		err := s.db.WithContext(s.ctx).
			Raw("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name").
			Scan(&tableList).Error
		return tableList, err
	default:
		return s.db.Migrator().GetTables()
	}
}

func (s *Service) getTableColumns(tableName string) ([]ColumnSchema, error) {
	switch s.db.Name() {
	case "sqlite", dqlite.DriverName:
		return s.getSQLiteTableColumns(tableName)
	default:
		columnTypes, err := s.db.Migrator().ColumnTypes(tableName)
		if err != nil {
			return nil, err
		}

		columns := make([]ColumnSchema, 0, len(columnTypes))
		for _, columnType := range columnTypes {
			nullable, _ := columnType.Nullable()
			primaryKey, _ := columnType.PrimaryKey()
			defaultValue, hasDefault := columnType.DefaultValue()

			column := ColumnSchema{
				Name:       columnType.Name(),
				DataType:   columnType.DatabaseTypeName(),
				Nullable:   nullable,
				PrimaryKey: primaryKey,
			}
			if hasDefault {
				column.DefaultValue = &defaultValue
			}
			columns = append(columns, column)
		}
		return columns, nil
	}
}

func (s *Service) getSQLiteTableColumns(tableName string) ([]ColumnSchema, error) {
	type pragmaColumn struct {
		Name       string         `gorm:"column:name"`
		DataType   string         `gorm:"column:type"`
		NotNull    int            `gorm:"column:notnull"`
		DefaultVal sql.NullString `gorm:"column:dflt_value"`
		PrimaryKey int            `gorm:"column:pk"`
	}

	query := fmt.Sprintf("PRAGMA table_info(%s)", quoteSQLiteIdentifier(tableName))
	var pragmaColumns []pragmaColumn
	if err := s.db.WithContext(s.ctx).Raw(query).Scan(&pragmaColumns).Error; err != nil {
		return nil, err
	}

	columns := make([]ColumnSchema, 0, len(pragmaColumns))
	for _, pragmaColumn := range pragmaColumns {
		column := ColumnSchema{
			Name:       pragmaColumn.Name,
			DataType:   pragmaColumn.DataType,
			Nullable:   pragmaColumn.NotNull == 0,
			PrimaryKey: pragmaColumn.PrimaryKey > 0,
		}
		if pragmaColumn.DefaultVal.Valid {
			defaultValue := pragmaColumn.DefaultVal.String
			column.DefaultValue = &defaultValue
		}
		columns = append(columns, column)
	}
	return columns, nil
}

func validateReadOnlyQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return ErrEmptyQuery
	}
	if hasMultipleStatements(query) {
		return ErrMultipleStatements
	}

	trimmed := trimLeadingComments(query)
	trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), ";")
	if trimmed == "" {
		return ErrEmptyQuery
	}

	normalized := sanitizeSQL(trimmed)
	upper := strings.ToUpper(normalized)
	switch {
	case strings.HasPrefix(upper, "SELECT "),
		strings.HasPrefix(upper, "WITH "),
		strings.HasPrefix(upper, "EXPLAIN "),
		strings.HasPrefix(upper, "VALUES "),
		strings.HasPrefix(upper, "SHOW "):
	default:
		return ErrUnsafeQuery
	}

	if forbiddenReadOnlyPattern.MatchString(upper) {
		return ErrUnsafeQuery
	}
	if selectIntoPattern.MatchString(upper) {
		return ErrUnsafeQuery
	}

	return nil
}

func hasMultipleStatements(query string) bool {
	inSingleQuote := false
	inDoubleQuote := false
	inBacktick := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(query); i++ {
		if inLineComment {
			if query[i] == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if i+1 < len(query) && query[i] == '*' && query[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inSingleQuote {
			if query[i] == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					i++
					continue
				}
				inSingleQuote = false
			}
			continue
		}
		if inDoubleQuote {
			if query[i] == '"' {
				inDoubleQuote = false
			}
			continue
		}
		if inBacktick {
			if query[i] == '`' {
				inBacktick = false
			}
			continue
		}

		if i+1 < len(query) && query[i] == '-' && query[i+1] == '-' {
			inLineComment = true
			i++
			continue
		}
		if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}

		switch query[i] {
		case '\'':
			inSingleQuote = true
		case '"':
			inDoubleQuote = true
		case '`':
			inBacktick = true
		case ';':
			if strings.TrimSpace(query[i+1:]) != "" {
				return true
			}
		}
	}

	return false
}

func trimLeadingComments(query string) string {
	trimmed := strings.TrimSpace(query)
	for {
		switch {
		case strings.HasPrefix(trimmed, "--"):
			idx := strings.Index(trimmed, "\n")
			if idx == -1 {
				return ""
			}
			trimmed = strings.TrimSpace(trimmed[idx+1:])
		case strings.HasPrefix(trimmed, "/*"):
			idx := strings.Index(trimmed, "*/")
			if idx == -1 {
				return ""
			}
			trimmed = strings.TrimSpace(trimmed[idx+2:])
		default:
			return trimmed
		}
	}
}

func queryStatementType(query string) string {
	trimmed := trimLeadingComments(query)
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return "unknown"
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "unknown"
	}
	return strings.ToLower(fields[0])
}

func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultQueryLimit
	case limit > maxQueryLimit:
		return maxQueryLimit
	default:
		return limit
	}
}

func normalizeQueryValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		if utf8.Valid(v) {
			return string(v)
		}
		return hex.EncodeToString(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case bool, float32, float64, int, int8, int16, int32, int64, string, uint, uint8, uint16, uint32, uint64:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		if jsonValue, err := json.Marshal(v); err == nil {
			return string(jsonValue)
		}
		return fmt.Sprint(v)
	}
}

func quoteSQLiteIdentifier(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}

func sanitizeSQL(query string) string {
	var builder strings.Builder
	builder.Grow(len(query))

	inSingleQuote := false
	inDoubleQuote := false
	inBacktick := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(query); i++ {
		ch := query[i]

		switch {
		case inLineComment:
			if ch == '\n' {
				inLineComment = false
				builder.WriteByte('\n')
			} else {
				builder.WriteByte(' ')
			}
			continue
		case inBlockComment:
			if i+1 < len(query) && ch == '*' && query[i+1] == '/' {
				inBlockComment = false
				builder.WriteString("  ")
				i++
			} else {
				builder.WriteByte(' ')
			}
			continue
		case inSingleQuote:
			if ch == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					builder.WriteString("  ")
					i++
					continue
				}
				inSingleQuote = false
			}
			builder.WriteByte(' ')
			continue
		case inDoubleQuote:
			if ch == '"' {
				inDoubleQuote = false
			}
			builder.WriteByte(' ')
			continue
		case inBacktick:
			if ch == '`' {
				inBacktick = false
			}
			builder.WriteByte(' ')
			continue
		}

		if i+1 < len(query) && ch == '-' && query[i+1] == '-' {
			inLineComment = true
			builder.WriteString("  ")
			i++
			continue
		}
		if i+1 < len(query) && ch == '/' && query[i+1] == '*' {
			inBlockComment = true
			builder.WriteString("  ")
			i++
			continue
		}

		switch ch {
		case '\'':
			inSingleQuote = true
			builder.WriteByte(' ')
		case '"':
			inDoubleQuote = true
			builder.WriteByte(' ')
		case '`':
			inBacktick = true
			builder.WriteByte(' ')
		default:
			builder.WriteByte(ch)
		}
	}

	return builder.String()
}
