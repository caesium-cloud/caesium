interface LogEmptyStateProps {
  title: string;
  body: string;
}

export function LogEmptyState({ title, body }: LogEmptyStateProps) {
  return (
    <div className="absolute inset-0 flex items-center justify-center bg-slate-950/96 px-6 text-center">
      <div className="max-w-sm space-y-2">
        <div className="text-sm font-semibold text-slate-100">{title}</div>
        <div className="text-xs leading-relaxed text-slate-400">{body}</div>
      </div>
    </div>
  );
}
