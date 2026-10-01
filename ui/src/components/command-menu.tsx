import * as React from "react"
import { useNavigate } from "@tanstack/react-router"
import {
  CommandDialog,
  Command,
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
  const [desktop, setDesktop] = React.useState(() => window.matchMedia("(min-width: 1024px)").matches)
  const triggerRef = React.useRef<HTMLButtonElement>(null)
  const inputRef = React.useRef<HTMLInputElement>(null)

  React.useEffect(() => {
    const media = window.matchMedia("(min-width: 1024px)");
    const resize = () => setDesktop(media.matches);
    media.addEventListener("change", resize);
    return () => media.removeEventListener("change", resize);
  }, []);
  React.useEffect(() => {
    if (open && desktop) inputRef.current?.focus();
  }, [open, desktop]);
  const { data: jobs } = useQuery({ queryKey: ["jobs"], queryFn: api.getJobs, enabled: open });
  const { data: triggers } = useQuery({ queryKey: ["triggers"], queryFn: api.getTriggers, enabled: open });
  const { data: atoms } = useQuery({ queryKey: ["atoms"], queryFn: api.getAtoms, enabled: open });
  React.useEffect(() => {
    const down = (e: KeyboardEvent) => {
      const typing = e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement || (e.target as HTMLElement).isContentEditable;
      if ((e.key === "k" && (e.metaKey || e.ctrlKey)) || (e.key === ":" && !typing)) {
        e.preventDefault(); setOpen(value => !value);
      }
      if (e.key === "Escape" && open) { setOpen(false); triggerRef.current?.focus(); }
    };
    document.addEventListener("keydown", down);
    return () => document.removeEventListener("keydown", down);
  }, [open]);
  const runCommand = React.useCallback((command: () => void) => {
    setOpen(false); triggerRef.current?.focus(); command();
  }, []);
  const contents = (

    <>
        <CommandInput ref={inputRef} aria-label="Type a command or search" placeholder="Type a command or search..." />
        <CommandList>
          <CommandEmpty>No results found.</CommandEmpty>
          <CommandGroup heading="Suggestions">
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
                value={`job ${job.alias} ${shortId(job.id)}`}
                keywords={[job.alias, shortId(job.id)]}
                onSelect={() => runCommand(() => navigate({ to: "/jobs/$jobId", params: { jobId: job.id } }))}
                className="flex items-center justify-between"
              >
                <div className="flex items-center">
                  <span>{job.alias}</span>
                </div>
                <div className="text-[11px] text-muted-foreground">
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
                value={`trigger ${trigger.alias} ${shortId(trigger.id)}`}
                keywords={[trigger.alias, shortId(trigger.id), trigger.type]}
                onSelect={() => runCommand(() => navigate({ to: "/triggers" }))}
                className="flex items-center justify-between"
              >
                <div className="flex items-center">
                  <span>{trigger.alias}</span>
                </div>
                <div className="text-[11px] lowercase text-muted-foreground">{trigger.type}</div>
              </CommandItem>
            ))}
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Atoms">
            {atoms?.map((atom) => (
              <CommandItem
                key={atom.id}
                value={`atom ${atom.image} ${atom.engine} ${shortId(atom.id)}`}
                keywords={[atom.image, atom.engine, shortId(atom.id)]}
                onSelect={() => runCommand(() => navigate({ to: "/atoms" }))}
                className="flex items-center justify-between"
              >
                <div className="flex min-w-0 items-center">
                  <span className="truncate" title={atom.image}>{atom.image.split("@")[0]}</span>
                </div>
                <div className="text-[11px] lowercase text-muted-foreground">{atom.engine}</div>
              </CommandItem>
            ))}
          </CommandGroup>
        </CommandList>
    </>
  );
  return <footer className="relative z-30 flex h-10 shrink-0 items-center justify-between border-t border-border bg-obsidian px-4 lg:px-6">
    <button ref={triggerRef} type="button" aria-label="Open search" aria-expanded={open} aria-haspopup="dialog" onClick={() => setOpen(!open)} className="flex h-full items-center gap-2 text-[13px] text-text-2">
      <span className="hidden lg:inline">caesium</span><span className="text-cyan">❯</span><span aria-hidden="true" className="cs-cursor" />
    </button>
    <div className="flex items-center gap-7 text-[11px] text-text-3">
      <span className="hidden xl:inline">why task</span><span className="hidden xl:inline">blame run</span><span className="hidden xl:inline">diff run run</span><span className="hidden xl:inline">replay run</span><span className="hidden xl:inline">verify receipt</span><kbd>⌘K</kbd>
    </div>
    {desktop ? open ? <div role="dialog" aria-label="Command palette" className="absolute bottom-10 left-0 right-0 border-t border-border bg-midnight p-2 shadow-sm">
      <Command filter={commandPaletteFilter}>{contents}</Command>
    </div> : null : <CommandDialog open={open} onOpenChange={setOpen} commandProps={{ filter: commandPaletteFilter }}>{contents}</CommandDialog>}
  </footer>;
}
