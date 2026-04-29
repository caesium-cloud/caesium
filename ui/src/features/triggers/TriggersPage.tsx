import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, type Trigger, type TriggerCreateRequest, type TriggerUpdateRequest } from "@/lib/api";
import { Card } from "@/components/ui/card";
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
import { Clock, Globe, Plus, Pencil, Copy, Check, ChevronDown, ChevronRight, Zap } from "lucide-react";
import { useMemo, useState, useEffect } from "react";
import { cn } from "@/lib/utils";
import cronParser from "cron-parser";

const inputClass =
  "w-full rounded-md border border-graphite/50 bg-midnight/50 px-3 py-2 text-sm text-text-1 ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-cyan-glow";
const labelClass = "mb-1 block text-[10px] font-bold uppercase tracking-widest text-text-3";
const textareaClass = `${inputClass} min-h-[112px] font-mono text-xs`;

type HTTPTriggerFormState = {
  alias: string;
  path: string;
  secret: string;
  signatureScheme: string;
  signatureHeader: string;
  paramMappingText: string;
  defaultParamsText: string;
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
    // ignore malformed config
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

function NextFire({ expression, timezone }: { expression: string; timezone?: string }) {
  const [nextDate, setNextDate] = useState<Date | null>(null);

  useEffect(() => {
    const compute = () => {
      try {
        const interval = cronParser.parse(expression, timezone ? { tz: timezone } : undefined);
        setNextDate(interval.next().toDate());
      } catch {
        setNextDate(null);
      }
    };
    
    compute();
    const timer = setInterval(compute, 60000);
    return () => clearInterval(timer);
  }, [expression, timezone]);

  if (!nextDate) return <span className="text-[10px] text-text-4 font-mono">Invalid cron</span>;

  return (
    <span className="text-[10px] font-mono text-text-2 bg-midnight/30 px-2 py-1 rounded border border-graphite/20">
      Next: <RelativeTime date={nextDate.toISOString()} />
    </span>
  );
}

function CopyWebhookUrl({ path, externalUrl }: { path: string; externalUrl?: string }) {
  const [copied, setCopied] = useState(false);
  const baseUrl = externalUrl || window.location.origin;
  const fullUrl = `${baseUrl}${path}`;
  const isFallback = !externalUrl;

  const handleCopy = async (e: React.MouseEvent) => {
    e.stopPropagation();
    await navigator.clipboard.writeText(fullUrl);
    setCopied(true);
    toast.success("Webhook URL copied");
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div 
      className="flex items-center gap-2 bg-midnight/40 border border-graphite/30 rounded-md px-2 py-1 max-w-sm relative group"
      onClick={(e) => e.stopPropagation()}
    >
      {isFallback && (
        <div className="absolute -top-8 left-0 hidden group-hover:block bg-obsidian border border-graphite/50 text-text-2 text-[10px] px-2 py-1 rounded shadow-lg whitespace-nowrap z-50">
          CAESIUM_API_EXTERNAL_URL is unset. Falling back to browser origin.
        </div>
      )}
      <code className="text-[10px] text-text-3 font-mono truncate flex-1">{fullUrl}</code>
      <Button 
        variant="ghost" 
        size="icon" 
        className="h-5 w-5 text-text-4 hover:text-cyan-glow hover:bg-transparent" 
        onClick={handleCopy}
      >
        {copied ? <Check className="h-3 w-3 text-success" /> : <Copy className="h-3 w-3" />}
      </Button>
    </div>
  );
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

  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
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
        <Skeleton className="h-8 w-48 bg-graphite/20" />
        <div className="grid gap-4">
          {[1, 2, 3].map((i) => <Skeleton key={i} className="h-20 w-full bg-graphite/10" />)}
        </div>
      </div>
    );
  }
  
  if (error) {
    return (
      <div className="flex flex-col items-center justify-center p-12 text-center">
        <div className="text-destructive mb-2 font-bold">Error loading triggers</div>
        <div className="text-text-3 text-sm">{error.message}</div>
      </div>
    );
  }

  return (
    <>
      <div className="space-y-6">
        <div className="flex items-center justify-between gap-4">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">Triggers</h1>
            <p className="text-sm text-text-3 mt-1">Cron schedules and HTTP webhooks</p>
          </div>
          <div className="flex items-center gap-4">
            <span className="text-[10px] font-bold uppercase tracking-widest text-text-4 hidden sm:inline-block">
              {filtered.length} trigger{filtered.length !== 1 ? "s" : ""}
            </span>
            <Button size="sm" onClick={openCreateDialog} className="bg-cyan-glow text-midnight hover:bg-cyan-dim">
              <Plus className="mr-1.5 h-3.5 w-3.5" />
              New HTTP Trigger
            </Button>
          </div>
        </div>

        {triggerTypes.length > 0 && (
          <div className="flex gap-2">
            {triggerTypes.map((type) => {
              const isActive = typeFilter === type;
              return (
                <button
                  key={type}
                  onClick={() => setTypeFilter(isActive ? null : type)}
                  className={cn(
                    "rounded-full px-3 py-1.5 text-xs border transition-colors flex items-center gap-1.5 font-medium",
                    isActive
                      ? "bg-cyan-glow/10 text-cyan-glow border-cyan-glow/30"
                      : "bg-midnight/50 text-text-3 border-graphite/50 hover:border-text-3 hover:text-text-2",
                  )}
                >
                  {type === "cron" ? <Clock className="h-3 w-3" /> : <Globe className="h-3 w-3" />}
                  <span className="capitalize">{type}</span>
                </button>
              );
            })}
          </div>
        )}

        {filtered.length === 0 && (
          <div className="rounded-md border border-graphite/30 bg-midnight/30 h-32 flex flex-col items-center justify-center text-text-4 text-sm">
            <Globe className="h-6 w-6 mb-2 opacity-20" />
            No triggers found
          </div>
        )}

        <div className="grid gap-3">
          {filtered.map((trigger) => {
            const config = parseTriggerConfiguration(trigger);
            const isHttp = trigger.type === "http";
            const webhookPath = webhookRoute(config.path);
            const isExpanded = expanded === trigger.id;
            const expr = (config.expression || config.cron || trigger.configuration) as string;

            return (
              <Card 
                key={trigger.id} 
                className={cn(
                  "overflow-hidden transition-colors border-graphite/30",
                  isExpanded ? "bg-midnight/60 border-graphite/50" : "bg-midnight/30 hover:bg-midnight/50 hover:border-graphite/50 cursor-pointer"
                )}
                onClick={() => !isExpanded && setExpanded(trigger.id)}
              >
                <div className="flex items-center justify-between px-5 py-4">
                  <div className="flex items-center gap-4 min-w-0 flex-1">
                    <div className="shrink-0 flex items-center justify-center w-10">
                      {trigger.type === "cron" ? (
                        <div className="h-8 w-8 rounded-full bg-cyan-glow/10 flex items-center justify-center text-cyan-glow border border-cyan-glow/20">
                          <Clock className="h-4 w-4" />
                        </div>
                      ) : (
                        <div className="h-8 w-8 rounded-full bg-gold/10 flex items-center justify-center text-gold border border-gold/20">
                          <Globe className="h-4 w-4" />
                        </div>
                      )}
                    </div>
                    <div className="min-w-0 flex-1 grid grid-cols-1 md:grid-cols-[2fr_3fr_1fr] items-center gap-4">
                      <div className="truncate">
                        <div className="font-semibold text-text-1 text-sm truncate">{trigger.alias}</div>
                        <div className="text-[10px] text-text-4 font-mono truncate mt-0.5">ID: {trigger.id.substring(0, 8)}</div>
                      </div>
                      
                      <div className="hidden md:flex items-center">
                        {isHttp && webhookPath ? (
                          <CopyWebhookUrl path={webhookPath} externalUrl={features?.external_url} />
                        ) : trigger.type === "cron" ? (
                          <div className="flex flex-col">
                            <code className="text-xs text-text-2 font-mono">{expr}</code>
                            <div className="mt-1">
                              <NextFire expression={expr} timezone={config.timezone as string | undefined} />
                            </div>
                          </div>
                        ) : (
                          <span className="text-xs text-text-4 font-mono truncate max-w-[200px]">
                            {String(trigger.configuration ?? "").slice(0, 40)}
                          </span>
                        )}
                      </div>

                      <div className="hidden md:flex justify-end">
                        
                      </div>
                    </div>
                  </div>
                  
                  <div className="flex items-center gap-3 shrink-0 ml-4">
                    {isHttp && (
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-7 text-xs bg-transparent border-graphite/50 text-text-3 hover:text-text-1"
                        onClick={(e) => {
                          e.stopPropagation();
                          openEditDialog(trigger);
                        }}
                        disabled={editorPending}
                      >
                        <Pencil className="h-3 w-3 mr-1.5" />
                        Edit
                      </Button>
                    )}
                    <Button
                      size="icon"
                      variant="ghost"
                      className="h-7 w-7 text-text-4 hover:text-text-2"
                      onClick={(e) => {
                        e.stopPropagation();
                        setExpanded(isExpanded ? null : trigger.id);
                      }}
                    >
                      {isExpanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
                    </Button>
                  </div>
                </div>

                {isExpanded && (
                  <div className="border-t border-graphite/20 bg-black/20 px-5 py-4 space-y-4">
                    <div className="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
                      <div className="col-span-2 md:col-span-1">
                        <p className="text-[10px] uppercase tracking-widest font-bold text-text-4 mb-1">Full ID</p>
                        <p className="font-mono text-xs text-text-2 break-all">{trigger.id}</p>
                      </div>
                      <div>
                        <p className="text-[10px] uppercase tracking-widest font-bold text-text-4 mb-1">Created</p>
                        <p className="text-xs text-text-2"><RelativeTime date={trigger.created_at} /></p>
                      </div>
                      <div>
                        <p className="text-[10px] uppercase tracking-widest font-bold text-text-4 mb-1">Updated</p>
                        <p className="text-xs text-text-2"><RelativeTime date={trigger.updated_at} /></p>
                      </div>
                    </div>
                    
                    <div className="md:hidden">
                      <p className="text-[10px] uppercase tracking-widest font-bold text-text-4 mb-1">Action</p>
                      {isHttp && webhookPath ? (
                          <div className="mt-1 max-w-[300px]">
                            <CopyWebhookUrl path={webhookPath} externalUrl={features?.external_url} />
                          </div>
                        ) : trigger.type === "cron" ? (
                          <div className="flex flex-col mt-1">
                            <code className="text-xs text-text-2 font-mono bg-midnight/40 px-2 py-1 rounded inline-block w-max">{expr}</code>
                            <div className="mt-2">
                              <NextFire expression={expr} timezone={config.timezone as string | undefined} />
                            </div>
                          </div>
                        ) : null}
                    </div>

                    <div>
                      <p className="text-[10px] uppercase tracking-widest font-bold text-text-4 mb-1">Raw Configuration</p>
                      <pre className="bg-void border border-graphite/30 text-text-2 rounded-md p-3 text-[11px] overflow-auto max-h-48 font-mono">
                        {Object.keys(config).length > 0
                          ? JSON.stringify(config, null, 2)
                          : trigger.configuration}
                      </pre>
                    </div>

                    {isHttp && (
                      <div className="rounded-md border border-gold/20 bg-gold/5 px-3 py-2 text-xs text-text-3 flex items-start gap-2">
                        <Zap className="h-3.5 w-3.5 text-gold shrink-0 mt-0.5" />
                        <div>
                          Manual fire is an operator-only API action via <code className="font-mono text-[10px] text-text-2 bg-midnight/50 px-1 py-0.5 rounded border border-graphite/30 mx-1">POST /v1/triggers/:id/fire</code>.
                          External systems should POST to the webhook URL instead.
                        </div>
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
        <DialogContent className="max-w-2xl bg-midnight border-graphite/50 text-text-1">
          <DialogHeader>
            <DialogTitle className="text-xl font-bold">{editorMode === "create" ? "New HTTP Trigger" : "Edit HTTP Trigger"}</DialogTitle>
            <DialogDescription className="text-text-3">
              Configure the webhook route, auth, and request-body mappings. Standalone triggers only run jobs that already reference them.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleEditorSubmit} className="space-y-5 mt-2">
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
                  className={cn(inputClass, "appearance-none")}
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
                  placeholder="{&#34;ref&#34;: &#34;$.ref&#34;}"
                />
              </div>
              <div>
                <label className={labelClass}>Default Params JSON</label>
                <textarea
                  value={formState.defaultParamsText}
                  onChange={(event) => setFormState((current) => ({ ...current, defaultParamsText: event.target.value }))}
                  className={textareaClass}
                  disabled={editorPending}
                  placeholder="{&#34;env&#34;: &#34;production&#34;}"
                />
              </div>
            </div>

            <div className="rounded-md border border-graphite/30 bg-midnight/40 px-3 py-2.5 text-xs text-text-3">
              Param mappings use simple JSONPath expressions like <code className="mx-1 rounded bg-black/40 border border-graphite/40 px-1 py-0.5 text-[10px] text-text-2 font-mono">$.ref</code> and
              <code className="mx-1 rounded bg-black/40 border border-graphite/40 px-1 py-0.5 text-[10px] text-text-2 font-mono">$</code> for the whole payload.
            </div>

            {formError && <p className="text-sm text-danger font-medium">{formError}</p>}

            <DialogFooter className="pt-2">
              <Button 
                type="button" 
                variant="outline" 
                onClick={() => setEditorOpen(false)} 
                disabled={editorPending}
                className="bg-transparent border-graphite/50 text-text-2 hover:bg-graphite/20 hover:text-text-1"
              >
                Cancel
              </Button>
              <Button 
                type="submit" 
                disabled={editorPending}
                className="bg-cyan-glow text-midnight hover:bg-cyan-dim"
              >
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
