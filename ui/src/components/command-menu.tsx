import * as React from "react"
import { useNavigate } from "@tanstack/react-router"
import {
  Database,
  LayoutDashboard,
  BarChart,
  Circle,
  FileCode2,
  Radio,
  Server,
  Search,
} from "lucide-react"

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

function normalizeSearchText(value: string) {
  return value.trim().toLowerCase()
}

function searchWords(value: string) {
  return normalizeSearchText(value).split(/[^a-z0-9]+/).filter(Boolean)
}

function fieldScore(field: string, term: string) {
  const normalized = normalizeSearchText(field)
  if (!normalized) return 0
  if (normalized === term) return 1
  if (normalized.startsWith(term)) return 0.9
  if (searchWords(normalized).some((word) => word.startsWith(term))) return 0.8
  if (normalized.includes(term)) return 0.75
  return 0
}

export function commandPaletteFilter(value: string, search: string, keywords?: string[]) {
  const terms = searchWords(search)
  if (terms.length === 0) return 1

  const fields = [value, ...(keywords ?? [])]
  let score = 0
  for (const term of terms) {
    const termScore = Math.max(...fields.map((field) => fieldScore(field, term)))
    if (termScore === 0) return 0
    score += termScore
  }
  return score / terms.length
}

export function CommandMenu() {
  const [open, setOpen] = React.useState(false)
  const navigate = useNavigate()

  const { data: jobs } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    enabled: open,
  })

  const { data: triggers } = useQuery({
    queryKey: ["triggers"],
    queryFn: api.getTriggers,
    enabled: open,
  })

  const { data: atoms } = useQuery({
    queryKey: ["atoms"],
    queryFn: api.getAtoms,
    enabled: open,
  })

  React.useEffect(() => {
    const down = (e: KeyboardEvent) => {
      if (e.key === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        setOpen((open) => !open)
      }
    }

    document.addEventListener("keydown", down)
    return () => document.removeEventListener("keydown", down)
  }, [])

  const runCommand = React.useCallback((command: () => void) => {
    setOpen(false)
    command()
  }, [])

  return (
    <>
      <button
        onClick={() => setOpen(true)}
        className="flex items-center gap-2 px-3 py-1.5 text-sm text-muted-foreground border rounded-md hover:bg-muted transition-colors w-64 justify-between"
      >
        <div className="flex items-center gap-2">
          <Search className="h-4 w-4" />
          <span>Search...</span>
        </div>
        <kbd className="pointer-events-none inline-flex h-5 select-none items-center gap-1 rounded border bg-muted px-1.5 font-mono text-[10px] font-medium text-muted-foreground opacity-100">
          <span className="text-xs">⌘</span>K
        </kbd>
      </button>
      <CommandDialog open={open} onOpenChange={setOpen} commandProps={{ filter: commandPaletteFilter }}>
        <CommandInput placeholder="Type a command or search..." />
        <CommandList>
          <CommandEmpty>No results found.</CommandEmpty>
          <CommandGroup heading="Suggestions">
            <CommandItem value="jobs pipelines" onSelect={() => runCommand(() => navigate({ to: "/jobs" }))}>
              <LayoutDashboard className="mr-2 h-4 w-4" />
              <span>Jobs</span>
            </CommandItem>
            <CommandItem value="stats analytics" onSelect={() => runCommand(() => navigate({ to: "/stats" }))}>
              <BarChart className="mr-2 h-4 w-4" />
              <span>Stats</span>
            </CommandItem>
            <CommandItem value="triggers schedules events" onSelect={() => runCommand(() => navigate({ to: "/triggers" }))}>
              <Radio className="mr-2 h-4 w-4" />
              <span>Triggers</span>
            </CommandItem>
            <CommandItem value="atoms containers" onSelect={() => runCommand(() => navigate({ to: "/atoms" }))}>
              <Database className="mr-2 h-4 w-4" />
              <span>Atoms</span>
            </CommandItem>
            <CommandItem value="system health nodes" onSelect={() => runCommand(() => navigate({ to: "/system" }))}>
              <Server className="mr-2 h-4 w-4" />
              <span>System</span>
            </CommandItem>
            <CommandItem value="job definitions manifests yaml" onSelect={() => runCommand(() => navigate({ to: "/jobdefs" }))}>
              <FileCode2 className="mr-2 h-4 w-4" />
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
                  <Circle className="mr-2 h-4 w-4 text-running" />
                  <span>{job.alias}</span>
                  <span className="ml-2 text-xs text-muted-foreground font-mono">{shortId(job.id)}</span>
                </div>
                <div className="text-[10px] text-muted-foreground">
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
                  <Radio className="mr-2 h-4 w-4 text-cyan-glow" />
                  <span>{trigger.alias}</span>
                  <span className="ml-2 text-xs text-muted-foreground font-mono">{shortId(trigger.id)}</span>
                </div>
                <div className="text-[10px] uppercase text-muted-foreground">{trigger.type}</div>
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
                  <Database className="mr-2 h-4 w-4 shrink-0 text-success" />
                  <span className="truncate">{atom.image}</span>
                  <span className="ml-2 text-xs text-muted-foreground font-mono">{shortId(atom.id)}</span>
                </div>
                <div className="text-[10px] uppercase text-muted-foreground">{atom.engine}</div>
              </CommandItem>
            ))}
          </CommandGroup>
        </CommandList>
      </CommandDialog>
    </>
  )
}
