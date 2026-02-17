import * as React from "react"
import { useNavigate } from "@tanstack/react-router"
import {
  LayoutDashboard,
  Play,
  Box,
  BarChart,
  Circle,
  Search
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
import { RelativeTime } from "./relative-time"

export function CommandMenu() {
  const [open, setOpen] = React.useState(false)
  const navigate = useNavigate()

  const { data: jobs } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
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
      <CommandDialog open={open} onOpenChange={setOpen}>
        <CommandInput placeholder="Type a command or search..." />
        <CommandList>
          <CommandEmpty>No results found.</CommandEmpty>
          <CommandGroup heading="Suggestions">
            <CommandItem onSelect={() => runCommand(() => navigate({ to: "/jobs" }))}>
              <LayoutDashboard className="mr-2 h-4 w-4" />
              <span>Jobs</span>
            </CommandItem>
            <CommandItem onSelect={() => runCommand(() => navigate({ to: "/triggers" }))}>
              <Play className="mr-2 h-4 w-4" />
              <span>Triggers</span>
            </CommandItem>
            <CommandItem onSelect={() => runCommand(() => navigate({ to: "/atoms" }))}>
              <Box className="mr-2 h-4 w-4" />
              <span>Atoms</span>
            </CommandItem>
            <CommandItem onSelect={() => runCommand(() => navigate({ to: "/stats" }))}>
              <BarChart className="mr-2 h-4 w-4" />
              <span>Stats</span>
            </CommandItem>
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Jobs">
            {jobs?.map((job) => (
              <CommandItem
                key={job.id}
                onSelect={() => runCommand(() => navigate({ to: "/jobs/$jobId", params: { jobId: job.id } }))}
                className="flex items-center justify-between"
              >
                <div className="flex items-center">
                  <Circle className="mr-2 h-4 w-4 text-blue-500" />
                  <span>{job.alias}</span>
                  <span className="ml-2 text-xs text-muted-foreground font-mono">{job.id.substring(0, 8)}</span>
                </div>
                <div className="text-[10px] text-muted-foreground">
                  <RelativeTime date={job.created_at} />
                </div>
              </CommandItem>
            ))}
          </CommandGroup>
        </CommandList>
      </CommandDialog>
    </>
  )
}
