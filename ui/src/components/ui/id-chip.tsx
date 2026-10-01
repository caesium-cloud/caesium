import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";
import { toast } from "sonner";
import { cn } from "@/lib/utils";

/** Diagnostic identifiers never take the place of a human-readable name. */
export function IdChip({ value, label = "id", className }: { value: string; label?: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1000);
    } catch {
      toast.error(`Could not copy ${label}`);
    }
  }
  return <button type="button" title={value} aria-label={`Copy ${label}: ${value}`}
    className={cn("inline-flex h-[22px] max-w-full items-center gap-1.5 rounded-md border border-input px-1.5 text-xs font-normal text-text-3", className)}
    onClick={(event) => { event.preventDefault(); event.stopPropagation(); void copy(); }}>
    <span className="truncate">{value.replace(/^sha256:/, "").slice(0, 8)}</span>
    {copied ? <Check aria-hidden="true" className="h-3 w-3 shrink-0 text-success" /> : <Copy aria-hidden="true" className="h-3 w-3 shrink-0" />}
    {copied ? <span role="status" className="sr-only">Copied</span> : null}
  </button>;
}
