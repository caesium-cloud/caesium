import { useCallback, useDeferredValue, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, type AllowBreakingRequest, type ContractDiffFinding, type DiffResponse, type LintResponse } from "@/lib/api";
import { FileCode2, Play, CheckCircle2, XCircle, Upload, GitBranch, FileWarning, Info, HardDrive, ShieldCheck, AlertTriangle } from "lucide-react";
import { Button } from "@/components/ui/button";
import CodeMirror from "@uiw/react-codemirror";
import { yaml as yamlLang } from "@codemirror/lang-yaml";
import { linter, type Diagnostic } from "@codemirror/lint";
import { EditorView, type ViewUpdate } from "@codemirror/view";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Card } from "@/components/ui/card";
import { getJobDefRuntimeHints, type JobDefRuntimeHints } from "./runtimeHints";

export const EXAMPLE_YAML = `apiVersion: v1
kind: Job
metadata:
  alias: infra-plan-apply
  labels:
    team: platform
    tier: critical
  serviceAccountName: caesium-planner
  podAnnotations:
    iam.gke.io/gcp-service-account: terraform-planner@example.iam.gserviceaccount.com
  automountServiceAccountToken: true
trigger:
  type: http
  configuration:
    path: "/hooks/infra/apply"
volumes:
  - name: work
    sources:
      docker:
        bind: /mnt/nfs/caesium-work
      podman:
        bind: /mnt/nfs/caesium-work
      kubernetes:
        pvc: ci-shared-rwx
steps:
  - name: plan
    engine: kubernetes
    image: hashicorp/terraform:1.9
    command: ["terraform", "plan", "-out=/work/tf.plan"]
    volumeMounts:
      - {volume: work, path: /work}
  - name: apply
    engine: kubernetes
    image: hashicorp/terraform:1.9
    dependsOn: [plan]
    serviceAccountName: caesium-deployer
    volumeMounts:
      - {volume: work, path: /work, readOnly: true, subPath: plans}
    command: ["terraform", "apply", "/work/tf.plan"]
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

function formatStepCount(count: number) {
  return `${count} ${count === 1 ? "step" : "steps"}`;
}

function contractStatusLabel(summary: string) {
  const detailStart = summary.indexOf(" (");
  return detailStart === -1 ? summary : summary.slice(0, detailStart);
}

function collectContractFindings(diff: DiffResponse | null) {
  if (!diff) return [];
  return dedupeContractFindings([
    ...(diff.added ?? []).flatMap((job) => job.contractFindings ?? []),
    ...(diff.removed ?? []).flatMap((job) => job.contractFindings ?? []),
    ...(diff.modified ?? []).flatMap((job) => job.contractFindings ?? []),
  ]);
}

function dedupeContractFindings(findings: ContractDiffFinding[] | undefined) {
  if (!findings?.length) return [];
  const seen = new Set<string>();
  const out: ContractDiffFinding[] = [];
  for (const finding of findings) {
    const key = [
      finding.edgeId,
      finding.edgeClass,
      finding.from,
      finding.to,
      finding.verdict,
      finding.path,
      finding.key,
      finding.detail,
      contractDatasetLabel(finding.dataset),
    ].join("|");
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(finding);
  }
  return out;
}

function buildContractTeamLookup(diff: DiffResponse | null) {
  const teams: Record<string, string> = {};
  for (const job of [...(diff?.added ?? []), ...(diff?.removed ?? [])]) {
    const alias = job.alias?.trim();
    const team = job.labels?.team?.trim();
    if (alias && team) {
      teams[alias] = team;
    }
  }
  return teams;
}

function contractDatasetLabel(dataset: ContractDiffFinding["dataset"] | undefined) {
  const namespace = dataset?.namespace?.trim();
  const name = dataset?.name?.trim();
  if (namespace && name) return `${namespace}/${name}`;
  return name || namespace || "";
}

function jobAliasFromNodeID(nodeID: string | undefined) {
  const value = nodeID?.trim() ?? "";
  return value.startsWith("job:") ? value.slice(4) : value;
}

function contractSubjectForFinding(finding: ContractDiffFinding) {
  const dataset = contractDatasetLabel(finding.dataset);
  if (dataset) return dataset;
  const producer = jobAliasFromNodeID(finding.from) || "producer";
  const key = finding.key?.trim();
  if (key) return `${producer}.output.${key}`;
  return `${producer}.contract`;
}

function contractConsumerLabel(finding: ContractDiffFinding) {
  return jobAliasFromNodeID(finding.to) || "unknown consumer";
}

function contractTeamLabel(finding: ContractDiffFinding, contractTeams: Record<string, string>) {
  const consumer = contractConsumerLabel(finding);
  const team = finding.consumerTeam?.trim() || finding.consumer_team?.trim() || contractTeams[consumer];
  if (team) return `team: ${team}`;
  return "team not set";
}

function contractFindingTitle(finding: ContractDiffFinding, contractTeams: Record<string, string>) {
  return [
    `${finding.verdict}: ${contractSubjectForFinding(finding)}`,
    `consumer: ${contractConsumerLabel(finding)}`,
    contractTeamLabel(finding, contractTeams),
    finding.detail,
  ].filter(Boolean).join(" · ");
}

function contractBadgeClass(verdict: ContractDiffFinding["verdict"]) {
  switch (verdict) {
    case "breaking":
      return "border-danger/35 bg-danger/10 text-danger";
    case "unknown":
      return "border-warning/35 bg-warning/10 text-warning";
    case "compatible":
      return "border-success/35 bg-success/10 text-success";
    default:
      return "border-text-3/30 text-text-3";
  }
}

export function JobDefsPage() {
  const queryClient = useQueryClient();
  const [yaml, setYaml] = useState(EXAMPLE_YAML);
  const [tab, setTab] = useState("editor");
  const [lintResult, setLintResult] = useState<LintResponse>({ errors: [], warnings: [], summary: { steps: 0 } });
  const [diffResult, setDiffResult] = useState<DiffResponse | null>(null);
  const [isLinting, setIsLinting] = useState(false);
  const [ackReason, setAckReason] = useState("");
  const latestYamlRef = useRef(EXAMPLE_YAML);
  const yamlVersionRef = useRef(0);
  const validationSeqRef = useRef(0);
  const editorViewRef = useRef<EditorView | null>(null);

  const syncLatestYaml = useCallback((value: string) => {
    if (latestYamlRef.current !== value) {
      latestYamlRef.current = value;
      yamlVersionRef.current += 1;
    }
    return yamlVersionRef.current;
  }, []);

  const currentEditorYaml = useCallback(() => (
    editorViewRef.current?.state.doc.toString() ?? latestYamlRef.current
  ), []);

  const runValidation = useCallback(async (sourceYaml: string, sourceVersion = yamlVersionRef.current) => {
    const validationID = validationSeqRef.current + 1;
    validationSeqRef.current = validationID;
    const isCurrentValidation = () => (
      validationID === validationSeqRef.current && sourceVersion === yamlVersionRef.current
    );

    setIsLinting(true);
    try {
      const lr = await api.lintJobDef(sourceYaml);
      if (!isCurrentValidation()) return;
      setLintResult(lr);

      // Only get diff if lint passes
      if (lr.errors && lr.errors.length === 0) {
        const dr = await api.diffJobDef(sourceYaml);
        if (!isCurrentValidation()) return;
        setDiffResult(dr);
      } else {
        setDiffResult(null);
      }
    } catch (err) {
      if (!isCurrentValidation()) return;
      // Surface YAML parse errors or network failures
      setLintResult({
        errors: [{ message: err instanceof Error ? err.message : "Request failed", line: 1 }],
        warnings: [],
        summary: { steps: 0 },
      });
      setDiffResult(null);
    } finally {
      if (isCurrentValidation()) {
        setIsLinting(false);
      }
    }
  }, []);

  const handleYamlChange = useCallback((val: string) => {
    syncLatestYaml(val);
    setYaml(val);
    setIsLinting(true);
    setAckReason("");
  }, [syncLatestYaml]);

  const handleEditorUpdate = useCallback((update: ViewUpdate) => {
    if (update.docChanged) {
      syncLatestYaml(update.state.doc.toString());
    }
  }, [syncLatestYaml]);

  const handleResetExample = useCallback(() => {
    syncLatestYaml(EXAMPLE_YAML);
    setYaml(EXAMPLE_YAML);
    setIsLinting(true);
    setAckReason("");
  }, [syncLatestYaml]);

  const handleTabChange = useCallback((value: string) => {
    setTab(value);
    if (value !== "diff") return;

    const sourceYaml = currentEditorYaml();
    const sourceVersion = syncLatestYaml(sourceYaml);
    if (sourceYaml !== yaml) {
      setYaml(sourceYaml);
      setIsLinting(true);
      setAckReason("");
    }
    void runValidation(sourceYaml, sourceVersion);
  }, [currentEditorYaml, runValidation, syncLatestYaml, yaml]);

  // Debounced API calls
  useEffect(() => {
    const sourceVersion = yamlVersionRef.current;
    const timer = setTimeout(async () => {
      await runValidation(yaml, sourceVersion);
    }, 300);

    return () => {
      clearTimeout(timer);
    };
  }, [runValidation, yaml]);

  const contractTeams = useMemo(() => buildContractTeamLookup(diffResult), [diffResult]);
  const contractFindings = useMemo(() => collectContractFindings(diffResult), [diffResult]);
  const breakingContractFindings = contractFindings.filter((finding) => finding.verdict === "breaking");
  const hasBreakingContractFindings = breakingContractFindings.length > 0;
  // The allow_breaking request (like the CLI flag) acknowledges ONE subject per
  // apply, so the in-page ack can only unlock single-subject breaking diffs;
  // multi-subject breaks must be split into separate applies.
  const breakingSubjects = [...new Set(breakingContractFindings.map(contractSubjectForFinding))];
  const hasMultipleBreakingSubjects = breakingSubjects.length > 1;
  const ackSubject = breakingSubjects[0] ?? "";
  const trimmedAckReason = ackReason.trim();
  const allowBreaking: AllowBreakingRequest | undefined = hasBreakingContractFindings
    ? { dataset: ackSubject, reason: trimmedAckReason }
    : undefined;

  const applyMutation = useMutation({
    mutationFn: () => api.applyJobDef(yaml, allowBreaking),
    onSuccess: (data) => {
      toast.success(`Applied successfully (${data.applied} jobs)`);
      for (const warning of data.contract_warnings ?? []) {
        toast.warning(warning.message || `Contract warning for ${warning.subject}`);
      }
      setAckReason("");
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
  const stepLabel = formatStepCount(lintResult.summary?.steps ?? 0);
  const contractSummary = lintResult.summary?.contracts?.trim() ?? "";
  const summaryLabel = contractSummary ? `${stepLabel} · ${contractStatusLabel(contractSummary)}` : stepLabel;
  const deferredYaml = useDeferredValue(yaml);
  const runtimeHints = useMemo(() => getJobDefRuntimeHints(deferredYaml), [deferredYaml]);

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
        <div className="flex flex-col sm:flex-row sm:items-center gap-2">
          <Button variant="outline" size="sm" className="bg-transparent border-graphite/50 text-text-2">
            <Upload className="h-3.5 w-3.5 mr-1.5" />
            Upload
          </Button>
          <Button variant="outline" size="sm" className="bg-transparent border-graphite/50 text-text-2">
            <GitBranch className="h-3.5 w-3.5 mr-1.5" />
            Git sync
          </Button>
          {hasBreakingContractFindings && !hasMultipleBreakingSubjects && (
            <input
              aria-label="Breaking change acknowledgement reason"
              data-testid="contract-ack-reason"
              value={ackReason}
              onChange={(event) => setAckReason(event.currentTarget.value)}
              placeholder={`Reason for ${ackSubject}`}
              className="h-8 w-full sm:w-64 rounded-md border border-danger/35 bg-danger/10 px-3 text-xs text-text-1 placeholder:text-danger/70 outline-none transition-colors focus:border-danger"
            />
          )}
          {hasMultipleBreakingSubjects && (
            <span
              data-testid="contract-ack-multi-subject"
              className="text-[11px] text-danger/90 max-w-xs"
            >
              {breakingSubjects.length} contracts break ({breakingSubjects.join(", ")}) — an
              acknowledgement covers one subject per apply, so split this change into separate
              applies (or use `caesium job apply --allow-breaking` per dataset).
            </span>
          )}
          <Button
            size="sm"
            className="bg-cyan-glow text-midnight hover:bg-cyan-dim disabled:opacity-50"
            onClick={() => applyMutation.mutate()}
            disabled={hasErrors || applyMutation.isPending || !yaml.trim() || isLinting || (hasBreakingContractFindings && !trimmedAckReason) || hasMultipleBreakingSubjects}
          >
            <Play className="h-3.5 w-3.5 mr-1.5" />
            {applyMutation.isPending ? "Applying..." : "Apply definition"}
          </Button>
        </div>
      </div>

      <div className="bg-obsidian border border-graphite/50 rounded-lg p-[3px] w-fit">
        <Tabs value={tab} onValueChange={handleTabChange}>
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
                      onClick={handleResetExample}
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
                    onChange={handleYamlChange}
                    onCreateEditor={(view) => {
                      editorViewRef.current = view;
                      syncLatestYaml(view.state.doc.toString());
                    }}
                    onUpdate={handleEditorUpdate}
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
                        <span className="text-text-3">{summaryLabel}</span>
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
                
                {!hasErrors && contractSummary && (
                  <div className="p-3">
                    <div className="flex items-start gap-2.5 text-xs">
                      <Info className="h-3.5 w-3.5 text-success mt-0.5 flex-shrink-0" />
                      <span className="text-text-2">{contractSummary}</span>
                    </div>
                  </div>
                )}
              </Card>
            </>
          ) : (
            <DiffView diff={diffResult} contractTeams={contractTeams} />
          )}
        </div>

        {/* Right rail */}
        <div className="flex flex-col gap-3 sticky top-4">
          <Card className="bg-midnight/30 border-graphite/50 p-4 shadow-md">
            <div className="text-[10px] font-bold uppercase tracking-widest text-text-3 mb-3">Runtime support</div>
            <div className="flex flex-col gap-3">
              <RuntimeHint
                icon={<HardDrive className="h-3.5 w-3.5" />}
                label="Volumes"
                value={formatVolumeHint(runtimeHints)}
                detail={formatVolumeDetail(runtimeHints)}
              />
              <RuntimeHint
                icon={<ShieldCheck className="h-3.5 w-3.5" />}
                label="Kubernetes identity"
                value={formatIdentityHint(runtimeHints)}
                detail={formatIdentityDetail(runtimeHints)}
              />
            </div>
          </Card>

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
            <RefBlock title="Volumes" code={`volumes:
  - name: work
    sources:
      docker: {bind: /mnt/nfs/work}
      kubernetes: {pvc: ci-shared-rwx}

