import { useQuery, useMutation } from "@tanstack/react-query";
import { api, type Trigger } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { RelativeTime } from "@/components/relative-time";
import { toast } from "sonner";
import { Zap, Clock, Globe, ChevronDown, ChevronRight, Play } from "lucide-react";
import { useState, useMemo } from "react";
import { cn } from "@/lib/utils";

function CronPreview({ expression }: { expression: string }) {
  // Show next N cron fire times using a simple description
  const describe = (expr: string) => {
    const parts = expr.trim().split(/\s+/);
    if (parts.length < 5) return expr;
    const [min, hour, dom, month, dow] = parts;
    if (min === "0" && hour === "*" && dom === "*" && month === "*" && dow === "*") return "Every hour";
    if (min === "0" && hour === "0" && dom === "*" && month === "*" && dow === "*") return "Daily at midnight";
    if (min === "0" && hour === "0" && dom === "*" && month === "*" && dow === "1") return "Every Monday at midnight";
    if (dom === "*" && month === "*" && dow === "*") {
      if (hour === "*") return `Every minute :${min.padStart(2, "0")}`;
      return `Daily at ${hour.padStart(2, "0")}:${min.padStart(2, "0")}`;
    }
    return expr;
  };
  return (
    <span className="text-xs text-muted-foreground font-mono" title={expression}>
      {describe(expression)}
    </span>
  );
}

function TriggerConfig({ trigger }: { trigger: Trigger }) {
  let config: Record<string, unknown> = {};
  try { config = JSON.parse(trigger.configuration); } catch { /* raw */ }

  if (trigger.type === "cron") {
    const expr = (config.expression || config.cron || trigger.configuration) as string;
    return (
      <div className="flex items-center gap-2">
        <Clock className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <CronPreview expression={expr} />
      </div>
    );
  }
  if (trigger.type === "http") {
    return (
      <div className="flex items-center gap-2">
        <Globe className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <span className="text-xs text-muted-foreground font-mono">HTTP webhook</span>
      </div>
    );
  }
  return <span className="text-xs text-muted-foreground font-mono">{trigger.configuration.substring(0, 60)}</span>;
}

