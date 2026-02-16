import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";

export function AtomsPage() {
  const { data: atoms, isLoading, error } = useQuery({
    queryKey: ["atoms"],
    queryFn: api.getAtoms,
  });

  if (isLoading) return <div className="p-8 text-center text-muted-foreground">Loading atoms...</div>;
  if (error) return <div className="p-8 text-center text-destructive">Error loading atoms: {error.message}</div>;

  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-bold tracking-tight">Atoms</h1>
      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Image</TableHead>
              <TableHead>Engine</TableHead>
              <TableHead>Command</TableHead>
              <TableHead>ID</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {atoms?.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="h-24 text-center">
                  No atoms found.
                </TableCell>
              </TableRow>
            )}
            {atoms?.map((atom) => (
              <TableRow key={atom.id}>
                <TableCell className="font-medium">{atom.image}</TableCell>
                <TableCell>
                  <Badge variant="outline">{atom.engine}</Badge>
                </TableCell>
                <TableCell className="font-mono text-xs">{atom.command}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{atom.id}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}
