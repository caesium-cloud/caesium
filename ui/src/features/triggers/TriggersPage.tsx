import { RelativeTime } from "@/components/relative-time";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, type Trigger, type TriggerCreateRequest, type TriggerUpdateRequest } from "@/lib/api";
import { IdChip } from "@/components/ui/id-chip";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useUTCTick } from "@/components/ui/utc-clock";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { toast } from "sonner";
import { Clock, Globe, Copy, Check, ChevronDown, ChevronRight } from "lucide-react";
import { useMemo, useState, useEffect } from "react";
import { cn, formatUTCTimestamp } from "@/lib/utils";
import {
  describeTrigger,
  getNextFireDate,
  normalizeWebhookPath,
  parseTriggerConfiguration,
  webhookRoute,
  type TriggerDescription,
} from "./trigger-utils";

const inputClass =
  "w-full rounded-md border border-graphite/50 bg-midnight/50 px-3 py-2 text-sm text-text-1 ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-cyan-glow";
const labelClass = "mb-1 block text-[11px] font-bold lowercase text-text-3";
const textareaClass = `${inputClass} min-h-[112px] text-xs`;

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
  const now = useUTCTick();
  const [nextDate, setNextDate] = useState<Date | null>(null);

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const compute = () => {
      const next = getNextFireDate(expression, timezone);
      setNextDate(next);
      if (!next) return;

      // Wake at the cron boundary, not an arbitrary minute tick. Long waits
      // are capped because browser timers cannot represent every future date.
      const delay = Math.max(1, next.getTime() - Date.now());
      timer = setTimeout(compute, Math.min(delay, 2_147_483_647));
    };

    compute();
    return () => {
      if (timer) clearTimeout(timer);
    };
  }, [expression, timezone]);

  if (!nextDate) return <span className="text-[11px] text-text-3">Invalid cron</span>;

  return (
    <span
      className="inline-flex items-center gap-7 text-xs text-text-2"
      data-testid="trigger-next-fire"
    >
      <span>next fires in {formatCountdown(nextDate.getTime() - now.getTime())}</span>
      <time dateTime={nextDate.toISOString()} data-testid="trigger-next-fire-timestamp">
        {formatUTCTimestamp(nextDate)}
      </time>
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
        <div className="absolute -top-8 left-0 hidden group-hover:block bg-obsidian border border-graphite/50 text-text-2 text-[11px] px-2 py-1 rounded shadow-lg whitespace-nowrap z-50">
          CAESIUM_API_EXTERNAL_URL is unset. Falling back to browser origin.
        </div>
      )}
      <code className="text-[11px] text-text-3 truncate flex-1">{fullUrl}</code>
      <Button
        variant="ghost"
        size="icon"
        className="h-5 w-5 text-text-3 hover:text-cyan-glow hover:bg-transparent"
        onClick={handleCopy}
        title={copied ? "Copied" : "Copy webhook URL"}
        aria-label={copied ? "Webhook URL copied" : "Copy webhook URL"}
      >
        {copied ? <Check className="h-3 w-3 text-success" /> : <Copy className="h-3 w-3" />}
      </Button>
    </div>
  );
}

