package dqlite

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

type Migrator struct {
	migrator.Migrator
}

func (m *Migrator) RunWithoutForeignKey(fc func() error) error {
	var enabled int
	m.DB.Raw("PRAGMA foreign_keys").Scan(&enabled)
	if enabled == 1 {
		m.DB.Exec("PRAGMA foreign_keys = OFF")
		defer m.DB.Exec("PRAGMA foreign_keys = ON")
	}

	return fc()
}

func (m Migrator) HasTable(value any) bool {
	var count int
	if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		return m.DB.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", stmt.Table).Row().Scan(&count)
	}); err != nil {
		return false
	}
	return count > 0
}

func (m Migrator) DropTable(values ...any) error {
	return m.RunWithoutForeignKey(func() error {
		values = m.ReorderModels(values, false)
		tx := m.DB.Session(&gorm.Session{})

		for _, value := range slices.Backward(values) {
			if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
				return tx.Exec("DROP TABLE IF EXISTS ?", clause.Table{Name: stmt.Table}).Error
			}); err != nil {
				return err
			}
		}

		return nil
	})
}

func (m Migrator) HasColumn(value any, name string) bool {
	var count int
	if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if field := stmt.Schema.LookUpField(name); field != nil {
				name = field.DBName
			}
		}

		if name != "" {
			return m.DB.Raw(
				"SELECT count(*) FROM sqlite_master WHERE type = ? AND tbl_name = ? AND (sql LIKE ? OR sql LIKE ? OR sql LIKE ?)",
				"table", stmt.Table, `%"`+name+`" %`, `%`+name+` %`, "%`"+name+"`%",
			).Row().Scan(&count)
		}
		return nil
	}); err != nil {
		return false
	}
	return count > 0
}

func (m Migrator) AlterColumn(value any, name string) error {
	return m.RunWithoutForeignKey(func() error {
		return m.recreateTablePreservingIndexes(value, nil, func(rawDDL string, stmt *gorm.Statement) (string, []any, error) {
			field := stmt.Schema.LookUpField(name)
			if field == nil {
				return "", nil, fmt.Errorf("failed to alter field with name %v", name)
			}
			// FullDataTypeOf renders defaults using the dialector, rather than leaving
			// bind arguments. No placeholder is needed in persisted table DDL.
			dataType := m.FullDataTypeOf(field)
			if len(dataType.Vars) != 0 {
				return "", nil, fmt.Errorf("alter field %s: unexpected type arguments", name)
			}
			sql, err := alterColumnDDL(rawDDL, field.DBName, dataType.SQL)
			return sql, nil, err
		})
	})
}

// ddlTokens retains byte offsets, including quoted values and parenthesized
// expressions. Unlike a comma regexp, it cannot consume another column or
// split a default/check expression. Unsupported or unbalanced input fails closed.
type ddlToken struct {
	text       string
	start, end int
}

func ddlTokens(sql string) ([]ddlToken, error) {
	var tokens []ddlToken
	for i := 0; i < len(sql); {
		if strings.ContainsRune(" \t\r\n", rune(sql[i])) {
			i++
			continue
		}
		start, depth, quote := i, 0, byte(0)
		grouped := sql[i] == '(' || strings.ContainsRune("'\"`[", rune(sql[i]))
		for ; i < len(sql); i++ {
			c := sql[i]
			if quote != 0 {
				if c == quote {
					if i+1 < len(sql) && sql[i+1] == quote {
						i++
						continue
					}
					quote = 0
					if depth == 0 {
						i++
						break
					}
				}
				continue
			}
			if depth == 0 && i > start && (strings.ContainsRune(" \t\r\n('\"`[", rune(c))) {
				break
			}
			switch c {
			case '\'', '"', '`':
				quote = c
			case '[':
				quote = ']'
			case '(':
				depth++
			case ')':
				depth--
				if depth < 0 {
					return nil, fmt.Errorf("invalid DDL: unbalanced parentheses")
				}
				if grouped && depth == 0 {
					i++
					goto tokenDone
				}
			case ',', ';':
				if depth == 0 {
					return nil, fmt.Errorf("invalid DDL: unexpected separator")
				}
			}
		}
	tokenDone:
		if quote != 0 || depth != 0 {
			return nil, fmt.Errorf("invalid DDL: unfinished expression")
		}
		tokens = append(tokens, ddlToken{sql[start:i], start, i})
	}
	return tokens, nil
}

