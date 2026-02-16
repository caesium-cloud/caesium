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

export function TriggersPage() {
  const { data: triggers, isLoading, error } = useQuery({
    queryKey: ["triggers"],
    queryFn: api.getTriggers,
  });

  if (isLoading) return <div className="p-8 text-center text-muted-foreground">Loading triggers...</div>;
  if (error) return <div className="p-8 text-center text-destructive">Error loading triggers: {error.message}</div>;

  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-bold tracking-tight">Triggers</h1>
      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Configuration</TableHead>
              <TableHead>Created At</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {triggers?.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="h-24 text-center">
                  No triggers found.
                </TableCell>
              </TableRow>
            )}
            {triggers?.map((trigger) => (
              <TableRow key={trigger.id}>
                <TableCell className="font-medium">{trigger.alias}</TableCell>
                <TableCell>
                  <Badge variant="secondary">{trigger.type}</Badge>
                </TableCell>
                <TableCell className="font-mono text-xs">{trigger.configuration}</TableCell>
                <TableCell className="text-muted-foreground text-sm">
                  {new Date(trigger.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}
