import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";
import { toast } from "sonner";
import { cn } from "@/lib/utils";
import { copyText } from "@/lib/clipboard";

/** Diagnostic identifiers never take the place of a human-readable name. */
export function IdChip({ value, label = "id", className }: { value: string; label?: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  async function copy() {
    try {
      await copyText(value);
      setCopied(true);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1000);
    } catch {
      setCopyFailed(true);
      toast.error(`Select and copy the full ${label}`);
    }
  }
  if (copyFailed) return <input readOnly autoFocus value={value} aria-label={`Full ${label}`} title={value}
    className={cn("h-[22px] max-w-full rounded-md border border-input bg-transparent px-1.5 text-xs text-text-1", className)}
    onFocus={event => event.currentTarget.select()}
    onClick={event => { event.preventDefault(); event.stopPropagation(); event.currentTarget.select(); }} />;
  return <button type="button" title={value} aria-label={`Copy ${label}: ${value}`}
    className={cn("inline-flex h-[22px] max-w-full items-center gap-1.5 rounded-md border border-input px-1.5 text-xs font-normal text-text-3", className)}
    onClick={(event) => { event.preventDefault(); event.stopPropagation(); void copy(); }}>
    <span className="truncate">{value.replace(/^sha256:/, "").slice(0, 8)}</span>
    {copied ? <Check aria-hidden="true" className="h-3 w-3 shrink-0 text-success" /> : <Copy aria-hidden="true" className="h-3 w-3 shrink-0" />}
    {copied ? <span role="status" className="sr-only">Copied</span> : null}
  </button>;
}
