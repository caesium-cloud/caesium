import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, type Trigger, type TriggerCreateRequest, type TriggerUpdateRequest } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { RelativeTime } from "@/components/relative-time";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { toast } from "sonner";
import { Zap, Clock, Globe, ChevronDown, ChevronRight, Plus, Pencil } from "lucide-react";
import { useMemo, useState } from "react";
import { cn } from "@/lib/utils";

const inputClass =
  "w-full rounded-md border bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";
const labelClass = "mb-1 block text-xs font-medium uppercase tracking-wide text-muted-foreground";
const textareaClass = `${inputClass} min-h-[112px] font-mono text-xs`;

type HTTPTriggerFormState = {
  alias: string;
  path: string;
  secret: string;
  signatureScheme: string;
  signatureHeader: string;
  paramMappingText: string;
  defaultParamsText: string;
  /** Config keys not managed by the form, preserved on round-trip. */
  extraConfig: Record<string, unknown>;
};

function normalizeWebhookPath(path: unknown) {
  if (typeof path !== "string") return "";
  let normalized = path.trim();
  normalized = normalized.replace(/^\/+/, "");
  normalized = normalized.replace(/^v1\/+/, "");
  normalized = normalized.replace(/^hooks\/+/, "");
  return normalized.replace(/\/+$/, "");
}

function webhookRoute(path: unknown) {
  const normalized = normalizeWebhookPath(path);
  return normalized ? `/v1/hooks/${normalized}` : "";
}

function parseTriggerConfiguration(trigger: Trigger) {
  try {
    const parsed = JSON.parse(trigger.configuration);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
  } catch {
    // ignore malformed config in UI summary
  }
  return {};
}

function stringifyStringMap(value: unknown) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return "{}";
  return JSON.stringify(value, null, 2);
}

const managedConfigKeys = new Set([
  "path", "secret", "signatureScheme", "signatureHeader",
  "paramMapping", "defaultParams",
]);

function formStateFromTrigger(trigger?: Trigger | null): HTTPTriggerFormState {
  const config = trigger ? parseTriggerConfiguration(trigger) : {};
  const extraConfig: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(config)) {
    if (!managedConfigKeys.has(key)) {
      extraConfig[key] = value;
    }
  }
  return {
    alias: trigger?.alias ?? "",
    path: typeof config.path === "string" ? webhookRoute(config.path) : "",
    secret: typeof config.secret === "string" ? config.secret : "",
    signatureScheme: typeof config.signatureScheme === "string" ? config.signatureScheme : "",
    signatureHeader: typeof config.signatureHeader === "string" ? config.signatureHeader : "",
    paramMappingText: stringifyStringMap(config.paramMapping),
    defaultParamsText: stringifyStringMap(config.defaultParams),
    extraConfig,
  };
}

