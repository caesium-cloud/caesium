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
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "sonner";
import { Trash2, Search, X, ChevronDown, ChevronRight, ChevronLeft, Container } from "lucide-react";
import { RelativeTime } from "@/components/relative-time";
import { useState, useMemo } from "react";
import { cn } from "@/lib/utils";

const PAGE_SIZE = 25;

const ENGINE_VARIANT: Record<string, string> = {
  docker: "default",
  kubernetes: "secondary",
  podman: "outline",
};

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
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Atoms</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Reusable execution units (containers)</p>
        </div>
        <span className="text-sm text-muted-foreground">{filtered.length} atom{filtered.length !== 1 ? "s" : ""}</span>
      </div>

      {/* Filters */}
      <div className="flex flex-wrap gap-2 items-center">
        <div className="relative flex-1 min-w-[200px] max-w-sm">
          <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground pointer-events-none" />
          <input
            value={search}
            onChange={e => { setSearch(e.target.value); setPage(0); }}
            placeholder="Search by image, command, ID..."
            className="w-full rounded-md border bg-background px-3 py-2 pl-8 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
          {search && (
            <button onClick={() => setSearch("")} className="absolute right-2.5 top-2.5 text-muted-foreground hover:text-foreground">
              <X className="h-4 w-4" />
            </button>
          )}
        </div>
        {engines.map(engine => (
          <button
            key={engine}
            onClick={() => { setEngineFilter(engineFilter === engine ? null : engine); setPage(0); }}
            className={cn(
              "rounded-full px-3 py-1 text-xs border transition-colors flex items-center gap-1.5",
              engineFilter === engine
                ? "bg-primary text-primary-foreground border-primary"
                : "bg-background text-muted-foreground border-border hover:border-primary hover:text-primary"
            )}
          >
            <Container className="h-3 w-3" />
            {engine}
          </button>
        ))}
      </div>

      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-8"></TableHead>
              <TableHead>ID</TableHead>
              <TableHead>Engine</TableHead>
              <TableHead>Image</TableHead>
              <TableHead>Command</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageAtoms.length === 0 && (
              <TableRow>
                <TableCell colSpan={7} className="h-24 text-center text-muted-foreground">
                  No atoms found.
                </TableCell>
              </TableRow>
            )}
            {pageAtoms.map(atom => (
              <>
                <TableRow
                  key={atom.id}
                  className="cursor-pointer"
                  onClick={() => setExpanded(expanded === atom.id ? null : atom.id)}
                >
                  <TableCell>
                    {expanded === atom.id
                      ? <ChevronDown className="h-4 w-4 text-muted-foreground" />
                      : <ChevronRight className="h-4 w-4 text-muted-foreground" />}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{atom.id.substring(0, 8)}</TableCell>
                  <TableCell>
                    <Badge variant={(ENGINE_VARIANT[atom.engine] as "default" | "secondary" | "outline") || "outline"}>
                      {atom.engine}
                    </Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs max-w-[200px] truncate" title={atom.image}>
                    {atom.image}
                  </TableCell>
                  <TableCell className="font-mono text-xs max-w-[200px] truncate text-muted-foreground" title={atom.command}>
                    {atom.command}
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
                        size="icon"
                        onClick={() => setDeleteConfirm(atom.id)}
                        className="text-muted-foreground hover:text-destructive"
                        title="Delete Atom"
                      >
                        <Trash2 className="h-4 w-4" />
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
                {expanded === atom.id && (
                  <TableRow key={`${atom.id}-detail`} className="bg-muted/30 hover:bg-muted/30">
                    <TableCell colSpan={7}>
                      <div className="py-2 px-2 space-y-3">
                        <div className="grid grid-cols-2 gap-4 text-sm">
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Full ID</p>
                            <p className="font-mono text-xs">{atom.id}</p>
                          </div>
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Updated</p>
                            <p className="text-xs"><RelativeTime date={atom.updated_at} /></p>
                          </div>
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Image</p>
                            <p className="font-mono text-xs break-all">{atom.image}</p>
                          </div>
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Command</p>
                            <p className="font-mono text-xs break-all">{atom.command}</p>
                          </div>
                        </div>
                        {atom.spec && Object.keys(atom.spec).length > 0 && (
                          <div>
                            <p className="text-xs text-muted-foreground mb-1">Spec</p>
                            <pre className="bg-caesium-void text-green-400 rounded p-3 text-xs overflow-auto max-h-40">
                              {JSON.stringify(atom.spec, null, 2)}
                            </pre>
                          </div>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                )}
              </>
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
