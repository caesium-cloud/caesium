import { useParams } from "@tanstack/react-router";
import { useQuery, useMutation } from "@tanstack/react-query";
import { api, type Atom } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { JobDAG } from "./JobDAG";
import { Play } from "lucide-react";
import { toast } from "sonner";

export function JobDetailPage() {
  const { jobId } = useParams({ strict: false }) as { jobId: string };

  const { data: job, isLoading: isLoadingJob } = useQuery({
    queryKey: ["job", jobId],
    queryFn: () => api.getJob(jobId),
  });

  const { data: dag, isLoading: isLoadingDAG } = useQuery({
    queryKey: ["job", jobId, "dag"],
    queryFn: () => api.getJobDAG(jobId),
  });

  const { data: atoms, isLoading: isLoadingAtoms } = useQuery({
    queryKey: ["atoms"],
    queryFn: api.getAtoms,
    select: (data) => {
        const map: Record<string, Atom> = {};
        data.forEach(a => map[a.id] = a);
        return map;
    }
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

  if (isLoadingJob || isLoadingDAG || isLoadingAtoms) return <div className="p-8">Loading...</div>;

  if (!job) return <div className="p-8">Job not found</div>;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
            <h1 className="text-2xl font-bold tracking-tight">{job.alias}</h1>
            <p className="text-muted-foreground font-mono text-sm">{job.id}</p>
        </div>
        <div className="flex gap-2">
             <Button onClick={() => triggerMutation.mutate(job.id)} disabled={triggerMutation.isPending}>
                <Play className="mr-2 h-4 w-4" /> Trigger
             </Button>
        </div>
      </div>

      <Tabs defaultValue="dag">
        <TabsList>
          <TabsTrigger value="dag">DAG</TabsTrigger>
          <TabsTrigger value="runs">Runs</TabsTrigger>
          <TabsTrigger value="definition">Definition</TabsTrigger>
        </TabsList>
        <TabsContent value="dag" className="h-[600px] mt-4">
            {dag && atoms && <JobDAG dag={dag} atoms={atoms} />}
        </TabsContent>
        <TabsContent value="runs" className="mt-4">
            <div className="p-4 border rounded-md bg-card">Runs List (Coming Soon)</div>
        </TabsContent>
        <TabsContent value="definition" className="mt-4">
            <pre className="p-4 bg-muted rounded-md overflow-auto text-xs border">
                {JSON.stringify(job, null, 2)}
            </pre>
        </TabsContent>
      </Tabs>
    </div>
  );
}
