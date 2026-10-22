import { useNamespaceView } from "./namespaceView";
import { cn } from "@/lib/utils";

const ALL = "";

/**
 * The header's namespace view filter (identity-and-access F2). Hidden when the
 * principal sees one namespace or fewer. The choice only narrows what the
 * server returns for the principal's grants.
 */
export function NamespaceSwitcher({ className }: { className?: string }) {
  const { selection, options, showSwitcher, select } = useNamespaceView();
  if (!showSwitcher) {
    return null;
  }
  return (
    <label className={cn("flex items-center gap-1.5 text-xs text-text-3", className)}>
      <span className="hidden sm:inline">Namespace</span>
      <select
        data-testid="namespace-switcher"
        aria-label="Namespace"
        className="h-7 max-w-[10rem] truncate rounded-md border border-border bg-background px-2 text-xs text-text-1"
        value={selection ?? ALL}
        onChange={(event) => select(event.target.value === ALL ? null : event.target.value)}
      >
        <option value={ALL}>All namespaces</option>
        {options.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>
    </label>
  );
}

/** The namespace a job lives in (job list rows and job detail). */
export function NamespaceChip({ namespace, className }: { namespace?: string; className?: string }) {
  const name = namespace?.trim() || "default";
  return (
    <span
      data-testid="namespace-chip"
      title={`Namespace ${name}`}
      className={cn(
        "inline-flex items-center rounded border border-border px-1.5 py-0.5 text-[11px] leading-none text-text-3",
        className,
      )}
    >
      {name}
    </span>
  );
}
