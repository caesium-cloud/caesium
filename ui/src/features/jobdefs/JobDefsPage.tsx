import { useState, useEffect } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, type DiffResponse, type LintResponse } from "@/lib/api";
import { FileCode2, Play, CheckCircle2, XCircle, Upload, GitBranch, FileWarning, Info } from "lucide-react";
import { Button } from "@/components/ui/button";
import CodeMirror from "@uiw/react-codemirror";
import { yaml as yamlLang } from "@codemirror/lang-yaml";
import { linter, type Diagnostic } from "@codemirror/lint";
import { EditorView } from "@codemirror/view";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Card } from "@/components/ui/card";

const EXAMPLE_YAML = `apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl-warehouse
  labels:
    team: data-platform
    tier: critical
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
    timezone: "UTC"
steps:
  - name: extract.users
    image: ghcr.io/cs/postgres-extractor:1.4
    command: ["python", "/app/extract.py", "--table=users"]
  - name: extract.orders
    image: ghcr.io/cs/postgres-extractor:1.4
    command: ["python", "/app/extract.py", "--table=orders"]
  - name: transform.users
    image: ghcr.io/cs/dbt:1.7
    dependsOn: [extract.users]
    command: ["dbt", "run", "--select", "users"]
  - name: transform.orders
    image: ghcr.io/cs/dbt:1.7
    dependsOn: [extract.orders]
    command: ["dbt", "run", "--select", "orders"]
  - name: load.warehouse
    image: snowflake/snowsql
    dependsOn: [transform.users, transform.orders]
    command: ["snowsql", "-f", "/sql/load.sql"]
`;

// Simple custom theme for the editor to match our brand
const customTheme = EditorView.theme({
  "&": {
    backgroundColor: "transparent !important",
    color: "hsl(var(--text-1))",
    fontSize: "12.5px",
    fontFamily: "var(--font-mono)",
  },
  ".cm-content": {
    caretColor: "hsl(var(--cyan-glow))",
  },
  "&.cm-focused .cm-cursor": {
    borderLeftColor: "hsl(var(--cyan-glow))",
  },
  ".cm-gutters": {
    backgroundColor: "hsl(var(--obsidian) / 0.5)",
    color: "hsl(var(--text-4))",
    borderRight: "1px solid hsl(var(--graphite))",
  },
  ".cm-activeLineGutter": {
    backgroundColor: "transparent",
    color: "hsl(var(--cyan-glow))",
  },
  ".cm-activeLine": {
    backgroundColor: "hsl(var(--cyan) / 0.05)",
  },
}, { dark: true });