function TriggerAction({
  description,
  externalUrl,
  mobile = false,
}: {
  description: TriggerDescription;
  externalUrl?: string;
  mobile?: boolean;
}) {
  if (description.kind === "http" && description.path) {
    return (
      <div className={cn(mobile && "mt-1 max-w-[300px]")}>
        <CopyWebhookUrl path={description.path} externalUrl={externalUrl} />
      </div>
    );
  }

  if (description.kind === "cron") {
    if (!description.expression) {
      return <span className="text-xs text-text-3">{description.summary}</span>;
    }

    return (
      <div className={cn("flex flex-col", mobile && "mt-1")}>
        <code
          className={cn(
            "text-xs text-text-2",
            mobile && "bg-midnight/40 px-2 py-1 rounded inline-block w-max",
          )}
        >
          {description.expression}
        </code>
        <div className={mobile ? "mt-2" : "mt-1"}>
          <NextFire expression={description.expression} timezone={description.timezone} />
        </div>
      </div>
    );
  }

  return (
    <div className={cn("min-w-0", mobile && "mt-1")}>
      <div className="truncate text-xs text-text-2">{description.summary}</div>
      {description.detail && (
        <div className="mt-0.5 truncate text-[11px] text-text-3">{description.detail}</div>
      )}
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
            <h1 className="text-2xl font-bold lowercase text-text-1">Triggers</h1>
            <p className="text-sm text-text-3 mt-1">Cron schedules and HTTP webhooks</p>
          </div>
          <div className="flex items-center gap-4">
            <span className="text-[11px] font-bold lowercase text-text-3 hidden sm:inline-block">
              {filtered.length} trigger{filtered.length !== 1 ? "s" : ""}
            </span>
            <Button size="sm" onClick={openCreateDialog} className="bg-primary text-primary-foreground hover:bg-primary/90">

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
                    "rounded-full px-3 py-1.5 text-xs border transition-colors flex items-center gap-1.5 font-normal",
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
          <div className="rounded-md border border-graphite/30 bg-midnight/30 h-32 flex flex-col items-center justify-center text-text-3 text-sm">

            No triggers found
          </div>
        )}

        <div className="grid">
          {filtered.map((trigger) => {
            const description = describeTrigger(trigger);
            const config = description.config;
            const isHttp = description.kind === "http";
            const isExpanded = expanded === trigger.id;

            return (
              <section
                key={trigger.id}
                data-testid="trigger-card"
                className={cn(
                  "overflow-hidden border-b border-border transition-colors",
                  isExpanded ? "bg-transparent" : "bg-transparent hover:bg-obsidian cursor-pointer"
                )}
                onClick={() => !isExpanded && setExpanded(trigger.id)}
              >
                <div className="flex min-h-14 items-center justify-between px-3 py-2">
                  <div className="flex items-center gap-4 min-w-0 flex-1">
                    <span className="w-10 shrink-0 text-xs text-text-3">{trigger.type}</span>
                    <div className="min-w-0 flex-1 grid grid-cols-1 md:grid-cols-[2fr_3fr_1fr] items-center gap-4">
                      <div className="truncate">
                        <div className="font-bold text-text-1 text-sm truncate">{trigger.alias}</div>

                      </div>

                      <div className="hidden md:flex items-center">
                        <TriggerAction description={description} externalUrl={features?.external_url} />
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

                        Edit
                      </Button>
                    )}
                    <Button
                      size="icon"
                      variant="ghost"
                      className="h-7 w-7 text-text-3 hover:text-text-2"
                      title={isExpanded ? "Collapse trigger details" : "Expand trigger details"}
                      aria-label={isExpanded ? "Collapse trigger details" : "Expand trigger details"}
                      aria-expanded={isExpanded}
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
                  <div className="border-t border-graphite/20 bg-obsidian/40 px-5 py-4 space-y-4">
                    <div className="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
                      <div className="col-span-2 md:col-span-1">
                        <p className="text-[11px] lowercase font-bold text-text-3 mb-1">Full ID</p>
                        <IdChip value={trigger.id} label="trigger id" />
                      </div>
                      <div>
                        <p className="text-[11px] lowercase font-bold text-text-3 mb-1">Created</p>
                        <p className="text-xs text-text-2"><RelativeTime date={trigger.created_at} /></p>
                      </div>
                      <div>
                        <p className="text-[11px] lowercase font-bold text-text-3 mb-1">Updated</p>
                        <p className="text-xs text-text-2"><RelativeTime date={trigger.updated_at} /></p>
                      </div>
                    </div>

                    <div className="md:hidden">
                      <p className="text-[11px] lowercase font-bold text-text-3 mb-1">Action</p>
                      <TriggerAction description={description} externalUrl={features?.external_url} mobile />
                    </div>

                    <div>
                      <p className="text-[11px] lowercase font-bold text-text-3 mb-1">Raw Configuration</p>
                      <pre className="bg-void border border-graphite/30 text-text-2 rounded-md p-3 text-[11px] overflow-auto max-h-48">
                        {Object.keys(config).length > 0
                          ? JSON.stringify(config, null, 2)
                          : trigger.configuration}
                      </pre>
                    </div>

                    {isHttp && (
                      <div className="rounded-md border border-gold/20 bg-gold/5 px-3 py-2 text-xs text-text-3 flex items-start gap-2">

                        <div>
                          Manual fire is an operator-only API action via <code className="text-[11px] text-text-2 bg-midnight/50 px-1 py-0.5 rounded border border-graphite/30 mx-1">POST /v1/triggers/:id/fire</code>.
                          External systems should POST to the webhook URL instead.
                        </div>
                      </div>
                    )}
                  </div>
                )}
              </section>
            );
          })}
        </div>
      </div>

      <Dialog open={editorOpen} onOpenChange={(open) => !editorPending && setEditorOpen(open)}>
        <DialogContent className="max-w-2xl bg-midnight border-graphite/50 p-4 text-text-1 sm:rounded-md sm:p-6">
          <DialogHeader>
            <DialogTitle className="text-xl font-bold">{editorMode === "create" ? "New HTTP Trigger" : "Edit HTTP Trigger"}</DialogTitle>
            <DialogDescription className="text-text-3">
              Configure the webhook route, auth, and request-body mappings. Standalone triggers only run jobs that already reference them.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleEditorSubmit} className="space-y-5 mt-2">
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <label htmlFor="http-trigger-alias" className={labelClass}>Alias</label>
                <input
                  id="http-trigger-alias"
                  value={formState.alias}
                  onChange={(event) => setFormState((current) => ({ ...current, alias: event.target.value }))}
                  className={inputClass}
                  disabled={editorPending}
                  required
                />
              </div>
              <div>
                <label htmlFor="http-trigger-path" className={labelClass}>Webhook Path</label>
                <input
                  id="http-trigger-path"
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
                <label htmlFor="http-trigger-secret" className={labelClass}>Secret</label>
                <input
                  id="http-trigger-secret"
                  value={formState.secret}
                  onChange={(event) => setFormState((current) => ({ ...current, secret: event.target.value }))}
                  placeholder="shared-secret or secret://..."
                  className={inputClass}
                  disabled={editorPending}
                />
              </div>
              <div>
                <label htmlFor="http-trigger-signatureScheme" className={labelClass}>Auth Scheme</label>
                <select
                  id="http-trigger-signatureScheme"
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
                <label htmlFor="http-trigger-signatureHeader" className={labelClass}>Signature Header</label>
                <input
                  id="http-trigger-signatureHeader"
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
                <label htmlFor="http-trigger-paramMappingText" className={labelClass}>Param Mapping JSON</label>
                <textarea
                  id="http-trigger-paramMappingText"
                  value={formState.paramMappingText}
                  onChange={(event) => setFormState((current) => ({ ...current, paramMappingText: event.target.value }))}
                  className={textareaClass}
                  disabled={editorPending}
                  placeholder="{&#34;ref&#34;: &#34;$.ref&#34;}"
                />
              </div>
              <div>
                <label htmlFor="http-trigger-defaultParamsText" className={labelClass}>Default Params JSON</label>
                <textarea
                  id="http-trigger-defaultParamsText"
                  value={formState.defaultParamsText}
                  onChange={(event) => setFormState((current) => ({ ...current, defaultParamsText: event.target.value }))}
                  className={textareaClass}
                  disabled={editorPending}
                  placeholder="{&#34;env&#34;: &#34;production&#34;}"
                />
              </div>
            </div>

            <div className="rounded-md border border-graphite/30 bg-midnight/40 px-3 py-2.5 text-xs text-text-3">
              Param mappings use simple JSONPath expressions like <code className="mx-1 rounded bg-void/60 border border-graphite/40 px-1 py-0.5 text-[11px] text-text-2">$.ref</code> and
              <code className="mx-1 rounded bg-void/60 border border-graphite/40 px-1 py-0.5 text-[11px] text-text-2">$</code> for the whole payload.
            </div>

            {formError && <p className="text-sm text-danger font-normal">{formError}</p>}

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
                className="bg-primary text-primary-foreground hover:bg-primary/90"
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

function formatCountdown(ms: number): string {
  const seconds = Math.floor(Math.max(0, ms) / 1000);
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor(seconds / 3600) % 24;
  const minutes = Math.floor(seconds / 60) % 60;
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${days ? `${days}d ` : ""}${seconds >= 3600 ? `${pad(hours)}:` : ""}${pad(minutes)}:${pad(seconds % 60)}`;
}
