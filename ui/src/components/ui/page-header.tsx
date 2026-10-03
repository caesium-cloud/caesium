import type { ReactNode } from "react";

/** A common hierarchy for top-level destinations; filters follow beneath it. */
export function PageHeader({ title, description, count, actions }: { title: string; description?: ReactNode; count?: ReactNode; actions?: ReactNode }) {
  return <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
    <div className="min-w-0 space-y-1">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1"><h1 className="text-2xl font-bold lowercase text-text-1">{title}</h1>{count != null ? <span className="text-xs tabular-nums text-text-3">{count}</span> : null}</div>
      {description ? <p className="max-w-3xl text-sm text-text-3">{description}</p> : null}
    </div>
    {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
  </header>;
}
