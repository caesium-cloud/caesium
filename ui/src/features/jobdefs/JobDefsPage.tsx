import { useMutation } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { toast } from "sonner";
import { FileCode2, Play, CheckCircle2, XCircle, RotateCcw } from "lucide-react";
import { useState } from "react";

const EXAMPLE_YAML = `# Example Caesium job definition
apiVersion: v1
kind: Job
metadata:
  alias: my-pipeline
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
    timezone: "UTC"
steps:
  - name: fetch
    image: python:3.11-slim
    command: ["python", "/app/fetch.py"]
  - name: transform
    image: python:3.11-slim
    command: ["python", "/app/transform.py"]
  - name: load
    image: python:3.11-slim
    command: ["python", "/app/load.py"]
`;

export function JobDefsPage() {
  const [yaml, setYaml] = useState("");
  const [result, setResult] = useState<{ ok: boolean; message: string; detail?: string } | null>(null);

  const applyMutation = useMutation({
    mutationFn: () => api.applyJobDef(yaml),
    onSuccess: (data) => {
      setResult({ ok: true, message: "Job definition applied successfully", detail: JSON.stringify(data, null, 2) });
      toast.success("Job definition applied");
    },
    onError: (err) => {
      const msg = err instanceof Error ? err.message : "Unknown error";
      setResult({ ok: false, message: "Failed to apply job definition", detail: msg });
      toast.error("Failed to apply job definition");
    },
  });

  const lineCount = yaml.split("\n").length;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Job Definitions</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Apply YAML manifests to create or update jobs, atoms, and triggers</p>
        </div>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-3 gap-4">
        {/* Editor */}
        <div className="xl:col-span-2 space-y-3">
          <Card>
            <CardHeader className="pb-2 flex flex-row items-center justify-between">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <FileCode2 className="h-4 w-4" />
                YAML Editor
              </CardTitle>
              <div className="flex items-center gap-2">
                <span className="text-xs text-muted-foreground">{lineCount} lines</span>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => { setYaml(EXAMPLE_YAML); setResult(null); }}
                  className="text-xs h-7"
                >
                  Load example
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => { setYaml(""); setResult(null); }}
                  className="text-xs h-7"
                  disabled={!yaml}
                >
                  <RotateCcw className="h-3 w-3 mr-1" />
                  Clear
                </Button>
              </div>
            </CardHeader>
            <CardContent className="p-0">
              <div className="relative">
                {/* Line numbers */}
                <div className="absolute left-0 top-0 bottom-0 w-10 flex flex-col bg-muted/50 border-r text-[10px] font-mono text-muted-foreground overflow-hidden pointer-events-none select-none pt-3 pl-2">
                  {yaml.split("\n").map((_, i) => (
                    <div key={i} className="leading-6">{i + 1}</div>
                  ))}
                  {yaml.length === 0 && <div className="leading-6">1</div>}
                </div>
                <textarea
                  value={yaml}
                  onChange={e => { setYaml(e.target.value); setResult(null); }}
                  spellCheck={false}
                  placeholder={`# Paste your job definition YAML here\n# or click "Load example" to get started`}
                  className="w-full min-h-[480px] pl-12 pr-4 pt-3 pb-3 font-mono text-sm bg-caesium-void text-green-400 rounded-b-lg focus:outline-none focus:ring-2 focus:ring-ring resize-y placeholder:text-green-900 leading-6"
                  style={{ tabSize: 2 }}
                  onKeyDown={e => {
                    // Tab inserts 2 spaces
                    if (e.key === "Tab") {
                      e.preventDefault();
                      const start = e.currentTarget.selectionStart;
                      const end = e.currentTarget.selectionEnd;
                      const newVal = yaml.substring(0, start) + "  " + yaml.substring(end);
                      setYaml(newVal);
                      setTimeout(() => {
                        e.currentTarget.selectionStart = start + 2;
                        e.currentTarget.selectionEnd = start + 2;
                      }, 0);
                    }
                  }}
                />
              </div>
            </CardContent>
          </Card>

          <div className="flex items-center gap-3">
            <Button
              onClick={() => applyMutation.mutate()}
              disabled={!yaml.trim() || applyMutation.isPending}
              className="gap-2"
            >
              <Play className="h-4 w-4" />
              {applyMutation.isPending ? "Applying..." : "Apply Definition"}
            </Button>
            {result && (
              <Badge variant={result.ok ? "success" : "destructive"} className="gap-1.5">
                {result.ok
                  ? <CheckCircle2 className="h-3 w-3" />
                  : <XCircle className="h-3 w-3" />}
                {result.message}
              </Badge>
            )}
          </div>

          {/* Result detail */}
          {result?.detail && (
            <Card>
              <CardHeader className="pb-2">
                <CardTitle className="text-sm font-medium">
                  {result.ok ? "Response" : "Error Detail"}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <pre className={`text-xs rounded p-3 overflow-auto max-h-48 font-mono ${
                  result.ok ? "bg-muted text-foreground" : "bg-destructive/10 text-destructive"
                }`}>
                  {result.detail}
                </pre>
              </CardContent>
            </Card>
          )}
        </div>

        {/* Reference panel */}
        <div className="space-y-4">
          <Card>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium">Schema Reference</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4 text-xs">
              <div>
                <p className="font-semibold text-foreground mb-1.5">Required top-level fields</p>
                <pre className="bg-muted rounded p-2 text-muted-foreground overflow-x-auto">{`apiVersion: v1
kind: Job
metadata:
  alias: string     # required
trigger:
  type: cron|http
  configuration: {} # type-specific
steps:
  - name: string    # required
    image: string   # required
    command: [...]  # optional
    engine: docker  # default`}</pre>
              </div>
              <div>
                <p className="font-semibold text-foreground mb-1.5">Step DAG edges</p>
                <pre className="bg-muted rounded p-2 text-muted-foreground overflow-x-auto">{`# Explicit successors
next: [step-b, step-c]
# Or prerequisites
dependsOn: [step-a]
# Without edges: steps run
# sequentially top-to-bottom`}</pre>
              </div>
              <div>
                <p className="font-semibold text-foreground mb-1.5">Callbacks (optional)</p>
                <pre className="bg-muted rounded p-2 text-muted-foreground overflow-x-auto">{`callbacks:
  - type: notification
    configuration:
      webhook_url: "https://..."
      channel: "#alerts"`}</pre>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium">Notes</CardTitle>
            </CardHeader>
            <CardContent className="text-xs text-muted-foreground space-y-2">
              <p>• Applying a definition is <strong className="text-foreground">idempotent</strong> — re-applying the same definition updates existing resources.</p>
              <p>• Jobs, atoms, and triggers can be defined in the same manifest or separate files.</p>
              <p>• Trigger aliases are referenced by jobs in the <code className="bg-muted px-1 rounded">trigger</code> field.</p>
              <p>• Task order is defined by the <code className="bg-muted px-1 rounded">next</code> field to form a DAG.</p>
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  );
}