export function JobDefsPage() {
  const queryClient = useQueryClient();
  const [yaml, setYaml] = useState(EXAMPLE_YAML);
  const [tab, setTab] = useState("editor");
  const [lintResult, setLintResult] = useState<LintResponse>({ errors: [], warnings: [], summary: { steps: "" } });
  const [diffResult, setDiffResult] = useState<DiffResponse | null>(null);
  
  // Debounced API calls
  useEffect(() => {
    const timer = setTimeout(async () => {
      try {
        const lr = await api.lintJobDef(yaml);
        setLintResult(lr);
        
        // Only get diff if lint passes
        if (lr.errors && lr.errors.length === 0) {
          const dr = await api.diffJobDef(yaml);
          setDiffResult(dr);
        } else {
          setDiffResult(null);
        }
      } catch {
        // Ignore API errors silently in the background
      }
    }, 300);
    return () => clearTimeout(timer);
  }, [yaml]);

  const applyMutation = useMutation({
    mutationFn: () => api.applyJobDef(yaml),
    onSuccess: (data) => {
      toast.success(`Applied successfully (${data.applied} jobs)`);
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
      queryClient.invalidateQueries({ queryKey: ["atoms"] });
      queryClient.invalidateQueries({ queryKey: ["triggers"] });
      queryClient.invalidateQueries({ queryKey: ["stats"] });
      // re-trigger diff to clear changes
      api.diffJobDef(yaml).then(dr => setDiffResult(dr)).catch(() => {});
    },
    onError: (err) => {
      toast.error(err instanceof Error ? err.message : "Failed to apply job definition");
    },
  });

  const customLinter = linter((view) => {
    const diagnostics: Diagnostic[] = [];
    if (lintResult.errors) {
      lintResult.errors.forEach(err => {
        const lineNo = err.line && err.line <= view.state.doc.lines ? err.line : 1;
        const line = view.state.doc.line(lineNo);
        diagnostics.push({
          from: line.from,
          to: line.to,
          severity: "error",
          message: err.message,
        });
      });
    }
    return diagnostics;
  });

  const lineCount = yaml.split("\n").length;
  const diffCount = diffResult ? (diffResult.added?.length || 0) + (diffResult.removed?.length || 0) + (diffResult.modified?.length || 0) : 0;
  const hasErrors = lintResult.errors && lintResult.errors.length > 0;

  return (
    <div className="space-y-6 pb-12">
      <div className="flex flex-col sm:flex-row sm:items-end justify-between gap-4">
        <div>
          <div className="text-[10px] font-bold uppercase tracking-widest text-gold/85 mb-1">Declarative Manifests</div>
          <h1 className="text-2xl font-bold tracking-tight m-0 leading-tight">Job Definitions</h1>
          <p className="text-sm text-text-3 mt-1 flex items-center gap-1.5">
            Lint, diff, and apply YAML manifests <span className="text-text-4">·</span> <code className="font-mono text-cyan-glow text-xs bg-cyan-glow/10 px-1 py-0.5 rounded">caesium job apply</code>
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" className="bg-transparent border-graphite/50 text-text-2">
            <Upload className="h-3.5 w-3.5 mr-1.5" />
            Upload
          </Button>
          <Button variant="outline" size="sm" className="bg-transparent border-graphite/50 text-text-2">
            <GitBranch className="h-3.5 w-3.5 mr-1.5" />
            Git sync
          </Button>
          <Button 
            size="sm" 
            className="bg-cyan-glow text-midnight hover:bg-cyan-dim disabled:opacity-50"
            onClick={() => applyMutation.mutate()}
            disabled={hasErrors || applyMutation.isPending || !yaml.trim()}
          >
            <Play className="h-3.5 w-3.5 mr-1.5" />
            {applyMutation.isPending ? "Applying..." : "Apply definition"}
          </Button>
        </div>
      </div>

      <div className="bg-obsidian border border-graphite/50 rounded-lg p-[3px] w-fit">
        <Tabs value={tab} onValueChange={setTab}>
          <TabsList className="bg-transparent h-auto p-0 space-x-1">
            <TabsTrigger 
              value="editor" 
              className="data-[state=active]:bg-cyan/15 data-[state=active]:text-cyan-glow data-[state=active]:border-cyan/30 border border-transparent px-3.5 py-1.5 text-xs font-medium text-text-2 transition-all"
            >
              Editor
              {hasErrors && (
                <span className="ml-2 font-mono text-[10px] px-1.5 py-0.5 rounded-full bg-danger/20 text-danger">
                  {lintResult.errors.length}
                </span>
              )}
            </TabsTrigger>
            <TabsTrigger 
              value="diff" 
              className="data-[state=active]:bg-cyan/15 data-[state=active]:text-cyan-glow data-[state=active]:border-cyan/30 border border-transparent px-3.5 py-1.5 text-xs font-medium text-text-2 transition-all"
            >
              Diff vs server
              {diffCount > 0 && !hasErrors && (
                <span className="ml-2 font-mono text-[10px] px-1.5 py-0.5 rounded-full bg-gold/20 text-gold">
                  {diffCount}
                </span>
              )}
            </TabsTrigger>
            <TabsTrigger 
              value="history" 
              disabled
              title="Coming in v1.1"
              className="disabled:opacity-50 border border-transparent px-3.5 py-1.5 text-xs font-medium text-text-2 transition-all cursor-not-allowed"
            >
              History
            </TabsTrigger>
          </TabsList>
        </Tabs>
      </div>

      <div className="grid lg:grid-cols-[minmax(0,1fr)_320px] gap-4 items-start">
        <div className="flex flex-col gap-4">
          {tab === "editor" ? (
            <>
              <Card className="bg-midnight/30 border-graphite/50 overflow-hidden shadow-lg">
                <div className="flex justify-between items-center px-4 py-2.5 border-b border-graphite/50 bg-obsidian/50">
                  <div className="flex items-center gap-2">
                    <FileCode2 className="h-3.5 w-3.5 text-text-3" />
                    <span className="font-mono text-xs text-text-2">job.yaml</span>
                  </div>
                  <div className="flex items-center gap-3">
                    <span className="font-mono text-[10px] text-text-4">
                      {lineCount} lines <span className="mx-1">·</span> {(yaml.length / 1024).toFixed(1)} KB
                    </span>
                    <button 
                      onClick={() => setYaml(EXAMPLE_YAML)} 
                      className="text-[11px] text-text-3 hover:text-text-1 transition-colors"
                    >
                      Reset example
                    </button>
                  </div>
                </div>
                
                <div className="min-h-[420px] bg-void">
                  <CodeMirror
                    value={yaml}
                    height="420px"
                    extensions={[yamlLang(), customTheme, customLinter]}
                    onChange={(val) => setYaml(val)}
                    basicSetup={{
                      lineNumbers: true,
                      foldGutter: true,
                      dropCursor: true,
                      allowMultipleSelections: true,
                      indentOnInput: true,
                      bracketMatching: true,
                      closeBrackets: true,
                      autocompletion: true,
                      highlightActiveLine: true,
                      highlightSelectionMatches: true,
                    }}
                  />
                </div>
              </Card>

              {/* Lint feedback */}
              <Card className="bg-midnight/30 border-graphite/50 overflow-hidden">
                <div className="px-4 py-2.5 border-b border-graphite/50 flex justify-between items-center bg-obsidian/30">
                  <div className="text-xs font-medium flex items-center gap-2">
                    {hasErrors ? (
                      <>
                        <XCircle className="h-3.5 w-3.5 text-danger" />
                        <span className="text-danger">{lintResult.errors.length} validation {lintResult.errors.length === 1 ? "error" : "errors"}</span>
                      </>
                    ) : (
                      <>
                        <CheckCircle2 className="h-3.5 w-3.5 text-success" />
                        <span className="text-success">Schema valid</span>
                        <span className="text-text-4 mx-1">·</span>
                        <span className="text-text-3">{lintResult.summary?.steps || "No steps"}</span>
                      </>
                    )}
                  </div>
                  <span className="font-mono text-[10px] text-text-4">live lint</span>
                </div>
                
                {hasErrors && (
                  <div className="p-3 flex flex-col gap-2">
                    {lintResult.errors.map((e, i) => (
                      <div key={i} className="flex items-start gap-2.5 text-xs">
                        <FileWarning className="h-3.5 w-3.5 text-danger mt-0.5 flex-shrink-0" />
                        <span className="text-danger/90">{e.message}</span>
                        {e.line != null && (
                          <span className="font-mono text-[10px] text-text-4 ml-auto whitespace-nowrap mt-0.5">line {e.line}</span>
                        )}
                      </div>
                    ))}
                  </div>
                )}
                
                {!hasErrors && lintResult.summary?.steps && (
                  <div className="p-3">
                    <div className="flex items-start gap-2.5 text-xs">
                      <Info className="h-3.5 w-3.5 text-success mt-0.5 flex-shrink-0" />
                      <span className="text-text-2">{lintResult.summary.steps}</span>
                    </div>
                  </div>
                )}
              </Card>
            </>
          ) : (
            <DiffView diff={diffResult} />
          )}
        </div>

        {/* Right rail */}
        <div className="flex flex-col gap-3 sticky top-4">
          <Card className="bg-midnight/30 border-graphite/50 p-4 shadow-md">
            <div className="text-[10px] font-bold uppercase tracking-widest text-text-3 mb-3">Schema reference</div>
            <RefBlock title="Top-level fields" code={`apiVersion: v1
kind: Job
metadata:
  alias: string     # required
trigger:
  type: cron|http
steps: [...]`} />
            <RefBlock title="DAG edges" code={`# Prerequisites (recommended)
dependsOn: [extract.users]

# Or explicit successors
next: [transform.users]`} />
            <RefBlock title="Callbacks" code={`callbacks:
  - type: notification
    configuration:
      webhook_url: "https://…"
      channel: "#alerts"`} />
          </Card>
          
          <Card className="bg-midnight/30 border-graphite/50 p-4 shadow-md">
            <div className="text-[10px] font-bold uppercase tracking-widest text-text-3 mb-3">Tips</div>
            <ul className="m-0 p-0 list-none flex flex-col gap-2.5 text-xs text-text-2 leading-relaxed">
              <li className="flex gap-2">
                <span className="text-cyan-glow mt-0.5">·</span> 
                <span>Apply is <strong>idempotent</strong> — re-applying updates existing resources.</span>
              </li>
              <li className="flex gap-2">
                <span className="text-cyan-glow mt-0.5">·</span> 
                <span>Multiple resources can share one manifest.</span>
              </li>
              <li className="flex gap-2">
                <span className="text-cyan-glow mt-0.5">·</span> 
                <span>Use <code className="font-mono bg-obsidian px-1 py-0.5 rounded text-[11px] text-cyan-glow border border-graphite/50">caesium job lint</code> in CI.</span>
              </li>
              <li className="flex gap-2">
                <span className="text-cyan-glow mt-0.5">·</span> 
                <span>Without edges, steps run sequentially.</span>
              </li>
            </ul>
          </Card>
        </div>
      </div>
    </div>
  );
}

