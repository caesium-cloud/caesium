import type { DatabaseSchemaResponse, DatabaseSchemaTable } from "@/lib/api";

export interface SqlSuggestion {
  kind: "keyword" | "table" | "column";
  value: string;
  label: string;
  detail: string;
}

export interface SqlSnippet {
  id: string;
  label: string;
  description: string;
  sql: string;
}

const SQL_KEYWORDS = [
  "SELECT",
  "FROM",
  "WHERE",
  "ORDER BY",
  "GROUP BY",
  "LIMIT",
  "JOIN",
  "LEFT JOIN",
  "INNER JOIN",
  "ON",
  "COUNT",
  "AVG",
  "MIN",
  "MAX",
  "SUM",
  "DISTINCT",
  "CASE",
  "WHEN",
  "THEN",
  "ELSE",
  "END",
  "WITH",
  "EXPLAIN",
  "VALUES",
];

const TOKEN_PATTERN = /[A-Za-z0-9_.]/;

export function getAutocompleteToken(sql: string, cursor: number) {
  let start = cursor;
  let end = cursor;

  while (start > 0 && TOKEN_PATTERN.test(sql[start - 1] ?? "")) {
    start -= 1;
  }
  while (end < sql.length && TOKEN_PATTERN.test(sql[end] ?? "")) {
    end += 1;
  }

  return {
    start,
    end,
    token: sql.slice(start, end),
  };
}

export function buildSqlSuggestions(schema: DatabaseSchemaResponse | undefined, sql: string, cursor: number): SqlSuggestion[] {
  const { token } = getAutocompleteToken(sql, cursor);
  const normalizedToken = token.toLowerCase();

  const suggestions: SqlSuggestion[] = SQL_KEYWORDS.map((keyword) => ({
    kind: "keyword",
    value: keyword,
    label: keyword,
    detail: "Keyword",
  }));

  if (schema) {
    const columnNames = new Set<string>();
    for (const table of schema.tables) {
      suggestions.push({
        kind: "table",
        value: table.name,
        label: table.name,
        detail: table.row_count != null ? `${table.row_count.toLocaleString()} rows` : "Table",
      });

      for (const column of table.columns) {
        columnNames.add(column.name);
        suggestions.push({
          kind: "column",
          value: `${table.name}.${column.name}`,
          label: `${table.name}.${column.name}`,
          detail: `${column.data_type.toLowerCase()} column`,
        });
      }
    }

    for (const columnName of columnNames) {
      suggestions.push({
        kind: "column",
        value: columnName,
        label: columnName,
        detail: "Column",
      });
    }
  }

  const filtered = suggestions.filter((suggestion) => {
    if (!normalizedToken) {
      return suggestion.kind === "keyword" || suggestion.kind === "table";
    }
    return suggestion.value.toLowerCase().includes(normalizedToken);
  });

  const unique = new Map<string, SqlSuggestion>();
  for (const suggestion of filtered) {
    unique.set(`${suggestion.kind}:${suggestion.value.toLowerCase()}`, suggestion);
  }

  return Array.from(unique.values())
    .sort((a, b) => rankSuggestion(a, normalizedToken) - rankSuggestion(b, normalizedToken) || a.label.localeCompare(b.label))
    .slice(0, 10);
}

function rankSuggestion(suggestion: SqlSuggestion, token: string) {
  const value = suggestion.value.toLowerCase();
  if (!token) {
    return suggestion.kind === "table" ? 0 : suggestion.kind === "keyword" ? 1 : 2;
  }
  if (value === token) return 0;
  if (value.startsWith(token)) return 1;
  if (value.includes(`.${token}`)) return 2;
  return 3;
}

export function applySqlSuggestion(sql: string, cursor: number, suggestion: SqlSuggestion) {
  const { start, end } = getAutocompleteToken(sql, cursor);
  const before = sql.slice(0, start);
  const after = sql.slice(end);
  const needsSpace = after.length === 0 || !/^[\s,);]/.test(after);
  const insertion = `${suggestion.value}${needsSpace ? " " : ""}`;
  return {
    sql: `${before}${insertion}${after}`,
    cursor: before.length + insertion.length,
  };
}

export function buildDefaultSnippets(schema: DatabaseSchemaResponse | undefined): SqlSnippet[] {
  const hasTable = (name: string) => schema?.tables.some((table) => table.name === name) ?? false;
  const snippets: SqlSnippet[] = [];

  if (hasTable("job_runs") && hasTable("jobs")) {
    snippets.push({
      id: "recent-runs",
      label: "Recent runs",
      description: "Latest job runs with aliases and durations",
      sql: [
        "SELECT jr.id, j.alias, jr.status, jr.started_at, jr.completed_at",
        "FROM job_runs jr",
        "JOIN jobs j ON j.id = jr.job_id",
        "ORDER BY jr.started_at DESC",
        "LIMIT 25;",
      ].join("\n"),
    });
  }

  if (hasTable("task_runs") && hasTable("tasks")) {
    snippets.push({
      id: "failed-tasks",
      label: "Failed tasks",
      description: "Tasks that failed most recently",
      sql: [
        "SELECT tr.id, tr.job_run_id, t.name, tr.status, tr.error, tr.completed_at",
        "FROM task_runs tr",
        "JOIN tasks t ON t.id = tr.task_id",
        "WHERE tr.status = 'failed'",
        "ORDER BY tr.completed_at DESC",
        "LIMIT 25;",
      ].join("\n"),
    });
  }

  if (hasTable("jobs")) {
    snippets.push({
      id: "jobs-overview",
      label: "Jobs overview",
      description: "Jobs and pause state",
      sql: [
        "SELECT id, alias, paused, created_at, updated_at",
        "FROM jobs",
        "ORDER BY updated_at DESC",
        "LIMIT 50;",
      ].join("\n"),
    });
  }

  if (snippets.length === 0 && schema?.tables[0]) {
    snippets.push({
      id: "first-table",
      label: `Browse ${schema.tables[0].name}`,
      description: "Inspect the first available table",
      sql: buildSelectSnippet(schema.tables[0]),
    });
  }

  return snippets;
}

export function buildSelectSnippet(table: DatabaseSchemaTable) {
  return [
    `SELECT *`,
    `FROM ${table.name}`,
    `LIMIT 50;`,
  ].join("\n");
}

export function buildCountSnippet(table: DatabaseSchemaTable) {
  return `SELECT COUNT(*) AS total FROM ${table.name};`;
}
