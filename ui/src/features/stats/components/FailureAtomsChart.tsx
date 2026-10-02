import { Link } from '@tanstack/react-router';
import { IdChip } from '@/components/ui/id-chip';
import type { FailingAtom } from '@/lib/api';

/** Labelled bars keep identity and values available to touch and keyboard users. */
export function FailureAtomsChart({ data }: { data: FailingAtom[] }) {
  if (!data.length) return <div className="flex h-[350px] items-center justify-center text-text-3">No failure data available</div>;
  const maximum = Math.max(...data.map(atom => atom.failure_count), 1);
  return <ol className="min-h-[350px] space-y-5 py-3" aria-label="Failing atoms by failure count">
    {data.map(atom => <li key={`${atom.job_id}:${atom.atom_name}`} className="space-y-2">
      <div className="flex items-start justify-between gap-4"><div className="min-w-0">
        <div className="break-words text-sm font-bold">{atom.atom_name || 'Unnamed task'}</div>
        <Link to="/jobs/$jobId" params={{ jobId: atom.job_id }} className="block break-words text-xs text-cyan hover:underline">{atom.alias || 'Job'}</Link>
        <IdChip value={atom.job_id} label="job id" />
      </div><span className="shrink-0 text-sm tabular-nums">{atom.failure_count} <span className="text-xs text-text-3">{atom.failure_count === 1 ? 'failure' : 'failures'}</span></span></div>
      <div aria-hidden="true" className="h-2 bg-obsidian"><div className="h-full rounded-r-sm bg-danger" style={{ width: `${atom.failure_count / maximum * 100}%` }} /></div>
    </li>)}
  </ol>;
}
