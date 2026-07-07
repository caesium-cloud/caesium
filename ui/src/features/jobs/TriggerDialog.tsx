import { useMemo, useState, type FormEvent } from "react";
import { Play } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

interface TriggerDialogProps {
  open: boolean;
  disabled?: boolean;
  isPending?: boolean;
  onConfirm: (params: Record<string, string>) => void;
  onOpenChange: (open: boolean) => void;
}

export function TriggerDialog({
  open,
  disabled,
  isPending,
  onConfirm,
  onOpenChange,
}: TriggerDialogProps) {
  const [logicalDate, setLogicalDate] = useState("");
  const [paramLines, setParamLines] = useState("");
  const [validationError, setValidationError] = useState<string | null>(null);

  function handleOpenChange(next: boolean) {
    if (!next) {
      setLogicalDate("");
      setParamLines("");
      setValidationError(null);
    }
    onOpenChange(next);
  }

  const inputClassName = useMemo(
    () =>
      "w-full rounded-md border border-border bg-background px-3 py-1.5 text-sm text-foreground " +
      "focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50",
    [],
  );
  const labelClassName = "block text-xs uppercase tracking-wide text-muted-foreground mb-1";

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setValidationError(null);

    const parsed = parseParamLines(paramLines);
    if (parsed.error) {
      setValidationError(parsed.error);
      return;
    }

    const params = { ...parsed.params };
    const trimmedLogicalDate = logicalDate.trim();
    if (trimmedLogicalDate) {
      params.logical_date = trimmedLogicalDate;
    }

    onConfirm(params);
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>Trigger Job</DialogTitle>
          <DialogDescription>
            Confirm a manual run and optionally pass run parameters.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4 pt-1">
          <div>
            <label htmlFor="trigger-logical-date" className={labelClassName}>
              logical_date
            </label>
            <input
              id="trigger-logical-date"
              value={logicalDate}
              onChange={(event) => setLogicalDate(event.target.value)}
              disabled={isPending || disabled}
              className={inputClassName}
              placeholder="2026-07-07T12:00:00Z"
            />
          </div>
          <div>
            <label htmlFor="trigger-extra-params" className={labelClassName}>
              Additional params
            </label>
            <textarea
              id="trigger-extra-params"
              value={paramLines}
              onChange={(event) => setParamLines(event.target.value)}
              disabled={isPending || disabled}
              className={`${inputClassName} min-h-24 font-mono text-xs`}
              placeholder="key=value"
            />
          </div>

          {validationError ? (
            <p className="text-xs text-destructive">{validationError}</p>
          ) : null}

          <DialogFooter>
            <Button type="button" variant="outline" size="sm" onClick={() => handleOpenChange(false)} disabled={isPending}>
              Cancel
            </Button>
            <Button type="submit" size="sm" disabled={isPending || disabled}>
              <Play className="mr-1.5 h-3.5 w-3.5" />
              {isPending ? "Triggering..." : "Confirm Trigger"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function parseParamLines(raw: string): { params: Record<string, string>; error?: string } {
  const params: Record<string, string> = {};
  const lines = raw.split(/\r?\n/);
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index].trim();
    if (!line) continue;

    const separatorIndex = line.indexOf("=");
    if (separatorIndex <= 0) {
      return { params: {}, error: `Line ${index + 1} must be key=value` };
    }

    const key = line.slice(0, separatorIndex).trim();
    const value = line.slice(separatorIndex + 1).trim();
    if (!key) {
      return { params: {}, error: `Line ${index + 1} is missing a key` };
    }
    params[key] = value;
  }

  return { params };
}
