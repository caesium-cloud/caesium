import cronParser from "cron-parser";
import type { Trigger } from "@/lib/api";

export type TriggerConfig = Record<string, unknown>;

export type TriggerDescription =
  | {
      kind: "cron";
      triggerType: string;
      config: TriggerConfig;
      summary: string;
      detail?: string;
      expression: string | null;
      timezone?: string;
    }
  | {
      kind: "http";
      triggerType: string;
      config: TriggerConfig;
      summary: string;
      detail?: string;
      path: string;
    }
  | {
      kind: "event";
      triggerType: string;
      config: TriggerConfig;
      summary: string;
      detail?: string;
    }
  | {
      kind: "other";
      triggerType: string;
      config: TriggerConfig;
      summary: string;
      detail?: string;
    };

const cronExpressionKeys = ["expression", "cron", "schedule"] as const;

export function normalizeWebhookPath(path: unknown) {
  if (typeof path !== "string") return "";
  let normalized = path.trim();
  normalized = normalized.replace(/^\/+/, "");
  normalized = normalized.replace(/^v1\/+/, "");
  normalized = normalized.replace(/^hooks\/+/, "");
  return normalized.replace(/\/+$/, "");
}

export function webhookRoute(path: unknown) {
  const normalized = normalizeWebhookPath(path);
  return normalized ? `/v1/hooks/${normalized}` : "";
}

export function parseTriggerConfiguration(trigger: Pick<Trigger, "configuration">): TriggerConfig {
  try {
    const parsed = JSON.parse(trigger.configuration);
    if (isRecord(parsed)) {
      return parsed;
    }
  } catch {
    // Malformed trigger configuration is rendered as an empty structured config.
  }
  return {};
}

export function getCronExpression(config: TriggerConfig): string | null {
  for (const key of cronExpressionKeys) {
    const value = trimmedString(config[key]);
    if (value) return value;
  }
  return null;
}

export function getNextFireDate(expression: string | null | undefined, timezone?: string): Date | null {
  const cronExpression = typeof expression === "string" ? expression.trim() : "";
  if (!cronExpression) return null;

  try {
    const options = timezone ? { tz: timezone } : undefined;
    return cronParser.parse(cronExpression, options).next().toDate();
  } catch {
    return null;
  }
}

export function describeTrigger(trigger: Trigger): TriggerDescription {
  const config = parseTriggerConfiguration(trigger);

  if (trigger.type === "cron") {
    const expression = getCronExpression(config);
    const timezone = trimmedString(config.timezone);
    return {
      kind: "cron",
      triggerType: trigger.type,
      config,
      summary: expression ?? "Missing cron expression",
      detail: timezone ? `Timezone: ${timezone}` : undefined,
      expression,
      timezone: timezone ?? undefined,
    };
  }

  if (trigger.type === "http") {
    const path = webhookRoute(config.path);
    return {
      kind: "http",
      triggerType: trigger.type,
      config,
      summary: path || "HTTP webhook",
      detail: path ? undefined : "missing webhook path",
      path,
    };
  }

  if (trigger.type === "event") {
    return describeEventTrigger(trigger.type, config);
  }

  return {
    kind: "other",
    triggerType: trigger.type,
    config,
    summary: `${formatTriggerType(trigger.type)} trigger`,
    detail: configurationKeySummary(config),
  };
}

function describeEventTrigger(triggerType: string, config: TriggerConfig): TriggerDescription {
  const patterns = eventPatterns(config);
  const summaries = patterns.map(summarizeEventPattern).filter((summary) => summary.length > 0);
  const remaining = Math.max(0, summaries.length - 2);
  const summary =
    summaries.length > 0
      ? `${summaries.slice(0, 2).join(", ")}${remaining > 0 ? ` +${remaining} more` : ""}`
      : "Event trigger";

  return {
    kind: "event",
    triggerType,
    config,
    summary,
    detail: eventDetailSummary(config, patterns),
  };
}

function eventPatterns(config: TriggerConfig): TriggerConfig[] {
  if (Array.isArray(config.events)) {
    return config.events.filter(isRecord);
  }
  if (isRecord(config.event)) {
    return [config.event];
  }
  if (trimmedString(config.type)) {
    return [config];
  }
  return [];
}

function summarizeEventPattern(pattern: TriggerConfig) {
  const type = trimmedString(pattern.type) ?? "any event";
  const source = trimmedString(pattern.source);
  return source ? `${type} from ${source}` : type;
}

function eventDetailSummary(config: TriggerConfig, patterns: TriggerConfig[]) {
  const filterCount = patterns.reduce((count, pattern) => count + recordSize(pattern.filter), 0);
  const mappedParamCount = recordSize(config.paramMapping);
  const defaultParamCount = recordSize(config.defaultParams);
  const parts = [
    countLabel(filterCount, "filter"),
    countLabel(mappedParamCount, "mapped param"),
    countLabel(defaultParamCount, "default param"),
  ].filter(Boolean);
  return parts.length > 0 ? parts.join(", ") : undefined;
}

function configurationKeySummary(config: TriggerConfig) {
  const keys = Object.keys(config).sort();
  if (keys.length === 0) return "no configuration";
  const visible = keys.slice(0, 3).join(", ");
  return `configuration keys: ${visible}${keys.length > 3 ? ` +${keys.length - 3} more` : ""}`;
}

function formatTriggerType(type: string) {
  if (!type) return "Unknown";
  return type
    .split(/[-_\s]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

function countLabel(count: number, label: string) {
  if (count === 0) return null;
  return `${count} ${label}${count === 1 ? "" : "s"}`;
}

function recordSize(value: unknown) {
  return isRecord(value) ? Object.keys(value).length : 0;
}

function trimmedString(value: unknown) {
  if (typeof value !== "string") return null;
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}

function isRecord(value: unknown): value is TriggerConfig {
  return Boolean(value && typeof value === "object" && !Array.isArray(value));
}
