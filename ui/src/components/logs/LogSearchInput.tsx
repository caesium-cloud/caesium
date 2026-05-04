import { Search } from "lucide-react";
import { cn } from "@/lib/utils";

interface LogSearchInputProps {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  className?: string;
}

export function LogSearchInput({
  value,
  onChange,
  placeholder = "Filter...",
  className,
}: LogSearchInputProps) {
  return (
    <div
      className={cn(
        "flex min-w-[180px] flex-1 items-center gap-2 rounded-md border border-graphite/50 bg-midnight/60 px-2.5 py-1.5",
        className,
      )}
    >
      <Search className="h-3.5 w-3.5 shrink-0 text-text-4" />
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className="w-full bg-transparent text-xs text-text-1 outline-none placeholder:text-text-4"
      />
    </div>
  );
}
