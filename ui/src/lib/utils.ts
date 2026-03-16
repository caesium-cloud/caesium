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
