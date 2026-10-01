import { cn } from "@/lib/utils";
import { resolveStatusForDomain, type StatusDomain, type StatusMeta } from "@/lib/status";

export type StatusBadgeVariant = "word" | "glyph";
export type StatusBadgeSize = "sm" | "md";

export function StatusGlyph({ meta, className, testId }: { meta: StatusMeta; className?: string; testId?: string }) {
  return <span aria-hidden="true" data-testid={testId} data-shape={meta.shape}
    className={cn("cs-status-glyph", `cs-status-${meta.shape}`, meta.dotClass, className)}
    style={{ color: meta.fg }} />;
}

export function StatusBadge({ status, domain, variant = "word", size = "md", label, className }: {
  status: string;
  domain?: StatusDomain;
  variant?: StatusBadgeVariant;
  size?: StatusBadgeSize;
  label?: string;
  className?: string;
}) {
  const { key, meta } = resolveStatusForDomain(status, domain);
  const text = label ?? meta.label;
  return <span className={cn("inline-flex items-center gap-2 whitespace-nowrap font-bold lowercase", size === "sm" ? "text-xs" : "text-[13px]", className)}
    style={{ color: meta.fg }} data-status={key ?? "unknown"} data-variant={variant}
    aria-label={variant === "glyph" ? text : undefined}>
    <StatusGlyph meta={meta} />
    {variant === "word" ? text : <span className="sr-only">{text}</span>}
  </span>;
}
