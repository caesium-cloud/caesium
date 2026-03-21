import { useDeferredValue, useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  ArrowRight,
  Copy,
  Database,
  History,
  Play,
  RefreshCw,
  Search,
  TerminalSquare,
} from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ApiError, api, type DatabaseQueryResponse, type DatabaseSchemaTable } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  applySqlSuggestion,
  buildCountSnippet,
  buildDefaultSnippets,
  buildSelectSnippet,
  buildSqlSuggestions,
} from "./sqlConsole";

const STORAGE_KEYS = {
  sql: "caesium.database.console.sql",
  history: "caesium.database.console.history",
  limit: "caesium.database.console.limit",
};

const DEFAULT_LIMIT = 200;

export function DatabaseConsolePage() {
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const editorSurfaceRef = useRef<HTMLDivElement | null>(null);
  const initialStoredSqlRef = useRef(readStoredString(STORAGE_KEYS.sql, ""));
  const [sql, setSql] = useState(() => initialStoredSqlRef.current);
  const [history, setHistory] = useState<string[]>(() => readStoredJSON(STORAGE_KEYS.history, []));
  const [limit, setLimit] = useState<number>(() => readStoredNumber(STORAGE_KEYS.limit, DEFAULT_LIMIT));
  const [schemaSearch, setSchemaSearch] = useState("");
  const [cursorPosition, setCursorPosition] = useState(0);
  const [autocompleteVisible, setAutocompleteVisible] = useState(false);
  const [selectedSuggestionIndex, setSelectedSuggestionIndex] = useState(0);
  const [autocompletePosition, setAutocompletePosition] = useState({ top: 16, left: 16 });
  const [hasSeededInitialQuery, setHasSeededInitialQuery] = useState(
    () => initialStoredSqlRef.current.trim().length > 0,
  );

  const deferredSchemaSearch = useDeferredValue(schemaSearch);

  const {
    data: schema,
    isLoading: schemaLoading,
    error: schemaError,
    refetch,
  } = useQuery({
    queryKey: ["database-schema"],
    queryFn: api.getDatabaseSchema,
    staleTime: 60_000,
  });

  const runQuery = useMutation({
    mutationFn: api.queryDatabase,
    onSuccess: (_result, variables) => {
      toast.success("Query completed");
      persistString(STORAGE_KEYS.sql, variables.sql);
      const nextHistory = [variables.sql, ...history.filter((entry) => entry !== variables.sql)].slice(0, 12);
      setHistory(nextHistory);
      persistJSON(STORAGE_KEYS.history, nextHistory);
    },
    onError: (error: Error) => {
      toast.error(error.message);
    },
  });

  const snippets = useMemo(() => buildDefaultSnippets(schema), [schema]);
  const suggestions = useMemo(
    () => buildSqlSuggestions(schema, sql, cursorPosition),
    [schema, sql, cursorPosition],
  );

  const filteredTables = useMemo(() => {
    if (!schema) return [];
    const filter = deferredSchemaSearch.trim().toLowerCase();
    if (!filter) return schema.tables;

    return schema.tables.filter((table) =>
      table.name.toLowerCase().includes(filter) ||
      table.columns.some((column) =>
        column.name.toLowerCase().includes(filter) || column.data_type.toLowerCase().includes(filter),
      ),
    );
  }, [deferredSchemaSearch, schema]);

  useEffect(() => {
    if (!hasSeededInitialQuery && !sql && snippets.length > 0) {
      setSql(snippets[0].sql);
      persistString(STORAGE_KEYS.sql, snippets[0].sql);
      setHasSeededInitialQuery(true);
    }
  }, [hasSeededInitialQuery, snippets, sql]);

  useEffect(() => {
    if (selectedSuggestionIndex >= suggestions.length) {
      setSelectedSuggestionIndex(0);
    }
  }, [selectedSuggestionIndex, suggestions.length]);

  useEffect(() => {
    if (!autocompleteVisible) {
      return;
    }

    const textarea = textareaRef.current;
    const surface = editorSurfaceRef.current;
    if (!textarea || !surface) {
      return;
    }

    const updatePosition = () => {
      const caret = getTextareaCaretPosition(textarea, cursorPosition);
      const popupWidth = 320;
      const popupHeight = Math.min(240, suggestions.length * 44 + 16);
      const surfaceRect = surface.getBoundingClientRect();
      const top = clamp(caret.top + 24, 12, Math.max(12, surfaceRect.height - popupHeight - 12));
      const left = clamp(caret.left, 12, Math.max(12, surfaceRect.width - popupWidth - 12));
      setAutocompletePosition({ top, left });
    };

    updatePosition();
    textarea.addEventListener("scroll", updatePosition);
    window.addEventListener("resize", updatePosition);

    return () => {
      textarea.removeEventListener("scroll", updatePosition);
      window.removeEventListener("resize", updatePosition);
    };
  }, [autocompleteVisible, cursorPosition, sql, suggestions.length]);

  const result = runQuery.data;
  const databaseConsoleUnavailable = schemaError instanceof ApiError && schemaError.status === 404;

  function execute(sqlToRun = sql) {
    const trimmed = sqlToRun.trim();
    if (!trimmed) {
      toast.error("Enter a query first");
      return;
    }
    runQuery.mutate({ sql: trimmed, limit });
  }

  function applySnippet(nextSql: string) {
    setSql(nextSql);
    setCursorPosition(nextSql.length);
    setAutocompleteVisible(false);
    persistString(STORAGE_KEYS.sql, nextSql);
    requestAnimationFrame(() => {
      textareaRef.current?.focus();
      textareaRef.current?.setSelectionRange(nextSql.length, nextSql.length);
    });
  }

  function acceptSuggestion(index = selectedSuggestionIndex) {
    if (!suggestions[index]) return;
    const applied = applySqlSuggestion(sql, cursorPosition, suggestions[index]);
    setSql(applied.sql);
    setCursorPosition(applied.cursor);
    setAutocompleteVisible(false);
    persistString(STORAGE_KEYS.sql, applied.sql);
    requestAnimationFrame(() => {
      textareaRef.current?.focus();
      textareaRef.current?.setSelectionRange(applied.cursor, applied.cursor);
    });
  }

  function copyResults() {
    if (!result) return;
    const rows = result.rows.map((row) =>
      result.columns.reduce<Record<string, unknown>>((acc, column, index) => {
        acc[column.name] = row[index];
        return acc;
      }, {}),
    );
    void navigator.clipboard.writeText(JSON.stringify(rows, null, 2));
    toast.success("Results copied as JSON");
  }

  function handleEditorKeyDown(event: React.KeyboardEvent<HTMLTextAreaElement>) {
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      event.preventDefault();
      execute();
      return;
    }
    if ((event.metaKey || event.ctrlKey) && event.key === " ") {
      event.preventDefault();
      setAutocompleteVisible(true);
      return;
    }
    if (event.key === "Tab" && autocompleteVisible && suggestions.length > 0) {
      event.preventDefault();
      acceptSuggestion();
      return;
    }
    if (event.key === "Escape") {
      setAutocompleteVisible(false);
      return;
    }
    if (autocompleteVisible && suggestions.length > 0 && event.key === "ArrowDown") {
      event.preventDefault();
      setSelectedSuggestionIndex((current) => (current + 1) % suggestions.length);
      return;
    }
    if (autocompleteVisible && suggestions.length > 0 && event.key === "ArrowUp") {
      event.preventDefault();
      setSelectedSuggestionIndex((current) => (current - 1 + suggestions.length) % suggestions.length);
    }
  }

  function handleSqlChange(nextValue: string, selectionStart: number) {
    setSql(nextValue);
    setCursorPosition(selectionStart);
    setAutocompleteVisible(true);
    persistString(STORAGE_KEYS.sql, nextValue);
  }

  function handleLimitChange(nextValue: string) {
    const parsed = Number.parseInt(nextValue, 10);
    const nextLimit = Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_LIMIT;
    setLimit(nextLimit);
    persistString(STORAGE_KEYS.limit, String(nextLimit));
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
        <div className="space-y-2">
          <div className="flex items-center gap-3">
            <div className="rounded-2xl border border-primary/30 bg-primary/10 p-3 shadow-[0_0_40px_rgba(0,180,216,0.18)]">
              <TerminalSquare className="h-5 w-5 text-primary" />
            </div>
            <div>
              <h1 className="text-2xl font-bold tracking-tight">Database Console</h1>
              <p className="text-sm text-muted-foreground">
                Inspect Caesium&apos;s embedded persistence layer with schema-aware, read-only SQL.
              </p>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <Badge variant="secondary">{schema?.dialect ?? "database"}</Badge>
            {schema?.version ? <Badge variant="outline">v{schema.version}</Badge> : null}
            <Badge variant="outline" className="border-emerald-500/30 text-emerald-300">
              read-only
            </Badge>
            <span className="rounded-full border border-border/70 bg-background/80 px-3 py-1 font-mono text-muted-foreground">
              Ctrl/Cmd+Enter run
            </span>
            <span className="rounded-full border border-border/70 bg-background/80 px-3 py-1 font-mono text-muted-foreground">
              Ctrl/Cmd+Space autocomplete
            </span>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <Button
            variant="outline"
            size="sm"
            onClick={() => refetch()}
            disabled={schemaLoading || databaseConsoleUnavailable}
          >
            <RefreshCw className={cn("mr-1.5 h-3.5 w-3.5", schemaLoading && "animate-spin")} />
            Refresh schema
          </Button>
          <Button size="sm" onClick={() => execute()} disabled={runQuery.isPending || databaseConsoleUnavailable}>
            <Play className="mr-1.5 h-3.5 w-3.5" />
            Run query
          </Button>
        </div>
      </div>

      {databaseConsoleUnavailable ? (
        <Card className="border-amber-500/30 bg-amber-500/5">
          <CardHeader>
            <CardTitle className="text-base">Database Console Disabled</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-sm text-muted-foreground">
            <p>The database console endpoints are not enabled on this server.</p>
            <p className="font-mono text-xs text-foreground">
              Set <code>CAESIUM_DATABASE_CONSOLE_ENABLED=true</code> and restart Caesium to expose the operator database console.
            </p>
          </CardContent>
        </Card>
      ) : null}

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1.45fr)_360px]">
        <div className="space-y-6">
          <Card className="overflow-hidden border-border/70 bg-card/90 shadow-2xl shadow-black/10">
            <CardHeader className="border-b border-border/70 bg-[linear-gradient(135deg,rgba(15,23,42,0.98),rgba(30,41,59,0.96))] p-0">
              <div className="flex items-center justify-between border-b border-white/10 px-4 py-3">
                <div className="flex items-center gap-2">
                  <span className="h-3 w-3 rounded-full bg-rose-400/90" />
                  <span className="h-3 w-3 rounded-full bg-amber-400/90" />
                  <span className="h-3 w-3 rounded-full bg-emerald-400/90" />
                </div>
                <div className="flex items-center gap-2 text-xs uppercase tracking-[0.3em] text-slate-400">
                  <Database className="h-3.5 w-3.5" />
                  Operator SQL
                </div>
              </div>
              <div className="flex items-center justify-between px-4 py-3 text-sm text-slate-300">
                <div className="flex items-center gap-2 font-mono">
                  <span className="text-emerald-400">caesium@db</span>
                  <ArrowRight className="h-3.5 w-3.5 text-slate-500" />
                  <span>{schema?.dialect ?? "query"}</span>
                </div>
                <div className="flex items-center gap-2 text-xs">
                  <label className="font-medium text-slate-400" htmlFor="query-limit">
                    Row cap
                  </label>
                  <input
                    id="query-limit"
                    type="number"
                    min={1}
                    max={1000}
                    value={limit}
                    onChange={(event) => handleLimitChange(event.target.value)}
                    className="w-20 rounded-md border border-white/10 bg-slate-950/70 px-2 py-1 font-mono text-slate-100 outline-none ring-0 transition focus:border-primary"
                  />
                </div>
              </div>
            </CardHeader>
            <CardContent className="space-y-4 bg-[linear-gradient(180deg,rgba(2,6,23,0.98),rgba(15,23,42,0.98))] p-4">
              <div
                ref={editorSurfaceRef}
                className="relative rounded-2xl border border-white/10 bg-slate-950/70 shadow-[inset_0_1px_0_rgba(255,255,255,0.04)]"
              >
                <div className="border-b border-white/10 px-4 py-2 text-xs uppercase tracking-[0.28em] text-slate-500">
                  Query
                </div>
                <textarea
                  ref={textareaRef}
                  aria-label="SQL query editor"
                  value={sql}
                  onChange={(event) => handleSqlChange(event.target.value, event.target.selectionStart)}
                  onClick={(event) => setCursorPosition(event.currentTarget.selectionStart)}
                  onKeyDown={handleEditorKeyDown}
                  onFocus={() => setAutocompleteVisible(true)}
                  onBlur={() => {
                    window.setTimeout(() => setAutocompleteVisible(false), 120);
                  }}
                  onSelect={(event) => setCursorPosition(event.currentTarget.selectionStart)}
                  spellCheck={false}
                  disabled={databaseConsoleUnavailable}
                  className="min-h-[220px] w-full resize-y bg-transparent px-4 py-4 font-mono text-sm leading-6 text-slate-100 outline-none placeholder:text-slate-500"
                  placeholder="SELECT * FROM job_runs ORDER BY started_at DESC LIMIT 25;"
                />

                {autocompleteVisible && suggestions.length > 0 ? (
                  <div
                    className="absolute z-20 w-80 overflow-hidden rounded-2xl border border-primary/20 bg-slate-950/95 shadow-[0_24px_80px_rgba(2,6,23,0.45)] backdrop-blur"
                    style={{ top: autocompletePosition.top, left: autocompletePosition.left }}
                  >
                    <div className="max-h-60 overflow-auto p-2">
                      <div className="grid gap-1">
                        {suggestions.map((suggestion, index) => (
                          <button
                            key={`${suggestion.kind}-${suggestion.value}`}
                            type="button"
                            onMouseDown={(event) => {
                              event.preventDefault();
                              acceptSuggestion(index);
                            }}
                            className={cn(
                              "flex items-center justify-between rounded-xl px-3 py-2 text-left font-mono text-sm transition",
                              index === selectedSuggestionIndex
                                ? "bg-primary/15 text-primary"
                                : "text-slate-200 hover:bg-white/5",
                            )}
                          >
                            <span className="truncate pr-3">{suggestion.label}</span>
                            <span className="shrink-0 text-[10px] uppercase tracking-[0.2em] text-slate-500">
                              {suggestion.detail}
                            </span>
                          </button>
                        ))}
                      </div>
                    </div>
                  </div>
                ) : null}
              </div>

              <div className="flex flex-wrap gap-2">
                {snippets.map((snippet) => (
                  <button
                    key={snippet.id}
                    type="button"
                    onClick={() => applySnippet(snippet.sql)}
                    disabled={databaseConsoleUnavailable}
                    className="rounded-full border border-white/10 bg-white/5 px-3 py-1.5 text-left transition hover:border-primary/40 hover:bg-primary/10"
                  >
                    <div className="text-sm font-medium text-slate-100">{snippet.label}</div>
                    <div className="text-xs text-slate-400">{snippet.description}</div>
                  </button>
                ))}
              </div>
            </CardContent>
          </Card>

          <Card className="border-border/70 bg-card/90">
            <CardHeader className="flex flex-row items-center justify-between gap-4">
              <div>
                <CardTitle className="text-base">Results</CardTitle>
                <p className="mt-1 text-sm text-muted-foreground">
                  Query output is capped client-side for readability while remaining read-only on the server.
                </p>
              </div>
              {result ? (
                <div className="flex items-center gap-2">
                  <Badge variant="secondary">{result.statement_type}</Badge>
                  <Badge variant="outline">{result.duration_ms} ms</Badge>
                  <Badge variant="outline">{result.row_count} rows</Badge>
                  {result.truncated ? <Badge variant="outline">truncated at {result.limit}</Badge> : null}
                  <Button variant="outline" size="sm" onClick={copyResults}>
                    <Copy className="mr-1.5 h-3.5 w-3.5" />
                    Copy JSON
                  </Button>
                </div>
              ) : null}
            </CardHeader>
            <CardContent>
              {runQuery.isPending ? (
                <div className="rounded-xl border border-primary/20 bg-primary/5 px-4 py-6 text-sm text-muted-foreground">
                  Running query against {schema?.dialect ?? "database"}...
                </div>
              ) : null}

              {!runQuery.isPending && runQuery.error ? (
                <div className="rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-4 text-sm text-destructive">
                  {runQuery.error.message}
                </div>
              ) : null}

              {!runQuery.isPending && !runQuery.error && result ? (
                <ResultTable result={result} />
              ) : null}

              {!runQuery.isPending && !runQuery.error && !result && !databaseConsoleUnavailable ? (
                <div className="rounded-xl border border-dashed border-border/80 bg-muted/20 px-4 py-10 text-center text-sm text-muted-foreground">
                  No query executed yet. Start with a snippet or browse a table from the schema explorer.
                </div>
              ) : null}
            </CardContent>
          </Card>
        </div>

        <div className="space-y-6">
          <Card className="border-border/70 bg-card/90">
            <CardHeader className="pb-3">
              <CardTitle className="text-base">Schema Explorer</CardTitle>
              <p className="text-sm text-muted-foreground">
                Live table metadata from the connected database.
              </p>
            </CardHeader>
            <CardContent className="space-y-4">
              <label className="relative block">
                <Search className="pointer-events-none absolute left-3 top-3 h-4 w-4 text-muted-foreground" />
                <input
                  aria-label="Search tables"
                  value={schemaSearch}
                  onChange={(event) => setSchemaSearch(event.target.value)}
                  placeholder="Search tables or columns"
                  className="w-full rounded-xl border border-input bg-background px-10 py-2.5 text-sm outline-none transition focus:border-primary"
                />
              </label>

              <div className="max-h-[640px] space-y-3 overflow-auto pr-1">
                {schemaLoading ? (
                  <div className="rounded-xl border border-dashed border-border/80 px-4 py-8 text-center text-sm text-muted-foreground">
                    Loading schema...
                  </div>
                ) : null}

                {schemaError && !databaseConsoleUnavailable ? (
                  <div className="rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-4 text-sm text-destructive">
                    {schemaError.message}
                  </div>
                ) : null}

                {!schemaLoading && !schemaError && filteredTables.length === 0 ? (
                  <div className="rounded-xl border border-dashed border-border/80 px-4 py-8 text-center text-sm text-muted-foreground">
                    No tables match that filter.
                  </div>
                ) : null}

                {filteredTables.map((table) => (
                  <SchemaTableCard
                    key={table.name}
                    table={table}
                    onBrowse={() => applySnippet(buildSelectSnippet(table))}
                    onCount={() => applySnippet(buildCountSnippet(table))}
                  />
                ))}
              </div>
            </CardContent>
          </Card>

          <Card className="border-border/70 bg-card/90">
            <CardHeader className="pb-3">
              <CardTitle className="flex items-center gap-2 text-base">
                <History className="h-4 w-4" />
                Recent Queries
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2">
              {history.length === 0 ? (
                <div className="rounded-xl border border-dashed border-border/80 px-4 py-6 text-sm text-muted-foreground">
                  Executed queries will appear here for quick recall.
                </div>
              ) : null}
              {history.map((entry, index) => (
                <button
                  key={`${index}-${entry}`}
                  type="button"
                  onClick={() => applySnippet(entry)}
                  className="w-full rounded-xl border border-border/70 bg-background/60 px-3 py-3 text-left transition hover:border-primary/40 hover:bg-primary/5"
                >
                  <div className="line-clamp-3 font-mono text-xs text-foreground">{entry}</div>
                </button>
              ))}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  );
}