function parseStringMap(text: string, field: string) {
  const trimmed = text.trim();
  if (!trimmed) return {};

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    throw new Error(`${field} must be valid JSON`);
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${field} must be a JSON object`);
  }

  const result: Record<string, string> = {};
  for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
    if (typeof value !== "string") {
      throw new Error(`${field}.${key} must be a string`);
    }
    result[key] = value;
  }
  return result;
}

function buildHTTPTriggerPayload(state: HTTPTriggerFormState): Pick<TriggerCreateRequest, "alias" | "configuration"> {
  const normalizedPath = normalizeWebhookPath(state.path);
  if (!state.alias.trim()) throw new Error("Alias is required");
  if (!normalizedPath) throw new Error("Webhook path is required");

  const configuration: Record<string, unknown> = {
    ...state.extraConfig,
    path: `/hooks/${normalizedPath}`,
  };

  if (state.secret.trim()) configuration.secret = state.secret.trim();
  if (state.signatureScheme.trim()) configuration.signatureScheme = state.signatureScheme.trim();
  if (state.signatureHeader.trim()) configuration.signatureHeader = state.signatureHeader.trim();

  const paramMapping = parseStringMap(state.paramMappingText, "paramMapping");
  if (Object.keys(paramMapping).length > 0) configuration.paramMapping = paramMapping;

  const defaultParams = parseStringMap(state.defaultParamsText, "defaultParams");
  if (Object.keys(defaultParams).length > 0) configuration.defaultParams = defaultParams;

  return {
    alias: state.alias.trim(),
    configuration,
  };
}

function errorMessage(error: unknown) {
  if (error instanceof ApiError) return error.message;
  if (error instanceof Error) return error.message;
  return "Request failed";
}

function CronPreview({ expression }: { expression: string }) {
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
  const config = parseTriggerConfiguration(trigger);

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
    const route = webhookRoute(config.path);
    return (
      <div className="flex items-center gap-2">
        <Globe className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <span className="text-xs text-muted-foreground font-mono">
          {route || "HTTP webhook"}
        </span>
      </div>
    );
  }
  return <span className="text-xs text-muted-foreground font-mono">{String(trigger.configuration ?? "").slice(0, 60)}</span>;
}

export function TriggersPage() {
  const queryClient = useQueryClient();
  const [expanded, setExpanded] = useState<string | null>(null);
  const [typeFilter, setTypeFilter] = useState<string | null>(null);
  const [editorOpen, setEditorOpen] = useState(false);
  const [editorMode, setEditorMode] = useState<"create" | "edit">("create");
  const [editingTrigger, setEditingTrigger] = useState<Trigger | null>(null);
  const [formState, setFormState] = useState<HTTPTriggerFormState>(formStateFromTrigger());
  const [formError, setFormError] = useState<string | null>(null);

  const { data: triggers, isLoading, error } = useQuery({
    queryKey: ["triggers"],
    queryFn: api.getTriggers,
    refetchInterval: 30000,
  });

  const createMutation = useMutation({
    mutationFn: (body: TriggerCreateRequest) => api.createTrigger(body),
    onSuccess: () => {
      toast.success("Trigger created");
      queryClient.invalidateQueries({ queryKey: ["triggers"] });
      setEditorOpen(false);
    },
    onError: (err) => setFormError(errorMessage(err)),
  });

  const updateMutation = useMutation({
    mutationFn: ({ id, body }: { id: string; body: TriggerUpdateRequest }) => api.updateTrigger(id, body),
    onSuccess: () => {
      toast.success("Trigger updated");
      queryClient.invalidateQueries({ queryKey: ["triggers"] });
      setEditorOpen(false);
    },
    onError: (err) => setFormError(errorMessage(err)),
  });

  const triggerTypes = useMemo(() => {
    const set = new Set<string>();
    triggers?.forEach((trigger) => set.add(trigger.type));
    return Array.from(set);
  }, [triggers]);

  const filtered = useMemo(() => {
    if (!typeFilter) return triggers || [];
    return (triggers || []).filter((trigger) => trigger.type === typeFilter);
  }, [triggers, typeFilter]);

  const editorPending = createMutation.isPending || updateMutation.isPending;

  function openCreateDialog() {
    setEditorMode("create");
    setEditingTrigger(null);
    setFormState(formStateFromTrigger());
    setFormError(null);
    setEditorOpen(true);
  }

  function openEditDialog(trigger: Trigger) {
    setEditorMode("edit");
    setEditingTrigger(trigger);
    setFormState(formStateFromTrigger(trigger));
    setFormError(null);
    setEditorOpen(true);
  }

  function handleEditorSubmit(event: React.FormEvent) {
    event.preventDefault();
    setFormError(null);

    let payload: Pick<TriggerCreateRequest, "alias" | "configuration">;
    try {
      payload = buildHTTPTriggerPayload(formState);
    } catch (err) {
      setFormError(errorMessage(err));
      return;
    }

    if (editorMode === "create") {
      createMutation.mutate({
        alias: payload.alias,
        type: "http",
        configuration: payload.configuration,
      });
      return;
    }

    if (!editingTrigger) {
      setFormError("No trigger selected for editing");
      return;
    }

    updateMutation.mutate({
      id: editingTrigger.id,
      body: payload,
    });
  }

  if (isLoading) {
    return (
      <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-48" />
        <div className="grid gap-4">
          {[1, 2, 3].map((i) => <Skeleton key={i} className="h-24 w-full" />)}
        </div>
      </div>
    );
  }
  if (error) return <div className="p-8 text-center text-destructive">Error loading triggers: {error.message}</div>;

  return (
    <>
      <div className="space-y-4">
        <div className="flex items-center justify-between gap-4">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">Triggers</h1>
            <p className="text-sm text-muted-foreground mt-0.5">Cron schedules and HTTP webhooks</p>
          </div>
          <div className="flex items-center gap-3">
            <span className="text-sm text-muted-foreground">{filtered.length} trigger{filtered.length !== 1 ? "s" : ""}</span>
            <Button size="sm" onClick={openCreateDialog}>
              <Plus className="mr-1.5 h-3.5 w-3.5" />
              New HTTP Trigger
            </Button>
          </div>
        </div>

        <div className="flex gap-2">
          {triggerTypes.map((type) => (
            <button
              key={type}
              onClick={() => setTypeFilter(typeFilter === type ? null : type)}
              className={cn(
                "rounded-full px-3 py-1 text-xs border transition-colors flex items-center gap-1.5",
                typeFilter === type
                  ? "bg-primary text-primary-foreground border-primary"
                  : "bg-background text-muted-foreground border-border hover:border-primary hover:text-primary",
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
          {filtered.map((trigger) => {
            const config = parseTriggerConfiguration(trigger);
            const isHttp = trigger.type === "http";
            const webhookPath = webhookRoute(config.path);
            const signatureScheme = typeof config.signatureScheme === "string" ? config.signatureScheme : "";

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
                          {trigger.type === "cron"
                            ? <Clock className="h-2.5 w-2.5 mr-1" />
                            : <Globe className="h-2.5 w-2.5 mr-1" />}
                          {trigger.type}
                        </Badge>
                      </div>
                      <div className="mt-0.5">
                        <TriggerConfig trigger={trigger} />
                      </div>
                    </div>
                  </div>
                  <div className="flex items-center gap-2 shrink-0 ml-4" onClick={(e) => e.stopPropagation()}>
                    <span className="text-xs text-muted-foreground hidden sm:block">
                      <RelativeTime date={trigger.updated_at} />
                    </span>
                    {isHttp && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => openEditDialog(trigger)}
                        disabled={editorPending}
                      >
                        <Pencil className="h-3.5 w-3.5 mr-1.5" />
                        Edit
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
                      {isHttp && webhookPath && (
                        <div>
                          <p className="text-xs text-muted-foreground mb-1">Webhook</p>
                          <p className="font-mono text-xs">{webhookPath}</p>
                        </div>
                      )}
                      {isHttp && signatureScheme && (
                        <div>
                          <p className="text-xs text-muted-foreground mb-1">Auth</p>
                          <p className="text-xs">{signatureScheme}</p>
                        </div>
                      )}
                    </div>
                    <div>
                      <p className="text-xs text-muted-foreground mb-1">Configuration</p>
                      <pre className="bg-code-bg text-code-fg rounded p-3 text-xs overflow-auto max-h-48">
                        {Object.keys(config).length > 0
                          ? JSON.stringify(config, null, 2)
                          : trigger.configuration}
                      </pre>
                    </div>
                    {isHttp && (
                      <div className="rounded-md border border-yellow-500/30 bg-yellow-500/5 px-3 py-2 text-xs text-muted-foreground">
                        <Zap className="h-3 w-3 inline mr-1.5 text-yellow-500" />
                        Manual fire is now an operator-only API action via <code className="mx-1 rounded bg-muted px-1 py-0.5">POST /v1/triggers/:id/fire</code>.
                        External systems should post to the webhook route above.
                      </div>
                    )}
                  </div>
                )}
              </Card>
            );
          })}
        </div>
      </div>

      <Dialog open={editorOpen} onOpenChange={(open) => !editorPending && setEditorOpen(open)}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editorMode === "create" ? "New HTTP Trigger" : "Edit HTTP Trigger"}</DialogTitle>
            <DialogDescription>
              Configure the webhook route, auth, and request-body mappings. Standalone triggers only run jobs that already reference them.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleEditorSubmit} className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <label className={labelClass}>Alias</label>
                <input
                  value={formState.alias}
                  onChange={(event) => setFormState((current) => ({ ...current, alias: event.target.value }))}
                  className={inputClass}
                  disabled={editorPending}
                  required
                />
              </div>
              <div>
                <label className={labelClass}>Webhook Path</label>
                <input
                  value={formState.path}
                  onChange={(event) => setFormState((current) => ({ ...current, path: event.target.value }))}
                  placeholder="/v1/hooks/team/deploy"
                  className={inputClass}
                  disabled={editorPending}
                  required
                />
              </div>
            </div>

            <div className="grid gap-4 sm:grid-cols-3">
              <div>
                <label className={labelClass}>Secret</label>
                <input
                  value={formState.secret}
                  onChange={(event) => setFormState((current) => ({ ...current, secret: event.target.value }))}
                  placeholder="shared-secret or secret://..."
                  className={inputClass}
                  disabled={editorPending}
                />
              </div>
              <div>
                <label className={labelClass}>Auth Scheme</label>
                <select
                  value={formState.signatureScheme}
                  onChange={(event) => setFormState((current) => ({ ...current, signatureScheme: event.target.value }))}
                  className={inputClass}
                  disabled={editorPending}
                >
                  <option value="">Default</option>
                  <option value="hmac-sha256">hmac-sha256</option>
                  <option value="hmac-sha1">hmac-sha1</option>
                  <option value="bearer">bearer</option>
                  <option value="basic">basic</option>
                </select>
              </div>
              <div>
                <label className={labelClass}>Signature Header</label>
                <input
                  value={formState.signatureHeader}
                  onChange={(event) => setFormState((current) => ({ ...current, signatureHeader: event.target.value }))}
                  placeholder="X-Hub-Signature-256"
                  className={inputClass}
                  disabled={editorPending}
                />
              </div>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <label className={labelClass}>Param Mapping JSON</label>
                <textarea
                  value={formState.paramMappingText}
                  onChange={(event) => setFormState((current) => ({ ...current, paramMappingText: event.target.value }))}
                  className={textareaClass}
                  disabled={editorPending}
                />
              </div>
              <div>
                <label className={labelClass}>Default Params JSON</label>
                <textarea
                  value={formState.defaultParamsText}
                  onChange={(event) => setFormState((current) => ({ ...current, defaultParamsText: event.target.value }))}
                  className={textareaClass}
                  disabled={editorPending}
                />
              </div>
            </div>

            <div className="rounded-md border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
              Param mappings use simple JSONPath expressions like <code className="mx-1 rounded bg-muted px-1 py-0.5">$.ref</code> and
              <code className="mx-1 rounded bg-muted px-1 py-0.5">$</code> for the whole payload.
            </div>

            {formError && <p className="text-sm text-destructive">{formError}</p>}

            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setEditorOpen(false)} disabled={editorPending}>
                Cancel
              </Button>
              <Button type="submit" disabled={editorPending}>
                {editorPending
                  ? (editorMode === "create" ? "Creating..." : "Saving...")
                  : (editorMode === "create" ? "Create Trigger" : "Save Trigger")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  );
}
