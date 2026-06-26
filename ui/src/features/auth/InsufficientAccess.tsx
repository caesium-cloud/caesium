import { ShieldAlert } from "lucide-react";
import { cn } from "@/lib/utils";
import { isInsufficientAccessError } from "./access";

interface InsufficientAccessProps {
  error?: unknown;
  message?: string;
  className?: string;
}

export function InsufficientAccess({
  error,
  message = "Your key does not have permission for this action.",
  className,
}: InsufficientAccessProps) {
  if (!isInsufficientAccessError(error)) {
    return null;
  }

  return (
    <div
      role="alert"
      className={cn(
        "inline-flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive",
        className,
      )}
    >
      <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
      <span>{message}</span>
    </div>
  );
}
