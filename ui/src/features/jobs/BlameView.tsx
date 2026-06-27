import { type FormEvent, useState } from "react";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ArrowLeft, GitBranch, GitCommit, Info, Search } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type BlameEdgeAttribution, type BlameTaskAttribution } from "@/lib/api";
import { cn } from "@/lib/utils";

type BlameSearch = {
  from: string | undefined;
  to: string | undefined;
  task: string | undefined;
};

export function BlameRoutePage() {
  const { jobId } = useParams({ strict: false }) as { jobId: string };
  const search = useSearch({ strict: false }) as BlameSearch;
  const cleanSearch = buildSearch(search.from ?? "", search.to ?? "", search.task ?? "");

  return (
    <BlameView
      key={`${jobId}:${cleanSearch.from ?? ""}:${cleanSearch.to ?? ""}:${cleanSearch.task ?? ""}`}
      jobId={jobId}
      search={cleanSearch}
    />
  );
}

export function BlameView({ jobId, search }: { jobId: string; search: BlameSearch }) {
  const navigate = useNavigate();
  const fromCommit = cleanParam(search.from);
  const toCommit = cleanParam(search.to);
  const taskFilter = cleanParam(search.task);
  const [fromInput, setFromInput] = useState(fromCommit ?? "");
  const [toInput, setToInput] = useState(toCommit ?? "");
  const [taskInput, setTaskInput] = useState(taskFilter ?? "");

  const {
    data: blame,
    isLoading,
    error,
  } = useQuery({
    queryKey: ["job", jobId, "blame", fromCommit, toCommit, taskFilter],
    queryFn: () => api.getBlame(jobId, { from: fromCommit, to: toCommit, task: taskFilter }),
    enabled: Boolean(jobId),
  });

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    void navigate({
      to: "/jobs/$jobId/blame",
      params: { jobId },
      search: buildSearch(fromInput, toInput, taskInput),
    });
  }

  function clearFilters() {
    setFromInput("");
    setToInput("");
    setTaskInput("");
    void navigate({
      to: "/jobs/$jobId/blame",
      params: { jobId },
      search: buildSearch("", "", ""),
    });
  }

  if (isLoading) {
    return (
      <div className="space-y-5 p-8" data-testid="blame-container">
        <BlameBreadcrumb jobId={jobId} />
        <Skeleton className="h-8 w-[220px]" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-5" data-testid="blame-container">
        <BlameBreadcrumb jobId={jobId} />
        <EmptyState
          title="Blame unavailable"
          subtitle={error instanceof Error ? error.message : "The blame endpoint returned an error."}
          icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        />
      </div>
    );
  }

  if (!blame) {
    return (
      <div className="space-y-5" data-testid="blame-container">
        <BlameBreadcrumb jobId={jobId} />
        <EmptyState
          title="No blame data"
          subtitle="The blame endpoint returned no attribution data for this job."
          icon={<GitBranch className="h-12 w-12 text-text-3" />}
        />
      </div>
    );
  }

  const hasElements = blame.tasks.length > 0 || blame.edges.length > 0;

  return (
    <div className="space-y-5" data-testid="blame-container">
      <BlameBreadcrumb jobId={jobId} />

      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div className="min-w-0">
          <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3 mb-1">
            DAG blame
          </div>
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="text-xl font-semibold text-text-1 font-mono tracking-tight">
              Job attribution
            </h1>
            <Badge data-testid="blame-coverage" variant="outline" className="text-[10px]">
              {formatCoverage(blame.coverage)}
            </Badge>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-text-3">
            <span className="font-mono text-text-4 text-[10px]" data-testid="blame-job-id">
              {blame.job_id}
            </span>
            <span className="text-text-4">/</span>
            <span data-testid="blame-range-summary">
              {blame.from_commit ? `from ${blame.from_commit}` : "from first snapshot"}
              {" "}
              {blame.to_commit ? `to ${blame.to_commit}` : "to latest snapshot"}
            </span>
          </div>
        </div>
        <div
          className="flex w-fit max-w-full items-start gap-1.5 rounded-md border border-primary/30 bg-primary/5 px-2.5 py-1.5 text-xs font-medium text-primary"
          data-testid="blame-coverage-caveat"
        >
          <Info className="h-3.5 w-3.5 shrink-0" />
          <span>
            Coverage caveat: topology, image, and command are tracked. env/spec/retries/cache/schema/sla/triggerRules are intentionally untracked.
          </span>
        </div>
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm">Commit Range</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <form className="grid gap-3 md:grid-cols-[1fr_1fr_0.8fr_auto]" onSubmit={applyFilters}>
            <FilterInput
              id="blame-from"
              label="From commit"
              value={fromInput}
              onChange={setFromInput}
              placeholder="first snapshot"
              testId="blame-from-input"
            />
            <FilterInput
              id="blame-to"
              label="To commit"
              value={toInput}
              onChange={setToInput}
              placeholder="latest snapshot"
              testId="blame-to-input"
            />
            <FilterInput
              id="blame-task"
              label="Task"
              value={taskInput}
              onChange={setTaskInput}
              placeholder="all tasks"
              testId="blame-task-filter-input"
            />
            <div className="flex items-end gap-2">
              <Button type="submit" size="sm" className="h-9" data-testid="blame-filter-apply">
                <Search className="h-3.5 w-3.5" />
                Apply
              </Button>
              <Button type="button" variant="outline" size="sm" className="h-9" onClick={clearFilters}>
                Clear
              </Button>
            </div>
          </form>

          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <MetadataCell label="Coverage" value={blame.coverage} testId="blame-coverage-value" mono />
            <MetadataCell label="From Commit" value={blame.from_commit || "First snapshot"} testId="blame-from-commit" mono />
            <MetadataCell label="To Commit" value={blame.to_commit || "Latest snapshot"} testId="blame-to-commit" mono />
            <MetadataCell label="Elements" value={`${blame.tasks.length} tasks / ${blame.edges.length} edges`} />
          </div>
        </CardContent>
      </Card>

      {!hasElements ? (
        <EmptyState
          title="No attributed elements"
          subtitle="No task or edge descriptors were introduced inside the selected commit range."
          icon={<GitBranch className="h-12 w-12 text-text-3" />}
        />
      ) : null}

      {blame.tasks.length > 0 ? (
        <section className="space-y-3" aria-labelledby="blame-tasks-title">
          <div className="flex items-center justify-between gap-3">
            <h2 id="blame-tasks-title" className="text-sm font-semibold text-text-1">
              Tasks
            </h2>
            <span className="text-xs text-text-3">{blame.tasks.length} attributed</span>
          </div>
          <div className="space-y-3">
            {blame.tasks.map((task) => (
              <BlameTaskRow key={`${task.element.name}:${task.snapshot_id}:${task.introducing_commit}`} task={task} />
            ))}
          </div>
        </section>
      ) : null}

      {blame.edges.length > 0 ? (
        <section className="space-y-3" aria-labelledby="blame-edges-title">
          <div className="flex items-center justify-between gap-3">
            <h2 id="blame-edges-title" className="text-sm font-semibold text-text-1">
              Edges
            </h2>
            <span className="text-xs text-text-3">{blame.edges.length} attributed</span>
          </div>
          <div className="space-y-3">
            {blame.edges.map((edge) => (
              <BlameEdgeRow key={`${edge.element.from}:${edge.element.to}:${edge.snapshot_id}:${edge.introducing_commit}`} edge={edge} />
            ))}
          </div>
        </section>
      ) : null}
    </div>
  );
}

