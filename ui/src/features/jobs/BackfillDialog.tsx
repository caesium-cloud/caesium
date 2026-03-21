import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CalendarRange } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api, type CreateBackfillRequest } from "@/lib/api";

interface BackfillDialogProps {
  jobId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  disabled?: boolean;
}

// Format a Date to the value expected by datetime-local inputs
function toDatetimeLocal(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    `T${pad(d.getHours())}:${pad(d.getMinutes())}`
  );
}

export function BackfillDialog({ jobId, open, onOpenChange, disabled }: BackfillDialogProps) {
  const queryClient = useQueryClient();

  // Default: end = now, start = 24h ago
  const now = new Date();
  const yesterday = new Date(now.getTime() - 24 * 60 * 60 * 1000);

  const [start, setStart] = useState(toDatetimeLocal(yesterday));
  const [end, setEnd] = useState(toDatetimeLocal(now));
  const [maxConcurrent, setMaxConcurrent] = useState(1);
  const [reprocess, setReprocess] = useState<"none" | "failed" | "all">("none");
  const [validationError, setValidationError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: (body: CreateBackfillRequest) => api.createBackfill(jobId, body),
    onSuccess: () => {
      toast.success("Backfill started");
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "backfills"] });
      onOpenChange(false);
    },
    onError: (err: Error) => {
      toast.error(`Failed to start backfill: ${err.message}`);
    },
  });

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setValidationError(null);

    const startDate = new Date(start);
    const endDate = new Date(end);

    if (endDate <= startDate) {
      setValidationError("End must be after start");
      return;
    }

    mutation.mutate({
      start: startDate.toISOString(),
      end: endDate.toISOString(),
      max_concurrent: maxConcurrent,
      reprocess,
    });
  }

  const inputClass =
    "w-full rounded-md border border-border bg-background px-3 py-1.5 text-sm text-foreground " +
    "focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50";

  const labelClass = "block text-xs uppercase tracking-wide text-muted-foreground mb-1";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>Start Backfill</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4 pt-2">
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelClass}>Start</label>
              <input
                type="datetime-local"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                required
                disabled={mutation.isPending || disabled}
                className={inputClass}
              />
            </div>
            <div>
              <label className={labelClass}>End</label>
              <input
                type="datetime-local"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                required
                disabled={mutation.isPending || disabled}
                className={inputClass}
              />
            </div>
          </div>

          {validationError && (
            <p className="text-xs text-destructive">{validationError}</p>
          )}

          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <label className={labelClass}>Max Concurrent</label>
              <input
                type="number"
                min={1}
                value={maxConcurrent}
                onChange={(e) => setMaxConcurrent(Math.max(1, Number(e.target.value)))}
                disabled={mutation.isPending || disabled}
                className={inputClass}
              />
            </div>
            <div>
              <label className={labelClass}>Reprocess Policy</label>
              <select
                value={reprocess}
                onChange={(e) => setReprocess(e.target.value as "none" | "failed" | "all")}
                disabled={mutation.isPending || disabled}
                className={inputClass}
              >
                <option value="none">None — skip existing</option>
                <option value="failed">Failed — retry failures</option>
                <option value="all">All — reprocess everything</option>
              </select>
            </div>
          </div>

          <div className="flex justify-end pt-2">
            <Button
              type="submit"
              size="sm"
              disabled={mutation.isPending || disabled}
            >
              <CalendarRange className="mr-1.5 h-3.5 w-3.5" />
              {mutation.isPending ? "Starting…" : "Start Backfill"}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}