volumeMounts:
  - {volume: work, path: /work}`} />
            <RefBlock title="Kubernetes identity" code={`metadata:
  serviceAccountName: caesium-runner
  podAnnotations: {...}

steps:
  - engine: kubernetes
    serviceAccountName: caesium-deployer`} />
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
              <li className="flex gap-2">
                <span className="text-cyan-glow mt-0.5">·</span>
                <span>Use a shared <code className="font-mono bg-obsidian px-1 py-0.5 rounded text-[11px] text-cyan-glow border border-graphite/50">bind</code> path or RWX <code className="font-mono bg-obsidian px-1 py-0.5 rounded text-[11px] text-cyan-glow border border-graphite/50">pvc</code> for cross-step files.</span>
              </li>
            </ul>
          </Card>
        </div>
      </div>
    </div>
  );
}

function RuntimeHint({
  icon,
  label,
  value,
  detail,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  detail: string;
}) {
  return (
    <div className="flex items-start gap-2.5 text-xs">
      <div className="mt-0.5 text-cyan-glow shrink-0">{icon}</div>
      <div className="min-w-0">
        <div className="font-medium text-text-1">{label}</div>
        <div className="font-mono text-[11px] text-text-2 break-all">{value}</div>
        <div className="text-[11px] text-text-4 leading-relaxed">{detail}</div>
      </div>
    </div>
  );
}

function formatVolumeHint(hints: JobDefRuntimeHints) {
  if (hints.parseError) return "Waiting for valid YAML";
  if (hints.volumeCount === 0) return "No volumes declared";
  return `${hints.volumeCount} declared`;
}

function formatVolumeDetail(hints: JobDefRuntimeHints) {
  if (hints.parseError) return "Live lint will show parser details.";
  if (hints.volumeMountCount === 0) return "No step mounts a declared volume.";
  const steps = `${hints.volumeMountedStepCount} ${hints.volumeMountedStepCount === 1 ? "step" : "steps"}`;
  const mounts = `${hints.volumeMountCount} ${hints.volumeMountCount === 1 ? "mount" : "mounts"}`;
  return `${mounts} across ${steps}.`;
}

function formatIdentityHint(hints: JobDefRuntimeHints) {
  if (hints.parseError) return "Waiting for valid YAML";
  if (hints.serviceAccountNames.length === 0) return "No service account";
  return hints.serviceAccountNames.join(", ");
}

function formatIdentityDetail(hints: JobDefRuntimeHints) {
  if (hints.parseError) return "Live lint will show parser details.";
  const extras = [];
  if (hints.hasPodAnnotations) extras.push("pod annotations");
  if (hints.hasAutomountTokenSetting) extras.push("token setting");
  if (extras.length === 0) return "Step-level values override metadata defaults.";
  return `Includes ${extras.join(" and ")}.`;
}

function DiffView({ diff, contractTeams }: { diff: DiffResponse | null; contractTeams: Record<string, string> }) {
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
              <div key={`a-${i}`} className="py-1.5 border-b border-graphite/20 last:border-0">
                <div className="flex gap-3">
                  <span className="text-success font-bold w-4 flex-shrink-0 text-center">+</span>
                  <span className="text-cyan-glow flex-shrink-0">{a.alias}</span>
                  <span className="text-success/80 text-[11px] truncate whitespace-nowrap">Job will be created</span>
                </div>
                <ContractFindingsList findings={a.contractFindings} contractTeams={contractTeams} />
              </div>
            ))}
            
            {removed.map((r, i) => (
              <div key={`r-${i}`} className="py-1.5 border-b border-graphite/20 last:border-0">
                <div className="flex gap-3">
                  <span className="text-danger font-bold w-4 flex-shrink-0 text-center">-</span>
                  <span className="text-cyan-glow flex-shrink-0">{r.alias}</span>
                  <span className="text-danger/80 text-[11px] truncate whitespace-nowrap">Job will be deleted (if prune enabled)</span>
                </div>
                <ContractFindingsList findings={r.contractFindings} contractTeams={contractTeams} />
              </div>
            ))}
            
            {modified.map((m, i) => (
              <div key={`m-${i}`} className="flex flex-col py-2 border-b border-graphite/30 last:border-0">
                <div className="flex gap-3 mb-1">
                  <span className="text-gold font-bold w-4 flex-shrink-0 text-center">~</span>
                  <span className="text-cyan-glow">{m.alias}</span>
                </div>
                <ContractFindingsList findings={m.contractFindings} contractTeams={contractTeams} />
                <div className="pl-7 pr-2">
                  <pre className="text-[11px] font-mono text-text-3 overflow-x-auto bg-obsidian/30 p-2 rounded border border-graphite/20">
                    {m.diff}
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

function ContractFindingsList({
  findings,
  contractTeams,
}: {
  findings?: ContractDiffFinding[];
  contractTeams: Record<string, string>;
}) {
  const visibleFindings = dedupeContractFindings(findings);
  if (visibleFindings.length === 0) return null;

  return (
    <div className="pl-7 pr-2 pb-2 flex flex-col gap-1.5" data-testid="contract-findings">
      {visibleFindings.map((finding) => (
        <ContractFindingBadge
          key={`${finding.edgeId ?? ""}:${finding.path ?? ""}:${finding.detail ?? ""}`}
          finding={finding}
          contractTeams={contractTeams}
        />
      ))}
    </div>
  );
}

function ContractFindingBadge({
  finding,
  contractTeams,
}: {
  finding: ContractDiffFinding;
  contractTeams: Record<string, string>;
}) {
  const subject = contractSubjectForFinding(finding);
  const consumer = contractConsumerLabel(finding);
  const team = contractTeamLabel(finding, contractTeams);
  const edgeClass = finding.edgeClass ?? "edge";

  return (
    <details className="group max-w-full">
      <summary
        title={contractFindingTitle(finding, contractTeams)}
        data-testid="contract-finding-badge"
        data-verdict={finding.verdict}
        className={[
          "flex w-fit max-w-full cursor-pointer list-none flex-wrap items-center gap-1.5 rounded-full border px-2 py-1 text-[10px] font-semibold leading-tight shadow-sm transition-colors [&::-webkit-details-marker]:hidden",
          contractBadgeClass(finding.verdict),
        ].join(" ")}
      >
        {finding.verdict === "breaking" && <AlertTriangle className="h-3 w-3 flex-shrink-0" />}
        <span className="uppercase">{finding.verdict}</span>
        <span className="text-text-4">·</span>
        <span className="break-all">{subject}</span>
        <span className="text-text-4">·</span>
        <span>consumer: {consumer}</span>
        <span className="text-text-4">·</span>
        <span>{team}</span>
      </summary>
      <div className="mt-1 max-w-2xl rounded border border-graphite/30 bg-obsidian/40 p-2 text-[11px] leading-relaxed text-text-2">
        <div className="flex flex-wrap gap-x-3 gap-y-1">
          <span>{edgeClass}</span>
          {finding.path && <span>path: {finding.path}</span>}
          {finding.key && <span>key: {finding.key}</span>}
        </div>
        {finding.detail && <div className="mt-1 text-text-3">{finding.detail}</div>}
      </div>
    </details>
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
