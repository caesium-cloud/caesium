import type { DiffJobSpec, DiffResponse, DiffUpdate } from "@/lib/api";

export interface DiffPreview {
  added: DiffJobSpec[];
  modified: DiffUpdate[];
  pruneCandidates: DiffJobSpec[];
  pendingCount: number;
  pendingSummary: string;
  pruneSummary: string | null;
}

function jobCountLabel(count: number) {
  return `${count} ${count === 1 ? "job" : "jobs"}`;
}

export function pendingApplyCount(diff: DiffResponse | null | undefined): number {
  if (!diff) return 0;
  return (diff.added?.length ?? 0) + (diff.modified?.length ?? 0);
}

export function pruneCandidateSummary(count: number): string {
  const verb = count === 1 ? "is" : "are";
  return `${jobCountLabel(count)} on the server ${verb} not in this editor (unchanged unless you prune via CLI)`;
}

export function summarizeDiffPreview(diff: DiffResponse): DiffPreview {
  const added = diff.added ?? [];
  const modified = diff.modified ?? [];
  const pruneCandidates = diff.removed ?? [];
  const pendingCount = added.length + modified.length;

  return {
    added,
    modified,
    pruneCandidates,
    pendingCount,
    pendingSummary: `${pendingCount} ${pendingCount === 1 ? "change" : "changes"} pending apply`,
    pruneSummary: pruneCandidates.length === 0 ? null : pruneCandidateSummary(pruneCandidates.length),
  };
}