func ddlIdentifier(token string) string {
	if len(token) >= 2 {
		q := token[0]
		if q == '`' || q == '"' || q == '\'' || q == '[' {
			end := q
			if q == '[' {
				end = ']'
			}
			if token[len(token)-1] == end {
				return strings.ReplaceAll(token[1:len(token)-1], string(end)+string(end), string(end))
			}
		}
	}
	return token
}

func splitAlterDDL(raw string) (fields []string, open, closeAt int, err error) {
	// parseDDL validates the CREATE TABLE header. Split the original bytes as
	// well: its normalized fields cannot retain escaped string literals verbatim.
	if _, err := parseDDL(raw); err != nil {
		return nil, 0, 0, err
	}
	open = strings.IndexByte(raw, '(')
	if open < 0 {
		return nil, 0, 0, fmt.Errorf("invalid DDL: missing table body")
	}
	header, err := ddlTokens(raw[:open])
	if err != nil || len(header) != 3 || !strings.EqualFold(header[0].text, "CREATE") || !strings.EqualFold(header[1].text, "TABLE") {
		return nil, 0, 0, fmt.Errorf("invalid DDL: table header")
	}
	depth, quote, start := 1, byte(0), open+1
	closeAt = -1
	for i := open + 1; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if c == quote {
				if i+1 < len(raw) && raw[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '[':
			quote = ']'
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				fields = append(fields, raw[start:i])
				closeAt = i
			}
		case ',':
			if depth == 1 {
				fields = append(fields, raw[start:i])
				start = i + 1
			}
		}
		if closeAt >= 0 {
			break
		}
	}
	if closeAt < 0 || quote != 0 {
		return nil, 0, 0, fmt.Errorf("invalid DDL: unfinished table body")
	}
	tail := strings.TrimSpace(raw[closeAt+1:])
	if tail != "" && !strings.EqualFold(tail, "WITHOUT ROWID") && !strings.EqualFold(tail, "STRICT") {
		return nil, 0, 0, fmt.Errorf("invalid DDL: unsupported table suffix")
	}
	return fields, open, closeAt, nil
}

func alterColumnDDL(raw, name, dataType string) (string, error) {
	fields, open, closeAt, err := splitAlterDDL(raw)
	if err != nil {
		return "", err
	}
	found := -1
	for i, definition := range fields {
		tokens, err := ddlTokens(definition)
		if err != nil {
			return "", fmt.Errorf("invalid DDL field: %w", err)
		}
		if len(tokens) == 0 {
			return "", fmt.Errorf("invalid DDL: empty field")
		}
		if !strings.EqualFold(ddlIdentifier(tokens[0].text), name) {
			continue
		}
		if found >= 0 {
			return "", fmt.Errorf("ambiguous DDL field %s", name)
		}
		found = i
		suffix, err := retainedColumnConstraints(definition, tokens)
		if err != nil {
			return "", fmt.Errorf("alter field %s: %w", name, err)
		}
		fields[i] = tokens[0].text + " " + dataType + suffix
	}
	if found < 0 {
		return "", fmt.Errorf("missing DDL field %s", name)
	}
	return raw[:open+1] + strings.Join(fields, ",") + raw[closeAt:], nil
}

