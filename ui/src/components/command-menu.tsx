import * as React from "react"
import { Search } from "lucide-react"
import { useNavigate } from "@tanstack/react-router"
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command"
import { useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { shortId } from "@/lib/utils"
import { RelativeTime } from "./relative-time"
import { commandPaletteFilter } from "./command-filter"

export function CommandMenu() {
  const [open, setOpen] = React.useState(false)
  const navigate = useNavigate()
  const triggerRef = React.useRef<HTMLButtonElement>(null)
  const inputRef = React.useRef<HTMLInputElement>(null)
  const shortcut = /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘ K" : "Ctrl K"

  const { data: jobs } = useQuery({ queryKey: ["jobs"], queryFn: api.getJobs, enabled: open });
  const { data: triggers } = useQuery({ queryKey: ["triggers"], queryFn: api.getTriggers, enabled: open });
  const { data: atoms } = useQuery({ queryKey: ["atoms"], queryFn: api.getAtoms, enabled: open });
  React.useEffect(() => {
    const down = (e: KeyboardEvent) => {
      const typing = e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement || (e.target as HTMLElement).isContentEditable;
      if (!e.repeat && ((e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey)) || (e.key === ":" && !typing))) {
        e.preventDefault(); setOpen(value => !value);
      }
    };
    document.addEventListener("keydown", down);
    return () => document.removeEventListener("keydown", down);
  }, []);
  const runCommand = React.useCallback((command: () => void) => {
    setOpen(false); triggerRef.current?.focus(); command();
  }, []);
  const contents = (

    <>
        <CommandInput ref={inputRef} aria-label="Search pages, jobs, triggers, or atoms" placeholder="Search by name or ID…" />
        <CommandList>
          <CommandEmpty>No results found.</CommandEmpty>
          <CommandGroup heading="Pages">
            <CommandItem value="jobs pipelines" onSelect={() => runCommand(() => navigate({ to: "/jobs" }))}>
              <span>Jobs</span>
            </CommandItem>
            <CommandItem value="stats analytics" onSelect={() => runCommand(() => navigate({ to: "/stats" }))}>
              <span>Stats</span>
            </CommandItem>
            <CommandItem value="triggers schedules events" onSelect={() => runCommand(() => navigate({ to: "/triggers" }))}>
              <span>Triggers</span>
            </CommandItem>
            <CommandItem value="atoms containers" onSelect={() => runCommand(() => navigate({ to: "/atoms" }))}>
              <span>Atoms</span>
            </CommandItem>
            <CommandItem value="system health nodes" onSelect={() => runCommand(() => navigate({ to: "/system" }))}>
              <span>System</span>
            </CommandItem>
            <CommandItem value="job definitions manifests yaml" onSelect={() => runCommand(() => navigate({ to: "/jobdefs" }))}>
              <span>Job Definitions</span>
            </CommandItem>
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Jobs">
            {jobs?.map((job) => (
              <CommandItem
                key={job.id}
                value={`job ${job.alias} ${job.id}`}
                keywords={[job.alias, job.id]}
                onSelect={() => runCommand(() => navigate({ to: "/jobs/$jobId", params: { jobId: job.id } }))}
                className="flex items-center justify-between"
              >
                <div className="min-w-0">
                  <div className="truncate" title={job.alias}>{job.alias}</div>
                  <div className="text-xs text-text-3">{shortId(job.id)}</div>
                </div>
                <div className="shrink-0 text-xs text-text-3">
                  <RelativeTime date={job.created_at} />
                </div>
              </CommandItem>
            ))}
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Triggers">
            {triggers?.map((trigger) => (
              <CommandItem
                key={trigger.id}
                value={`trigger ${trigger.alias} ${trigger.id}`}
                keywords={[trigger.alias, trigger.id, trigger.type]}
                onSelect={() => runCommand(() => navigate({ to: "/triggers" }))}
                className="flex items-center justify-between"
              >
                <div className="min-w-0">
                  <div className="truncate" title={trigger.alias}>{trigger.alias}</div>
                  <div className="text-xs text-text-3">{shortId(trigger.id)}</div>
                </div>
                <div className="shrink-0 text-xs lowercase text-text-3">{trigger.type}</div>
              </CommandItem>
            ))}
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Atoms">
            {atoms?.map((atom) => (
              <CommandItem
                key={atom.id}
                value={`atom ${atom.image} ${atom.engine} ${atom.id}`}
                keywords={[atom.image, atom.engine, atom.id]}
                onSelect={() => runCommand(() => navigate({ to: "/atoms" }))}
                className="flex items-center justify-between"
              >
                <div className="min-w-0">
                  <div className="truncate" title={atom.image}>{atom.image.split("@")[0]}</div>
                  <div className="text-xs text-text-3">{shortId(atom.id)}</div>
                </div>
                <div className="shrink-0 text-xs lowercase text-text-3">{atom.engine}</div>
              </CommandItem>
            ))}
          </CommandGroup>
        </CommandList>
        <div className="flex shrink-0 items-center justify-between gap-3 border-t border-border px-4 py-2 text-xs text-text-3">
          <span><kbd>↑ ↓</kbd> move</span>
          <span><kbd>↵</kbd> open</span>
          <span><kbd>esc</kbd> close</span>
        </div>
    </>
  );
  return <footer className="relative z-30 h-11 shrink-0 border-t border-border bg-obsidian">
    <button ref={triggerRef} type="button" aria-label="Open search" aria-expanded={open} aria-haspopup="dialog" onClick={() => setOpen(true)} className="flex h-full w-full items-center gap-3 px-4 text-left text-[13px] text-text-2 transition-colors hover:bg-graphite hover:text-foreground focus-visible:-outline-offset-4 lg:px-6">
      <Search aria-hidden="true" className="size-4 shrink-0 text-cyan" />
      <span className="min-w-0 flex-1 truncate"><span className="sm:hidden">Search pages and resources…</span><span className="hidden sm:inline">Search pages, jobs, triggers, or atoms…</span></span>
      <kbd className="shrink-0 rounded border border-border bg-midnight px-1.5 py-0.5 text-xs text-text-3">{shortcut}</kbd>
    </button>
    <CommandDialog open={open} onOpenChange={setOpen} commandProps={{ filter: commandPaletteFilter }} contentProps={{
      onOpenAutoFocus: (event) => { event.preventDefault(); inputRef.current?.focus(); },
      onCloseAutoFocus: (event) => { event.preventDefault(); triggerRef.current?.focus(); },
    }}>{contents}</CommandDialog>
  </footer>;
}
