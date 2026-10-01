import { cn } from "@/lib/utils";

export function Oscillator({ className }: { className?: string }) {
  return <svg aria-hidden="true" width="120" height="20" viewBox="0 0 120 20" className={cn("cs-oscillator cs-animated shrink-0 text-running", className)}>
    <path className="cs-wave" d="M0 10 Q10 -6 20 10 T40 10 T60 10 T80 10 T100 10 T120 10 T140 10 T160 10" fill="none" stroke="currentColor" strokeWidth="1" />
    <path className="cs-wave-flat" d="M0 10 H120" fill="none" stroke="currentColor" />
  </svg>;
}