func retainedColumnConstraints(definition string, tokens []ddlToken) (string, error) {
	upper := func(i int) string {
		if i >= len(tokens) {
			return ""
		}
		return strings.ToUpper(tokens[i].text)
	}
	isConstraint := func(word string) bool {
		switch word {
		case "CONSTRAINT", "PRIMARY", "NOT", "NULL", "UNIQUE", "CHECK", "DEFAULT", "COLLATE", "REFERENCES", "GENERATED", "AS":
			return true
		}
		return false
	}
	i := 1
	for i < len(tokens) && !isConstraint(upper(i)) {
		i++
	}
	if i == 1 {
		return "", fmt.Errorf("missing column type")
	}
	var retained strings.Builder
	for i < len(tokens) {
		start, keep := i, true
		if upper(i) == "CONSTRAINT" {
			i += 2
			if i >= len(tokens) {
				return "", fmt.Errorf("unfinished named constraint")
			}
		}
		switch upper(i) {
		case "NOT":
			if upper(i+1) != "NULL" {
				return "", fmt.Errorf("unsupported NOT constraint")
			}
			i += 2
			keep = false
		case "NULL":
			i++
			keep = false
		case "DEFAULT":
			i++
			if i < len(tokens) && (upper(i) == "+" || upper(i) == "-") {
				i++
			}
			i++
			keep = false
		case "PRIMARY":
			if upper(i+1) != "KEY" {
				return "", fmt.Errorf("unfinished primary key")
			}
			i += 2
			if upper(i) == "ASC" || upper(i) == "DESC" {
				i++
			}
		case "UNIQUE":
			i++
		case "CHECK":
			i++
			if i >= len(tokens) || !strings.HasPrefix(tokens[i].text, "(") {
				return "", fmt.Errorf("unfinished CHECK")
			}
			i++
		case "COLLATE":
			i += 2
		case "REFERENCES":
			i += 2
			if i < len(tokens) && strings.HasPrefix(tokens[i].text, "(") {
				i++
			}
			for {
				switch upper(i) {
				case "ON":
					if upper(i+1) != "DELETE" && upper(i+1) != "UPDATE" {
						return "", fmt.Errorf("invalid reference action")
					}
					i += 2
					switch upper(i) {
					case "SET":
						i += 2
					case "NO":
						if upper(i+1) != "ACTION" {
							return "", fmt.Errorf("invalid reference action")
						}
						i += 2
					case "CASCADE", "RESTRICT":
						i++
					default:
						return "", fmt.Errorf("invalid reference action")
					}
				case "MATCH":
					i += 2
				case "NOT":
					if upper(i+1) != "DEFERRABLE" {
						goto referenceDone
					}
					i += 2
				case "DEFERRABLE":
					i++
				case "INITIALLY":
					i += 2
				default:
					goto referenceDone
				}
			}
		referenceDone:
			;
		case "GENERATED", "AS":
			if upper(i) == "GENERATED" {
				i++
				if upper(i) == "ALWAYS" {
					i++
				}
			}
			if upper(i) != "AS" {
				return "", fmt.Errorf("invalid generated constraint")
			}
			i++
			if i >= len(tokens) || !strings.HasPrefix(tokens[i].text, "(") {
				return "", fmt.Errorf("invalid generated expression")
			}
			i++
			if upper(i) == "STORED" || upper(i) == "VIRTUAL" {
				i++
			}
		default:
			return "", fmt.Errorf("unsupported column constraint %s", upper(i))
		}
		if upper(i) == "ON" && upper(i+1) == "CONFLICT" {
			i += 3
		}
		if upper(i) == "AUTOINCREMENT" {
			i++
		}
		if i > len(tokens) {
			return "", fmt.Errorf("unfinished column constraint")
		}
		if keep {
			retained.WriteByte(' ')
			retained.WriteString(definition[tokens[start].start:tokens[i-1].end])
		}
	}
	return retained.String(), nil
}

func (m Migrator) DropColumn(value any, name string) error {
	return m.recreateTable(value, nil, func(rawDDL string, stmt *gorm.Statement) (sql string, sqlArgs []any, err error) {
		if field := stmt.Schema.LookUpField(name); field != nil {
			name = field.DBName
		}

		reg, err := regexp.Compile("(`|'|\"| )" + name + "(`|'|\"| ) .*?,")
		if err != nil {
			return "", nil, err
		}

		createSQL := reg.ReplaceAllString(rawDDL, "")

		return createSQL, nil, nil
	})
}

func (m Migrator) CreateConstraint(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraintIfc, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		var (
			constraint *schema.Constraint
			chk        *schema.CheckConstraint
		)
		switch v := constraintIfc.(type) {
		case *schema.Constraint:
			constraint = v
		case *schema.CheckConstraint:
			chk = v
		}

		return m.recreateTable(value, &table,
			func(rawDDL string, stmt *gorm.Statement) (sql string, sqlArgs []any, err error) {
				var (
					constraintName   string
					constraintSql    string
					constraintValues []any
				)

				switch {
				case constraint != nil:
					constraintName = constraint.Name
					constraintSql, constraintValues = buildConstraint(constraint)
				case chk != nil:
					constraintName = chk.Name
					constraintSql = "CONSTRAINT ? CHECK (?)"
					constraintValues = []any{clause.Column{Name: chk.Name}, clause.Expr{SQL: chk.Constraint}}
				default:
					return "", nil, nil
				}

				createDDL, err := parseDDL(rawDDL)
				if err != nil {
					return "", nil, err
				}
				createDDL.addConstraint(constraintName, constraintSql)
				createSQL := createDDL.compile()

				return createSQL, constraintValues, nil
			})
	})
}