function ResultTable({ result }: { result: DatabaseQueryResponse }) {
  if (result.columns.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border/80 px-4 py-8 text-center text-sm text-muted-foreground">
        Query completed without a tabular result set.
      </div>
    );
  }

  return (
    <div className="overflow-hidden rounded-xl border border-border/70">
      <div className="max-h-[520px] overflow-auto">
        <Table>
          <TableHeader className="sticky top-0 z-10 bg-card">
            <TableRow>
              {result.columns.map((column) => (
                <TableHead key={column.name} className="whitespace-nowrap font-mono text-xs uppercase tracking-[0.2em]">
                  <div>{column.name}</div>
                  <div className="mt-1 text-[10px] font-normal text-muted-foreground">{column.data_type || "unknown"}</div>
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {result.rows.length === 0 ? (
              <TableRow>
                <TableCell colSpan={result.columns.length} className="h-24 text-center text-muted-foreground">
                  Query returned no rows.
                </TableCell>
              </TableRow>
            ) : null}
            {result.rows.map((row, rowIndex) => (
              <TableRow key={rowIndex}>
                {row.map((cell, cellIndex) => (
                  <TableCell key={`${rowIndex}-${cellIndex}`} className="max-w-[360px] align-top font-mono text-xs">
                    <div className="break-words whitespace-pre-wrap">{formatCellValue(cell)}</div>
                  </TableCell>
                ))}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

function SchemaTableCard({
  table,
  onBrowse,
  onCount,
}: {
  table: DatabaseSchemaTable;
  onBrowse: () => void;
  onCount: () => void;
}) {
  return (
    <div className="rounded-2xl border border-border/70 bg-background/60 p-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <div className="font-mono text-sm font-medium text-foreground">{table.name}</div>
          {table.row_count == null ? (
            <div className="mt-1 text-xs text-muted-foreground">Row count on demand</div>
          ) : (
            <div className="mt-1 text-xs text-muted-foreground">{table.row_count.toLocaleString()} rows</div>
          )}
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={onBrowse}>
            Query
          </Button>
          <Button variant="ghost" size="sm" onClick={onCount}>
            Count
          </Button>
        </div>
      </div>
      <div className="mt-3 grid gap-2">
        {table.columns.map((column) => (
          <div key={column.name} className="flex items-center justify-between gap-3 rounded-xl border border-border/60 px-3 py-2">
            <div className="min-w-0">
              <div className="truncate font-mono text-xs text-foreground">{column.name}</div>
              <div className="truncate text-[11px] text-muted-foreground">{column.data_type.toLowerCase()}</div>
            </div>
            <div className="flex shrink-0 gap-2">
              {column.primary_key ? <Badge variant="secondary">pk</Badge> : null}
              {!column.nullable ? <Badge variant="outline">not null</Badge> : null}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

function formatCellValue(value: unknown) {
  if (value == null) return "NULL";
  if (typeof value === "string") return value;
  return JSON.stringify(value);
}

function getTextareaCaretPosition(textarea: HTMLTextAreaElement, selectionStart: number) {
  const mirror = document.createElement("div");
  const style = window.getComputedStyle(textarea);
  const properties = [
    "boxSizing",
    "width",
    "height",
    "overflowX",
    "overflowY",
    "borderTopWidth",
    "borderRightWidth",
    "borderBottomWidth",
    "borderLeftWidth",
    "paddingTop",
    "paddingRight",
    "paddingBottom",
    "paddingLeft",
    "fontStyle",
    "fontVariant",
    "fontWeight",
    "fontStretch",
    "fontSize",
    "fontSizeAdjust",
    "lineHeight",
    "fontFamily",
    "letterSpacing",
    "textTransform",
    "textIndent",
    "textDecoration",
    "textAlign",
    "whiteSpace",
    "wordSpacing",
    "wordBreak",
  ] as const;

  mirror.style.position = "absolute";
  mirror.style.visibility = "hidden";
  mirror.style.pointerEvents = "none";
  mirror.style.whiteSpace = "pre-wrap";
  mirror.style.wordWrap = "break-word";
  mirror.style.overflow = "hidden";

  for (const property of properties) {
    mirror.style[property] = style[property];
  }

  mirror.textContent = textarea.value.slice(0, selectionStart);

  const marker = document.createElement("span");
  marker.textContent = textarea.value.slice(selectionStart, selectionStart + 1) || " ";
  mirror.appendChild(marker);

  document.body.appendChild(mirror);

  const top =
    marker.offsetTop -
    textarea.scrollTop +
    cssPixels(style.borderTopWidth) +
    cssPixels(style.paddingTop);
  const left =
    marker.offsetLeft -
    textarea.scrollLeft +
    cssPixels(style.borderLeftWidth) +
    cssPixels(style.paddingLeft);

  document.body.removeChild(mirror);
  return { top, left };
}

function clamp(value: number, min: number, max: number) {
  return Math.min(Math.max(value, min), max);
}

function cssPixels(value: string) {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function readStoredString(key: string, fallback: string) {
  if (typeof window === "undefined") return fallback;
  return window.localStorage.getItem(key) ?? fallback;
}

function readStoredNumber(key: string, fallback: number) {
  const raw = readStoredString(key, String(fallback));
  const parsed = Number.parseInt(raw, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function readStoredJSON<T>(key: string, fallback: T): T {
  if (typeof window === "undefined") return fallback;
  const raw = window.localStorage.getItem(key);
  if (!raw) return fallback;
  try {
    return JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}

function persistString(key: string, value: string) {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(key, value);
}

function persistJSON(key: string, value: unknown) {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(key, JSON.stringify(value));
}
