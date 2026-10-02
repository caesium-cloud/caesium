import { formatCommand } from "@/lib/command-display";
import { FilterChip } from "@/components/ui/filter-chip";
import { PageHeader as ConsolePageHeader } from "@/components/ui/page-header";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Atom } from "@/lib/api";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { IdChip } from "@/components/ui/id-chip";
import { ImageReference } from "@/components/ui/image-reference";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "sonner";
import { X, ChevronDown, ChevronRight, ChevronLeft } from "lucide-react";
import { RelativeTime } from "@/components/relative-time";
import { Fragment, useState, useMemo } from "react";

const PAGE_SIZE = 25;

export function AtomsPage() {
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [engineFilter, setEngineFilter] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);
  const [page, setPage] = useState(0);

  const { data: atoms, isLoading, error } = useQuery({
    queryKey: ["atoms"],
    queryFn: api.getAtoms,
    refetchInterval: 30000,
  });

  const deleteMutation = useMutation({
    mutationFn: api.deleteAtom,
    onSuccess: (_, id) => {
      queryClient.setQueryData(["atoms"], (old: Atom[] | undefined) =>
        old?.filter(a => a.id !== id)
      );
      toast.success("Atom deleted");
      setDeleteConfirm(null);
    },
    onError: () => toast.error("Failed to delete atom"),
  });

  const engines = useMemo(() => {
    const set = new Set<string>();
    atoms?.forEach(a => set.add(a.engine));
    return Array.from(set);
  }, [atoms]);

  const filtered = useMemo(() => {
    let result = atoms || [];
    if (search) {
      const q = search.toLowerCase();
      result = result.filter(a =>
        a.id.includes(q) || a.image.toLowerCase().includes(q) || a.command.toLowerCase().includes(q) || a.engine.toLowerCase().includes(q)
      );
    }
    if (engineFilter) result = result.filter(a => a.engine === engineFilter);
    return result;
  }, [atoms, search, engineFilter]);

  const totalPages = Math.ceil(filtered.length / PAGE_SIZE);
  const pageAtoms = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  if (isLoading) return (
    <div className="p-8 space-y-4">
      <Skeleton className="h-8 w-48" />
      <Skeleton className="h-64 w-full" />
    </div>
  );
  if (error) return <div className="p-8 text-center text-destructive">Error loading atoms: {error.message}</div>;

  return (
    <div className="space-y-5">
      <ConsolePageHeader title="Atoms" description="Reusable container execution units." count={`${filtered.length} atoms`} />

      {/* Filters */}
      <div className="flex flex-wrap gap-2 items-center">
        <div className="relative flex-1 min-w-[200px] max-w-sm">

          <input
            value={search}
            onChange={e => { setSearch(e.target.value); setPage(0); }}
            placeholder="Search by image, command, ID..."
            className="w-full rounded-md border bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
          {search && (
            <button aria-label="Clear atom search" onClick={() => { setSearch(""); setPage(0); }} className="absolute right-2.5 top-2.5 text-muted-foreground hover:text-foreground">
              <X className="h-4 w-4" />
            </button>
          )}
        </div>
        {engines.map(engine => (
          <FilterChip key={engine} active={engineFilter === engine} onClick={() => { setEngineFilter(engineFilter === engine ? null : engine); setPage(0); }}>
            {engine}
          </FilterChip>
        ))}
      </div>

      <div className="border-y border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-8"></TableHead>
              <TableHead>Atom / image</TableHead>
              <TableHead>Engine</TableHead>
              <TableHead>Command arguments</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageAtoms.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="h-24 text-center text-muted-foreground">
                  No atoms found.
                </TableCell>
              </TableRow>
            )}
            {pageAtoms.map(atom => (
              <Fragment key={atom.id}>
                <TableRow
                  key={atom.id}
                  className="cursor-pointer"
                  onClick={() => setExpanded(expanded === atom.id ? null : atom.id)}
                >
                  <TableCell>
                    <button aria-label={`Details for atom ${atom.id}`} aria-expanded={expanded === atom.id} onClick={event => { event.stopPropagation(); setExpanded(expanded === atom.id ? null : atom.id); }}>
                      {expanded === atom.id ? <ChevronDown className="h-4 w-4 text-text-3" /> : <ChevronRight className="h-4 w-4 text-text-3" />}
                    </button>
                  </TableCell>
                  <TableCell className="min-w-[180px] max-w-[280px]">
                    {typeof atom.spec?.name === "string" && atom.spec.name !== atom.image ? <div className="break-words text-sm font-bold">{atom.spec.name}</div> : null}
                    <div className="text-sm"><ImageReference image={atom.image} /></div><IdChip value={atom.id} label="atom id" />
                  </TableCell>
                  <TableCell>
                    <span className="text-xs text-text-3">{atom.engine}</span>
                  </TableCell>
                  <TableCell className="text-xs max-w-[200px] truncate text-muted-foreground" title={formatCommand(atom.command)}>
                    <span className="rounded-sm bg-code-bg px-2 py-1 text-code-fg">{formatCommand(atom.command)}</span>
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground whitespace-nowrap">
                    <RelativeTime date={atom.created_at} />
                  </TableCell>
                  <TableCell className="text-right" onClick={e => e.stopPropagation()}>
                    {deleteConfirm === atom.id ? (
                      <div className="flex items-center justify-end gap-1">
                        <Button
                          variant="destructive"
                          size="sm"
                          onClick={() => deleteMutation.mutate(atom.id)}
                          disabled={deleteMutation.isPending}
                        >
                          Confirm
                        </Button>
                        <Button variant="ghost" size="sm" onClick={() => setDeleteConfirm(null)}>
                          Cancel
                        </Button>
                      </div>
                    ) : (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-danger hover:bg-danger/10 hover:text-danger"
                        onClick={() => setDeleteConfirm(atom.id)}
                        title="Delete Atom"
                      >
                        <span>Delete Atom</span>
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
                {expanded === atom.id && (
                  <TableRow key={`${atom.id}-detail`} className="bg-muted/30 hover:bg-muted/30">
                    <TableCell colSpan={6}>
                      <div className="py-2 px-2 space-y-3">
                        <div className="grid grid-cols-2 gap-4 text-sm">
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Atom ID</p>
                            <IdChip value={atom.id} label="atom id" />
                          </div>
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Updated</p>
                            <p className="text-xs"><RelativeTime date={atom.updated_at} /></p>
                          </div>
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Image</p>
                            <p className="text-xs break-all"><ImageReference image={atom.image} /></p>
                          </div>
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Command</p>
                            <pre className="whitespace-pre-wrap break-words text-xs" aria-label="Command arguments (quoted)">{formatCommand(atom.command)}</pre>
                            <details className="mt-2 text-xs"><summary className="cursor-pointer text-text-3">Raw command</summary><pre className="mt-2 whitespace-pre-wrap break-all">{atom.command}</pre></details>
                          </div>
                        </div>
                        {atom.spec && Object.keys(atom.spec).length > 0 && (
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Spec</p>
                            <pre className="bg-code-bg text-code-fg rounded p-3 text-xs overflow-auto max-h-40">
                              {JSON.stringify(atom.spec, null, 2)}
                            </pre>
                          </div>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                )}
              </Fragment>
            ))}
          </TableBody>
        </Table>
      </div>

      {totalPages > 1 && (
        <div className="flex items-center justify-between text-sm text-muted-foreground">
          <span>Showing {page * PAGE_SIZE + 1}–{Math.min((page + 1) * PAGE_SIZE, filtered.length)} of {filtered.length}</span>
          <div className="flex items-center gap-2">
            <Button variant="outline" size="icon" onClick={() => setPage(p => p - 1)} disabled={page === 0}>
              <ChevronLeft className="h-4 w-4" />
            </Button>
            <span className="px-2">Page {page + 1} of {totalPages}</span>
            <Button variant="outline" size="icon" onClick={() => setPage(p => p + 1)} disabled={page >= totalPages - 1}>
              <ChevronRight className="h-4 w-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