func (m Migrator) DropConstraint(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraintIfc, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		var (
			constraint *schema.Constraint
			chk        *schema.CheckConstraint
		)
		switch v := constraintIfc.(type) {
		case *schema.Constraint:
			constraint = v
		case *schema.CheckConstraint:
			chk = v
		}
		if constraint != nil {
			name = constraint.Name
		} else if chk != nil {
			name = chk.Name
		}

		return m.recreateTable(value, &table,
			func(rawDDL string, stmt *gorm.Statement) (sql string, sqlArgs []any, err error) {
				createDDL, err := parseDDL(rawDDL)
				if err != nil {
					return "", nil, err
				}
				createDDL.removeConstraint(name)
				createSQL := createDDL.compile()

				return createSQL, nil, nil
			})
	})
}

func (m Migrator) HasConstraint(value any, name string) bool {
	var count int64
	if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraintIfc, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		var (
			constraint *schema.Constraint
			chk        *schema.CheckConstraint
		)
		switch v := constraintIfc.(type) {
		case *schema.Constraint:
			constraint = v
		case *schema.CheckConstraint:
			chk = v
		}
		if constraint != nil {
			name = constraint.Name
		} else if chk != nil {
			name = chk.Name
		}

		return m.DB.Raw(
			"SELECT count(*) FROM sqlite_master WHERE type = ? AND tbl_name = ? AND (sql LIKE ? OR sql LIKE ? OR sql LIKE ?)",
			"table", table, `%CONSTRAINT "`+name+`" %`, `%CONSTRAINT `+name+` %`, "%CONSTRAINT `"+name+"`%",
		).Row().Scan(&count)

	}); err != nil {
		return false
	}
	return count > 0
}

func (m Migrator) CurrentDatabase() (name string) {
	var null any
	if err := m.DB.Raw("PRAGMA database_list").Row().Scan(&null, &name, &null); err != nil {
		return ""
	}
	return
}

func (m Migrator) BuildIndexOptions(opts []schema.IndexOption, stmt *gorm.Statement) (results []any) {
	for _, opt := range opts {
		str := stmt.Quote(opt.DBName)
		if opt.Expression != "" {
			str = opt.Expression
		}

		if opt.Collate != "" {
			str += " COLLATE " + opt.Collate
		}

		if opt.Sort != "" {
			str += " " + opt.Sort
		}
		results = append(results, clause.Expr{SQL: str})
	}
	return
}

func (m Migrator) CreateIndex(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if idx := stmt.Schema.LookIndex(name); idx != nil {
			opts := m.BuildIndexOptions(idx.Fields, stmt)
			values := []any{clause.Column{Name: idx.Name}, clause.Table{Name: stmt.Table}, opts}

			createIndexSQL := "CREATE "
			if idx.Class != "" {
				createIndexSQL += idx.Class + " "
			}
			createIndexSQL += "INDEX ?"

			if idx.Type != "" {
				createIndexSQL += " USING " + idx.Type
			}
			createIndexSQL += " ON ??"

			if idx.Where != "" {
				createIndexSQL += " WHERE " + idx.Where
			}

			return m.DB.Exec(createIndexSQL, values...).Error
		}

		return fmt.Errorf("failed to create index with name %v", name)
	})
}

func (m Migrator) HasIndex(value any, name string) bool {
	var count int
	if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if idx := stmt.Schema.LookIndex(name); idx != nil {
			name = idx.Name
		}

		if name != "" {
			return m.DB.Raw(
				"SELECT count(*) FROM sqlite_master WHERE type = ? AND tbl_name = ? AND name = ?", "index", stmt.Table, name,
			).Row().Scan(&count)
		}
		return nil
	}); err != nil {
		return false
	}
	return count > 0
}

func (m Migrator) RenameIndex(value any, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		var sql string
		if err := m.DB.Raw("SELECT sql FROM sqlite_master WHERE type = ? AND tbl_name = ? AND name = ?", "index", stmt.Table, oldName).Row().Scan(&sql); err != nil {
			return err
		}
		if sql != "" {
			return m.DB.Exec(strings.Replace(sql, oldName, newName, 1)).Error
		}
		return fmt.Errorf("failed to find index with name %v", oldName)
	})
}

func (m Migrator) DropIndex(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if idx := stmt.Schema.LookIndex(name); idx != nil {
			name = idx.Name
		}

		return m.DB.Exec("DROP INDEX ?", clause.Column{Name: name}).Error
	})
}

func buildConstraint(constraint *schema.Constraint) (sql string, results []any) {
	sql = "CONSTRAINT ? FOREIGN KEY ? REFERENCES ??"
	if constraint.OnDelete != "" {
		sql += " ON DELETE " + constraint.OnDelete
	}

	if constraint.OnUpdate != "" {
		sql += " ON UPDATE " + constraint.OnUpdate
	}

	var foreignKeys, references []any
	for _, field := range constraint.ForeignKeys {
		foreignKeys = append(foreignKeys, clause.Column{Name: field.DBName})
	}

	for _, field := range constraint.References {
		references = append(references, clause.Column{Name: field.DBName})
	}
	results = append(results, clause.Table{Name: constraint.Name}, foreignKeys, clause.Table{Name: constraint.ReferenceSchema.Table}, references)
	return
}

