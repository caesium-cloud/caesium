import { IdChip } from "@/components/ui/id-chip";
import { MetadataValue } from "@/components/ui/metadata-value";
import { type FormEvent, useEffect, useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type BlameEdgeAttribution, type BlameTaskAttribution } from "@/lib/api";
import { cn, formatCommandForDisplay } from "@/lib/utils";

type BlameSearch = {
  from: string | undefined;
  to: string | undefined;
  task: string | undefined;
};

const blameRouteApi = getRouteApi("/jobs/$jobId/blame");

export function BlameRoutePage() {
  const { jobId } = blameRouteApi.useParams();
  const search = blameRouteApi.useSearch();
  const cleanSearch = buildSearch(search.from ?? "", search.to ?? "", search.task ?? "");

  return <BlameView jobId={jobId} search={cleanSearch} />;
}

export function BlameView({ jobId, search }: { jobId: string; search: BlameSearch }) {
  const navigate = useNavigate();
  const fromCommit = cleanParam(search.from);
  const toCommit = cleanParam(search.to);
  const taskFilter = cleanParam(search.task);
  const [fromInput, setFromInput] = useState(fromCommit ?? "");
  const [toInput, setToInput] = useState(toCommit ?? "");
  const [taskInput, setTaskInput] = useState(taskFilter ?? "");

  useEffect(() => {
    let active = true;
    const nextFromInput = fromCommit ?? "";
    const nextToInput = toCommit ?? "";
    const nextTaskInput = taskFilter ?? "";

    queueMicrotask(() => {
      if (!active) {
        return;
      }
      setFromInput(nextFromInput);
      setToInput(nextToInput);
      setTaskInput(nextTaskInput);
    });

    return () => {
      active = false;
    };
  }, [fromCommit, toCommit, taskFilter]);

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
      <div className="space-y-5 relative" data-testid="blame-container"><span aria-hidden="true" className="cs-now-head absolute left-0 top-0 h-[7px] w-[7px] rounded-full bg-running" />
        <BlameBreadcrumb jobId={jobId} />
        <Skeleton className="h-8 w-[220px]" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-5 relative" data-testid="blame-container"><span aria-hidden="true" className="cs-now-head absolute left-0 top-0 h-[7px] w-[7px] rounded-full bg-running" />
        <BlameBreadcrumb jobId={jobId} />
        <EmptyState
          title="Blame unavailable"
          subtitle={error instanceof Error ? error.message : "The blame endpoint returned an error."}
        />
      </div>
    );
  }

  if (!blame) {
    return (
      <div className="space-y-5 relative" data-testid="blame-container"><span aria-hidden="true" className="cs-now-head absolute left-0 top-0 h-[7px] w-[7px] rounded-full bg-running" />
        <BlameBreadcrumb jobId={jobId} />
        <EmptyState
          title="No blame data"
          subtitle="The blame endpoint returned no attribution data for this job."
        />
      </div>
    );
  }

  const hasElements = blame.tasks.length > 0 || blame.edges.length > 0;

  return (
    <div className="space-y-5 relative" data-testid="blame-container"><span aria-hidden="true" className="cs-now-head absolute left-0 top-0 h-[7px] w-[7px] rounded-full bg-running" />
      <BlameBreadcrumb jobId={jobId} />

      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div className="min-w-0">
          <div className="text-[11px] font-bold lowercase text-text-3 mb-1">
            DAG blame
          </div>
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="text-2xl font-bold lowercase text-text-1">
              Job attribution
            </h1>
            <Badge data-testid="blame-coverage" variant="outline" className="text-[11px]">
              {formatCoverage(blame.coverage)}
            </Badge>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-text-3">
            <span className="text-text-3 text-[11px]" data-testid="blame-job-id">
              <IdChip value={blame.job_id} label="job id" />
            </span>
            <span className="text-text-3">/</span>
            <span data-testid="blame-range-summary">
              {blame.from_commit ? <>from <IdChip value={blame.from_commit} label="from commit" /></> : "from first snapshot"}
              {" "}
              {blame.to_commit ? <>to <IdChip value={blame.to_commit} label="to commit" /></> : "to latest snapshot"}
            </span>
          </div>
        </div>
        <div
          className="flex w-fit max-w-full items-start gap-1.5 rounded-md border border-primary/30 bg-primary/5 px-2.5 py-1.5 text-xs font-normal text-primary"
          data-testid="blame-coverage-caveat"
        >

          <span>
            Coverage caveat: topology, image, and command are tracked. env/spec/retries/cache/schema/sla/triggerRules are intentionally untracked.
          </span>
        </div>
      </div>

      <section className="border-b border-border py-3">
        <div className="pb-3">
          <h3 className="text-sm">Commit Range</h3>
        </div>
        <div className="space-y-4">
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

                Apply
              </Button>
              <Button type="button" variant="outline" size="sm" className="h-9" onClick={clearFilters}>
                Clear
              </Button>
            </div>
          </form>

          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <MetadataCell label="Coverage" value={blame.coverage} testId="blame-coverage-value" mono />
            <MetadataCell idChip={!!blame.from_commit} label="From Commit" value={blame.from_commit || "First snapshot"} testId="blame-from-commit" mono />
            <MetadataCell idChip={!!blame.to_commit} label="To Commit" value={blame.to_commit || "Latest snapshot"} testId="blame-to-commit" mono />
            <MetadataCell label="Elements" value={`${blame.tasks.length} tasks / ${blame.edges.length} edges`} />
          </div>
        </div>
      </section>

      {!hasElements ? (
        <EmptyState
          title="No attributed elements"
          subtitle="No task or edge descriptors were introduced inside the selected commit range."
        />
      ) : null}

      {blame.tasks.length > 0 ? (
        <section className="space-y-3" aria-labelledby="blame-tasks-title">
          <div className="flex items-center justify-between gap-3">
            <h2 id="blame-tasks-title" className="text-sm font-bold text-text-1">
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
            <h2 id="blame-edges-title" className="text-sm font-bold text-text-1">
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

        Job
      </Link>
      <span className="text-text-3">/</span>
      <span>Blame</span>
    </div>
  );
}

function BlameTaskRow({ task }: { task: BlameTaskAttribution }) {
  return (
    <section
      data-testid="blame-task-row"
      data-task-name={task.element.name}
      className="overflow-hidden"
    >
      <div className="p-0">
        <div
          className="min-h-[42px] border-l border-border px-4 py-2"
          data-testid={`blame-task-${testIdSlug(task.element.name)}`}
        >
          <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="text-sm font-bold text-text-1" data-testid="blame-task-name">
                  {task.element.name}
                </h3>
                <Badge variant="secondary" className="text-[11px]">Task</Badge>
              </div>
              <div className="mt-2 flex items-start gap-1.5 text-xs text-text-3">

                <span className="font-bold text-text-2">Introduced by</span>
                <span className="break-all" data-testid="blame-task-introducing-commit">
                  {task.introducing_commit ? <IdChip value={task.introducing_commit} label="introducing commit" /> : "No commit recorded"}
                </span>
              </div>
            </div>
          </div>

          <div className="mt-4 grid grid-cols-1 gap-3 md:grid-cols-3">
            <MetadataCell label="Image" value={task.element.image} testId="blame-task-image" mono />
            <MetadataCell label="Command" value={formatCommandForDisplay(task.element.command, "No command recorded")} testId="blame-task-command" mono />
            <MetadataCell idChip label="Snapshot ID" value={task.snapshot_id} testId="blame-task-snapshot-id" mono />
          </div>
        </div>
      </div>
    </section>
  );
}

function BlameEdgeRow({ edge }: { edge: BlameEdgeAttribution }) {
  return (
    <section
      data-testid="blame-edge-row"
      data-edge={`${edge.element.from}->${edge.element.to}`}
      className="overflow-hidden"
    >
      <div className="p-0">
        <div
          className="min-h-[42px] border-l border-border px-4 py-2"
          data-testid={`blame-edge-${testIdSlug(`${edge.element.from}-${edge.element.to}`)}`}
        >
          <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="text-sm font-bold text-text-1" data-testid="blame-edge-name">
                  {edge.element.from} -&gt; {edge.element.to}
                </h3>
                <Badge variant="outline" className="text-[11px]">Edge</Badge>
              </div>
              <div className="mt-2 flex items-start gap-1.5 text-xs text-text-3">

                <span className="font-bold text-text-2">Introduced by</span>
                <span className="break-all" data-testid="blame-edge-introducing-commit">
                  {edge.introducing_commit ? <IdChip value={edge.introducing_commit} label="introducing commit" /> : "No commit recorded"}
                </span>
              </div>
            </div>
          </div>

          <div className="mt-4 grid grid-cols-1 gap-3 md:grid-cols-4">
            <MetadataCell label="From" value={edge.element.from} testId="blame-edge-from" mono />
            <MetadataCell label="To" value={edge.element.to} testId="blame-edge-to" mono />
            <MetadataCell idChip label="Snapshot ID" value={edge.snapshot_id} testId="blame-edge-snapshot-id" mono />
            <MetadataCell
              label="Provenance Commit"
              idChip
              value={edge.provenance_commit ?? ""}
              testId="blame-edge-provenance-commit"
              mono
            />
          </div>
        </div>
      </div>
    </section>
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
      <span className="text-[11px] font-normal lowercase text-muted-foreground">{label}</span>
      <input
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        data-testid={testId}
        className="h-9 w-full rounded-md border border-input bg-background px-3 py-2 text-xs text-text-1 outline-none transition-colors placeholder:text-text-3 focus:border-primary focus:ring-1 focus:ring-primary"
      />
    </label>
  );
}

function MetadataCell({
  label,
  value,
  testId,
  mono = false,
  idChip = false,
}: {
  label: string;
  value: string;
  testId?: string;
  mono?: boolean;
  idChip?: boolean;
}) {
  return (
    <div className="min-w-0">
      <div className="mb-0.5 text-[11px] font-normal lowercase text-muted-foreground">
        {label}
      </div>
      <div
        className={cn("break-all text-xs text-foreground", mono && "")}
        data-testid={testId}
      >
        <MetadataValue value={value} label={label} idChip={idChip} />
      </div>
    </div>
  );
}

function cleanParam(value?: string): string | undefined {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}

function buildSearch(from: string, to: string, task: string): BlameSearch {
  return {
    from: cleanParam(from),
    to: cleanParam(to),
    task: cleanParam(task),
  };
}

function formatCoverage(coverage: string): string {
  if (coverage === "topology+image+command") {
    return "Topology, image, and command";
  }
  return coverage;
}


function testIdSlug(value: string): string {
  const slug = value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "");
  return slug || "element";
}