function BlameBreadcrumb({ jobId }: { jobId: string }) {
  return (
    <div className="flex items-center gap-2 text-[11px] text-text-3">
      <Link
        to="/jobs/$jobId"
        params={{ jobId }}
        className="flex items-center gap-1 hover:text-text-2 transition-colors"
      >
        <ArrowLeft className="h-3 w-3" />
        Job
      </Link>
      <span className="text-text-4">/</span>
      <span>Blame</span>
    </div>
  );
}

function BlameTaskRow({ task }: { task: BlameTaskAttribution }) {
  return (
    <Card
      data-testid="blame-task-row"
      data-task-name={task.element.name}
      className="overflow-hidden"
    >
      <CardContent className="p-0">
        <div
          className="border-l-2 border-primary px-4 py-3"
          data-testid={`blame-task-${testIdSlug(task.element.name)}`}
        >
          <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="font-mono text-sm font-semibold text-text-1" data-testid="blame-task-name">
                  {task.element.name}
                </h3>
                <Badge variant="secondary" className="text-[10px]">Task</Badge>
              </div>
              <div className="mt-2 flex items-start gap-1.5 text-xs text-text-3">
                <GitCommit className="mt-0.5 h-3.5 w-3.5 shrink-0 text-primary" />
                <span className="font-semibold text-text-2">Introduced by</span>
                <span className="break-all font-mono" data-testid="blame-task-introducing-commit">
                  {formatCommit(task.introducing_commit)}
                </span>
              </div>
            </div>
          </div>

          <div className="mt-4 grid grid-cols-1 gap-3 md:grid-cols-3">
            <MetadataCell label="Image" value={task.element.image} testId="blame-task-image" mono />
            <MetadataCell label="Command" value={formatCommand(task.element.command)} testId="blame-task-command" mono />
            <MetadataCell label="Snapshot ID" value={task.snapshot_id} testId="blame-task-snapshot-id" mono />
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function BlameEdgeRow({ edge }: { edge: BlameEdgeAttribution }) {
  return (
    <Card
      data-testid="blame-edge-row"
      data-edge={`${edge.element.from}->${edge.element.to}`}
      className="overflow-hidden"
    >
      <CardContent className="p-0">
        <div
          className="border-l-2 border-running px-4 py-3"
          data-testid={`blame-edge-${testIdSlug(`${edge.element.from}-${edge.element.to}`)}`}
        >
          <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="font-mono text-sm font-semibold text-text-1" data-testid="blame-edge-name">
                  {edge.element.from} -&gt; {edge.element.to}
                </h3>
                <Badge variant="outline" className="text-[10px]">Edge</Badge>
              </div>
              <div className="mt-2 flex items-start gap-1.5 text-xs text-text-3">
                <GitCommit className="mt-0.5 h-3.5 w-3.5 shrink-0 text-running" />
                <span className="font-semibold text-text-2">Introduced by</span>
                <span className="break-all font-mono" data-testid="blame-edge-introducing-commit">
                  {formatCommit(edge.introducing_commit)}
                </span>
              </div>
            </div>
          </div>

          <div className="mt-4 grid grid-cols-1 gap-3 md:grid-cols-4">
            <MetadataCell label="From" value={edge.element.from} testId="blame-edge-from" mono />
            <MetadataCell label="To" value={edge.element.to} testId="blame-edge-to" mono />
            <MetadataCell label="Snapshot ID" value={edge.snapshot_id} testId="blame-edge-snapshot-id" mono />
            <MetadataCell
              label="Provenance Commit"
              value={edge.provenance_commit || "None"}
              testId="blame-edge-provenance-commit"
              mono
            />
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function FilterInput({
  id,
  label,
  value,
  onChange,
  placeholder,
  testId,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
  testId: string;
}) {
  return (
    <label htmlFor={id} className="min-w-0 space-y-1.5">
      <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">{label}</span>
      <input
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        data-testid={testId}
        className="h-9 w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-text-1 outline-none transition-colors placeholder:text-text-4 focus:border-primary focus:ring-1 focus:ring-primary"
      />
    </label>
  );
}

function MetadataCell({
  label,
  value,
  testId,
  mono = false,
}: {
  label: string;
  value: string;
  testId?: string;
  mono?: boolean;
}) {
  return (
    <div className="min-w-0">
      <div className="mb-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div
        className={cn("break-all text-xs text-foreground", mono && "font-mono")}
        data-testid={testId}
      >
        {value}
      </div>
    </div>
  );
}

function cleanParam(value?: string): string | undefined {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}

function buildSearch(from: string, to: string, task: string): BlameSearch {
  const search: BlameSearch = {
    from: undefined,
    to: undefined,
    task: undefined,
  };
  const fromCommit = cleanParam(from);
  const toCommit = cleanParam(to);
  const taskFilter = cleanParam(task);
  if (fromCommit) {
    search.from = fromCommit;
  }
  if (toCommit) {
    search.to = toCommit;
  }
  if (taskFilter) {
    search.task = taskFilter;
  }
  return search;
}

function formatCoverage(coverage: string): string {
  if (coverage === "topology+image+command") {
    return "Topology, image, and command";
  }
  return coverage;
}

function formatCommit(commit: string): string {
  return commit || "No commit recorded";
}

function formatCommand(command?: string[]): string {
  return JSON.stringify(command ?? []);
}

function testIdSlug(value: string): string {
  const slug = value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "");
  return slug || "element";
}