function DiffView({ diff }: { diff: DiffResponse | null }) {
  if (!diff) {
    return (
      <Card className="bg-midnight/30 border-graphite/50 p-8 text-center text-text-3 text-sm">
        No differences to display. Schema must be valid to generate a diff.
      </Card>
    );
  }

  const added = diff.added || [];
  const modified = diff.modified || [];
  const removed = diff.removed || [];
  const total = added.length + modified.length + removed.length;

  return (
    <Card className="bg-midnight/30 border-graphite/50 overflow-hidden shadow-lg">
      <div className="px-4 py-3 border-b border-graphite/50 bg-obsidian/30 flex justify-between items-start sm:items-center flex-col sm:flex-row gap-2">
        <div>
          <div className="text-[13px] font-medium text-text-1">Diff vs server state</div>
          <div className="text-[11px] text-text-3 mt-0.5">{total} {total === 1 ? "change" : "changes"} pending apply</div>
        </div>
        <div className="flex gap-3 text-[11px] font-medium bg-obsidian/60 px-3 py-1.5 rounded-full border border-graphite/40">
          <span className="text-success flex items-center gap-1"><span className="text-[14px] leading-none">+</span> {added.length} added</span>
          <span className="text-gold flex items-center gap-1"><span className="text-[14px] leading-none">~</span> {modified.length} modified</span>
          <span className="text-danger flex items-center gap-1"><span className="text-[14px] leading-none">-</span> {removed.length} removed</span>
        </div>
      </div>
      
      <div className="p-0">
        {total === 0 ? (
          <div className="p-8 text-center text-text-3 text-sm">
            Local definitions exactly match the server state.
          </div>
        ) : (
          <div className="font-mono text-xs leading-relaxed overflow-x-auto bg-void p-4">
            {added.map((a, i) => (
              <div key={`a-${i}`} className="flex gap-3 py-1.5">
                <span className="text-success font-bold w-4 flex-shrink-0 text-center">+</span>
                <span className="text-cyan-glow flex-shrink-0">{a.Alias}</span>
                <span className="text-success/80 text-[11px] truncate whitespace-nowrap">Job will be created</span>
              </div>
            ))}
            
            {removed.map((r, i) => (
              <div key={`r-${i}`} className="flex gap-3 py-1.5">
                <span className="text-danger font-bold w-4 flex-shrink-0 text-center">-</span>
                <span className="text-cyan-glow flex-shrink-0">{r.Alias}</span>
                <span className="text-danger/80 text-[11px] truncate whitespace-nowrap">Job will be deleted (if prune enabled)</span>
              </div>
            ))}
            
            {modified.map((m, i) => (
              <div key={`m-${i}`} className="flex flex-col py-2 border-b border-graphite/30 last:border-0">
                <div className="flex gap-3 mb-1">
                  <span className="text-gold font-bold w-4 flex-shrink-0 text-center">~</span>
                  <span className="text-cyan-glow">{m.Alias}</span>
                </div>
                <div className="pl-7 pr-2">
                  <pre className="text-[11px] font-mono text-text-3 overflow-x-auto bg-obsidian/30 p-2 rounded border border-graphite/20">
                    {m.Diff}
                  </pre>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </Card>
  );
}

function RefBlock({ title, code }: { title: string; code: string }) {
  return (
    <div className="mb-4 last:mb-0">
      <div className="text-[11px] font-medium text-text-1 mb-1.5">{title}</div>
      <pre className="font-mono m-0 p-2.5 rounded bg-void border border-graphite/60 text-[10.5px] leading-relaxed text-text-2 whitespace-pre-wrap overflow-hidden">
        {code}
      </pre>
    </div>
  );
}