func (m Migrator) getRawDDL(table string) (string, error) {
	var createSQL string
	if err := m.DB.Raw("SELECT sql FROM sqlite_master WHERE type = ? AND tbl_name = ? AND name = ?", "table", table, table).Row().Scan(&createSQL); err != nil {
		return "", err
	}
	return createSQL, nil
}

func (m Migrator) recreateTable(value any, tablePtr *string,
	getCreateSQL func(rawDDL string, stmt *gorm.Statement) (sql string, sqlArgs []any, err error)) error {
	return m.recreateTableWithIndexes(value, tablePtr, getCreateSQL, false)
}

func (m Migrator) recreateTablePreservingIndexes(value any, tablePtr *string,
	getCreateSQL func(string, *gorm.Statement) (string, []any, error)) error {
	return m.recreateTableWithIndexes(value, tablePtr, getCreateSQL, true)
}

func (m Migrator) recreateTableWithIndexes(value any, tablePtr *string,
	getCreateSQL func(string, *gorm.Statement) (string, []any, error), preserveIndexes bool) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		table := stmt.Table
		if tablePtr != nil {
			table = *tablePtr
		}

		rawDDL, err := m.getRawDDL(table)
		if err != nil {
			return err
		}

		newTableName := table + "__temp"

		createSQL, sqlArgs, err := getCreateSQL(rawDDL, stmt)
		if err != nil {
			return err
		}
		if createSQL == "" {
			return nil
		}

		if preserveIndexes {
			_, open, _, err := splitAlterDDL(createSQL)
			if err != nil {
				return err
			}
			header, err := ddlTokens(createSQL[:open])
			if err != nil || len(header) != 3 || !strings.EqualFold(ddlIdentifier(header[2].text), table) {
				return fmt.Errorf("invalid DDL: table name does not match %s", table)
			}
			createSQL = "CREATE TABLE " + stmt.Quote(newTableName) + " " + createSQL[open:]
		} else {
			tableReg, err := regexp.Compile(" ('|`|\"| )" + table + "('|`|\"| ) ")
			if err != nil {
				return err
			}
			createSQL = tableReg.ReplaceAllString(createSQL, fmt.Sprintf(" `%v` ", newTableName))
		}

		createDDL, err := parseDDL(createSQL)
		if err != nil {
			return err
		}
		columns := createDDL.getColumns()
		if preserveIndexes {
			columns = nil
			fields, _, _, err := splitAlterDDL(createSQL)
			if err != nil {
				return err
			}
			for _, definition := range fields {
				tokens, err := ddlTokens(definition)
				if err != nil || len(tokens) == 0 {
					return fmt.Errorf("invalid copy column definition %q", definition)
				}
				switch strings.ToUpper(tokens[0].text) {
				case "PRIMARY", "UNIQUE", "CHECK", "CONSTRAINT", "FOREIGN":
					continue
				}
				generated := false
				for _, token := range tokens[1:] {
					if strings.EqualFold(token.text, "GENERATED") || strings.EqualFold(token.text, "AS") {
						generated = true
					}
				}
				if !generated {
					columns = append(columns, stmt.Quote(ddlIdentifier(tokens[0].text)))
				}
			}
		}

		return m.DB.Transaction(func(tx *gorm.DB) error {
			var indexes []string
			if preserveIndexes {
				// Automatic indexes belong to constraints in CREATE TABLE. Only
				// explicitly created indexes have SQL that must be replayed.
				if err := tx.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL ORDER BY name", table).Scan(&indexes).Error; err != nil {
					return err
				}
			}
			if err := tx.Exec(createSQL, sqlArgs...).Error; err != nil {
				return err
			}

			queries := []string{
				fmt.Sprintf("INSERT INTO `%v`(%v) SELECT %v FROM `%v`", newTableName, strings.Join(columns, ","), strings.Join(columns, ","), table),
				fmt.Sprintf("DROP TABLE `%v`", table),
				fmt.Sprintf("ALTER TABLE `%v` RENAME TO `%v`", newTableName, table),
			}
			queries = append(queries, indexes...)
			for _, query := range queries {
				if err := tx.Exec(query).Error; err != nil {
					return err
				}
			}
			return nil
		})
	})
}
