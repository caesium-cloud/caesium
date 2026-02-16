import { useQuery, useMutation } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { api } from "@/lib/api";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Play } from "lucide-react";
import { toast } from "sonner";

export function JobsPage() {
  const { data: jobs, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
  });

  const triggerMutation = useMutation({
    mutationFn: api.triggerJob,
    onSuccess: () => {
      toast.success("Job triggered successfully");
    },
    onError: (err) => {
      toast.error(`Failed to trigger job: ${err.message}`);
    },
  });

  if (isLoading) return <div className="p-8 text-center text-muted-foreground">Loading jobs...</div>;
  if (error) return <div className="p-8 text-center text-destructive">Error loading jobs: {error.message}</div>;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold tracking-tight">Jobs</h1>
        {/* <Button>Create Job</Button> */}
      </div>
      <div className="rounded-md border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>ID</TableHead>
              <TableHead>Created At</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {jobs?.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="h-24 text-center">
                  No jobs found.
                </TableCell>
              </TableRow>
            )}
            {jobs?.map((job) => (
              <TableRow key={job.id}>
                <TableCell className="font-medium">
                  <Link to="/jobs/$jobId" params={{ jobId: job.id }} className="hover:underline text-primary">
                    {job.alias}
                  </Link>
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{job.id}</TableCell>
                <TableCell className="text-muted-foreground">{new Date(job.created_at).toLocaleString()}</TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="icon"
                    onClick={() => triggerMutation.mutate(job.id)}
                    disabled={triggerMutation.isPending}
                    title="Trigger Run"
                  >
                    <Play className="h-4 w-4" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}
