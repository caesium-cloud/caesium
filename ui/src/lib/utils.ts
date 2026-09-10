import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function parseJSONConfig(raw?: string | null): Record<string, unknown> | null {
  if (!raw) {
    return null
  }

  try {
    const parsed = JSON.parse(raw)
    return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed as Record<string, unknown> : null
  } catch {
    return null
  }
}

/**
 * An atom's `command` arrives as the JSON array string the server stores
 * (models.Atom.Command), but task-run rows can carry an already-decoded array
 * or a plain shell string. Normalize all three into argv.
 */
export function normalizeCommand(command?: string | string[]): string[] {
  if (!command) {
    return []
  }
  if (Array.isArray(command)) {
    return command.map(String)
  }

  const trimmed = command.trim()
  if (!trimmed) {
    return []
  }

  try {
    const parsed = JSON.parse(trimmed)
    if (Array.isArray(parsed)) {
      return parsed.map(String)
    }
  } catch {
    // Non-JSON command strings are already displayable.
  }

  return [command]
}

export function formatCommandForDisplay(command?: string | string[]): string {
  const normalized = normalizeCommand(command)
  return normalized.length > 0 ? normalized.join(" ") : "N/A"
}

export function formatDurationNs(value?: number | null): string {
  if (!value) {
    return "0s"
  }

  const abs = Math.abs(value)
  if (abs < 1_000) {
    return `${value}ns`
  }
  if (abs < 1_000_000) {
    return `${(value / 1_000).toFixed(1)}us`
  }
  if (abs < 1_000_000_000) {
    return `${(value / 1_000_000).toFixed(1)}ms`
  }
  if (abs < 60_000_000_000) {
    return `${(value / 1_000_000_000).toFixed(1)}s`
  }
  if (abs < 3_600_000_000_000) {
    return `${(value / 60_000_000_000).toFixed(1)}m`
  }

  return `${(value / 3_600_000_000_000).toFixed(1)}h`
}

export function formatKeyValueMap(value?: Record<string, unknown> | null): string {
  if (!value || Object.keys(value).length === 0) {
    return "None"
  }

  return Object.entries(value)
    .map(([key, entry]) => `${key}=${String(entry)}`)
    .join(", ")
}

export function shortId(value?: string | null, length = 8): string {
  if (!value) {
    return "unknown"
  }

  return value.slice(0, length)
}

function padUTC(value: number): string {
  return String(value).padStart(2, "0")
}

export function formatUTCTimestamp(
  value: string | number | Date | null | undefined,
  fallback = "Unknown time",
): string {
  if (value === null || value === undefined || value === "") {
    return fallback
  }
  const date = value instanceof Date ? value : new Date(value)
  if (!Number.isFinite(date.getTime())) {
    return fallback
  }

  return [
    `${date.getUTCFullYear()}-${padUTC(date.getUTCMonth() + 1)}-${padUTC(date.getUTCDate())}`,
    `${padUTC(date.getUTCHours())}:${padUTC(date.getUTCMinutes())}:${padUTC(date.getUTCSeconds())}`,
    "UTC",
  ].join(" ")
}
