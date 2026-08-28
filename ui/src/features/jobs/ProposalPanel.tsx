import { useMemo } from "react";
import { FileDiff } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { formatByteSize, parseProposal, type ProposalArtifact, type ProposalSection } from "./proposal-renderers";

interface ProposalPanelProps {
  /** The task's output map (`TaskRun.output`). Renders nothing when it carries no `proposal_kind`. */
  output?: Record<string, string>;
}

/**
 * Renders a propose step's output as a reviewable proposal (spec §5.6): a
 * convention over three reserved `##caesium::output` keys, not a schema or
 * an endpoint. The Console never fetches the referenced artifact — it shows
 * the summary and the reference (digest/size/path), never a download.
 */
export function ProposalPanel({ output }: ProposalPanelProps) {
  const proposal = useMemo(() => parseProposal(output), [output]);

  if (!proposal) {
    return null;
  }

  return (
    <section data-testid="proposal-panel" className="rounded-lg border border-border/60 bg-muted/30 p-3">
      <div className="mb-3 flex items-center gap-2">
        <FileDiff className="h-4 w-4 text-primary" />
        <div className="min-w-0 flex-1">
          <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Proposal</div>
          <div className="text-xs text-text-3">
            A reviewable change this task proposed. Caesium is not in the data path — it renders the summary and the artifact reference only.
          </div>
        </div>
        <Badge data-testid="proposal-kind" variant="outline" className="shrink-0 font-mono text-[10px]">
          {proposal.kind}
        </Badge>
      </div>

      <ProposalSectionView section={proposal.section} />

      {proposal.artifact ? <ProposalArtifactView artifact={proposal.artifact} /> : null}
    </section>
  );
}

function ProposalSectionView({ section }: { section: ProposalSection }) {
  if (section.kind === "structured") {
    return <StructuredProposal section={section} />;
  }
  return <GenericProposal section={section} />;
}

function StructuredProposal({ section }: { section: Extract<ProposalSection, { kind: "structured" }> }) {
  return (
    <div className="space-y-3">
      {section.counts.length > 0 ? (
        <div data-testid="proposal-counts" className="flex flex-wrap gap-2">
          {section.counts.map((count) => (
            <Badge
              key={count.action}
              data-testid="proposal-count"
              data-action={count.action}
              variant={countBadgeVariant(count.action)}
              className="gap-1 font-mono text-[11px]"
            >
              <span className="capitalize">{count.action}</span>
              <span>{count.count}</span>
            </Badge>
          ))}
        </div>
      ) : null}

      {section.resources.length > 0 ? (
        <div>
          <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            Resources
          </div>
          <div data-testid="proposal-resource-table" className="rounded-md border border-border/60">
            <div className="grid grid-cols-[1fr_100px] gap-0 border-b border-border/40 bg-muted/30 px-2 py-1 text-[10px] font-medium text-muted-foreground">
              <span>Address</span>
              <span>Action</span>
            </div>
            <div className="max-h-64 overflow-auto">
              {section.resources.map((resource, index) => (
                <div
                  key={`${resource.address}:${index}`}
                  data-testid="proposal-resource-row"
                  data-address={resource.address}
                  data-action={resource.action}
                  className="grid grid-cols-[1fr_100px] items-center gap-0 border-b border-border/30 px-2 py-1 text-[11px] last:border-b-0"
                >
                  <span className="truncate font-mono" title={resource.address}>
                    {resource.address}
                  </span>
                  <span>
                    <Badge variant={countBadgeVariant(resource.action)} className="font-mono text-[10px]">
                      {resource.action}
                    </Badge>
                  </span>
                </div>
              ))}
            </div>
          </div>
          {section.resourcesTruncated ? (
            <div data-testid="proposal-resources-truncated" className="mt-1 text-[10px] text-muted-foreground">
              Showing the first {section.resources.length} resources.
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function GenericProposal({ section }: { section: Extract<ProposalSection, { kind: "generic" }> }) {
  return (
    <div className="space-y-2">
      {section.parseError ? (
        <div data-testid="proposal-summary-parse-error" className="text-[11px] text-warning">
          proposal_summary could not be parsed as JSON; showing the raw value below.
        </div>
      ) : null}
      <div data-testid="proposal-generic-entries" className="rounded-md border border-border/60 bg-background/40 p-2 space-y-1">
        {section.entries.map((entry) => (
          <div key={entry.key} data-testid="proposal-generic-entry" data-key={entry.key} className="flex gap-2 font-mono text-xs">
            <span className="shrink-0 font-semibold text-muted-foreground">{entry.key}:</span>
            <span className="break-all text-foreground">{entry.value}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function ProposalArtifactView({ artifact }: { artifact: ProposalArtifact }) {
  if (artifact.malformed) {
    return (
      <div
        data-testid="proposal-artifact-missing"
        className="mt-3 rounded-md border border-border/60 bg-background/40 p-2 text-[11px] text-muted-foreground"
      >
        proposal_artifact names <span className="font-mono">{artifact.key}</span>, but no usable output-ref was
        found under that key.
      </div>
    );
  }

  return (
    <div data-testid="proposal-artifact" className="mt-3 rounded-md border border-border/60 bg-background/40 p-2">
      <div className="mb-1.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        Artifact reference ({artifact.key})
      </div>
      <div className="grid gap-2 sm:grid-cols-3">
        <ArtifactCell label="digest" value={artifact.digest ?? "—"} />
        <ArtifactCell label="size" value={formatByteSize(artifact.size)} />
        <ArtifactCell label="path" value={artifact.path ?? "—"} />
      </div>
    </div>
  );
}

function ArtifactCell({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">{label}</div>
      <div data-testid={`proposal-artifact-${label}`} className="mt-0.5 break-all font-mono text-xs text-foreground" title={value}>
        {value}
      </div>
    </div>
  );
}

function countBadgeVariant(action: string): "success" | "destructive" | "secondary" | "outline" {
  switch (action) {
    case "add":
      return "success";
    case "destroy":
      return "destructive";
    case "change":
    case "replace":
      return "secondary";
    default:
      return "outline";
  }
}
