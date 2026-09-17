import { type ReactElement } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

interface GitSyncDialogProps {
  trigger: ReactElement;
}

export function GitSyncDialog({ trigger }: GitSyncDialogProps) {
  return (
    <Dialog>
      <DialogTrigger asChild>{trigger}</DialogTrigger>
      <DialogContent
        data-testid="jobdefs-git-sync-dialog"
        className="bg-midnight border-graphite/50 p-4 text-text-1 sm:max-w-lg sm:rounded-lg sm:p-6"
      >
        <DialogHeader>
          <DialogTitle>Git sync</DialogTitle>
          <DialogDescription className="text-text-3 mt-1.5">
            Git sync is configured on the Caesium server, not from this browser.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3 text-sm leading-relaxed text-text-2">
          <p>
            The scheduler clones repositories listed in{" "}
            <code className="rounded border border-graphite/50 bg-obsidian px-1 py-0.5 font-mono text-xs text-cyan-glow">
              CAESIUM_JOBDEF_GIT_SOURCES
            </code>{" "}
            when{" "}
            <code className="rounded border border-graphite/50 bg-obsidian px-1 py-0.5 font-mono text-xs text-cyan-glow">
              CAESIUM_JOBDEF_GIT_ENABLED=true
            </code>
            . This console cannot start a clone or edit those sources.
          </p>
          <p>
            To apply local YAML from this machine, use{" "}
            <code className="rounded border border-graphite/50 bg-obsidian px-1 py-0.5 font-mono text-xs text-cyan-glow">
              caesium job apply --path
            </code>
            .
          </p>
        </div>
        <DialogFooter className="mt-2">
          <DialogClose asChild>
            <Button
              type="button"
              variant="outline"
              className="border-graphite/50 bg-transparent text-text-2 hover:bg-graphite/20 hover:text-text-1"
            >
              Close
            </Button>
          </DialogClose>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