export function TriggersPage() {
  const [expanded, setExpanded] = useState<string | null>(null);
  const [typeFilter, setTypeFilter] = useState<string | null>(null);

  const { data: triggers, isLoading, error } = useQuery({
    queryKey: ["triggers"],
    queryFn: api.getTriggers,
    refetchInterval: 30000,
  });

  const fireMutation = useMutation({
    mutationFn: (id: string) => api.fireTrigger(id),
    onSuccess: () => toast.success("Trigger fired"),
    onError: () => toast.error("Failed to fire trigger"),
  });

  const triggerTypes = useMemo(() => {
    const set = new Set<string>();
    triggers?.forEach(t => set.add(t.type));
    return Array.from(set);
  }, [triggers]);

  const filtered = useMemo(() => {
    if (!typeFilter) return triggers || [];
    return (triggers || []).filter(t => t.type === typeFilter);
  }, [triggers, typeFilter]);

  if (isLoading) return (
    <div className="p-8 space-y-4">
      <Skeleton className="h-8 w-48" />
      <div className="grid gap-4">
        {[1, 2, 3].map(i => <Skeleton key={i} className="h-24 w-full" />)}
      </div>
    </div>
  );
  if (error) return <div className="p-8 text-center text-destructive">Error loading triggers: {error.message}</div>;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Triggers</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Cron schedules and HTTP webhooks</p>
        </div>
        <span className="text-sm text-muted-foreground">{filtered.length} trigger{filtered.length !== 1 ? "s" : ""}</span>
      </div>

      {/* Type filter */}
      <div className="flex gap-2">
        {triggerTypes.map(type => (
          <button
            key={type}
            onClick={() => setTypeFilter(typeFilter === type ? null : type)}
            className={cn(
              "rounded-full px-3 py-1 text-xs border transition-colors flex items-center gap-1.5",
              typeFilter === type
                ? "bg-primary text-primary-foreground border-primary"
                : "bg-background text-muted-foreground border-border hover:border-primary hover:text-primary"
            )}
          >
            {type === "cron" ? <Clock className="h-3 w-3" /> : <Globe className="h-3 w-3" />}
            {type}
          </button>
        ))}
      </div>

      {filtered.length === 0 && (
        <div className="rounded-md border bg-card h-24 flex items-center justify-center text-muted-foreground text-sm">
          No triggers found.
        </div>
      )}

      <div className="grid gap-3">
        {filtered.map(trigger => {
          let config: Record<string, unknown> = {};
          try { config = JSON.parse(trigger.configuration); } catch { /* ok */ }
          const isHttp = trigger.type === "http";

          return (
            <Card key={trigger.id} className="overflow-hidden">
              <div
                className="flex items-center justify-between px-4 py-3 cursor-pointer hover:bg-muted/30 transition-colors"
                onClick={() => setExpanded(expanded === trigger.id ? null : trigger.id)}
              >
                <div className="flex items-center gap-3 min-w-0">
                  <div className="shrink-0">
                    {expanded === trigger.id
                      ? <ChevronDown className="h-4 w-4 text-muted-foreground" />
                      : <ChevronRight className="h-4 w-4 text-muted-foreground" />}
                  </div>
                  <div className="min-w-0">
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className="font-medium text-sm">{trigger.alias}</span>
                      <Badge variant={trigger.type === "cron" ? "secondary" : "outline"} className="text-[10px]">
                        {trigger.type === "cron" ? <Clock className="h-2.5 w-2.5 mr-1" /> : <Globe className="h-2.5 w-2.5 mr-1" />}
                        {trigger.type}
                      </Badge>
                    </div>
                    <div className="mt-0.5">
                      <TriggerConfig trigger={trigger} />
                    </div>
                  </div>
                </div>
                <div className="flex items-center gap-3 shrink-0 ml-4" onClick={e => e.stopPropagation()}>
                  <span className="text-xs text-muted-foreground hidden sm:block">
                    <RelativeTime date={trigger.updated_at} />
                  </span>
                  {isHttp && (
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => fireMutation.mutate(trigger.id)}
                      disabled={fireMutation.isPending}
                      title="Fire this HTTP trigger"
                    >
                      <Play className="h-3.5 w-3.5 mr-1.5" />
                      Fire
                    </Button>
                  )}
                </div>
              </div>

              {expanded === trigger.id && (
                <div className="border-t bg-muted/20 px-4 py-3 space-y-3">
                  <div className="grid grid-cols-2 sm:grid-cols-3 gap-3 text-sm">
                    <div>
                      <p className="text-xs text-muted-foreground mb-1">ID</p>
                      <p className="font-mono text-xs">{trigger.id}</p>
                    </div>
                    <div>
                      <p className="text-xs text-muted-foreground mb-1">Created</p>
                      <p className="text-xs"><RelativeTime date={trigger.created_at} /></p>
                    </div>
                    <div>
                      <p className="text-xs text-muted-foreground mb-1">Updated</p>
                      <p className="text-xs"><RelativeTime date={trigger.updated_at} /></p>
                    </div>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground mb-1">Configuration</p>
                    <pre className="bg-caesium-void text-green-400 rounded p-3 text-xs overflow-auto max-h-48">
                      {Object.keys(config).length > 0
                        ? JSON.stringify(config, null, 2)
                        : trigger.configuration}
                    </pre>
                  </div>
                  {isHttp && (
                    <div className="rounded-md border border-yellow-500/30 bg-yellow-500/5 px-3 py-2 text-xs text-muted-foreground">
                      <Zap className="h-3 w-3 inline mr-1.5 text-yellow-500" />
                      Firing this trigger will immediately start all jobs associated with it.
                    </div>
                  )}
                </div>
              )}
            </Card>
          );
        })}
      </div>
    </div>
  );
}
