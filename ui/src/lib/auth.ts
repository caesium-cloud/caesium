/**
 * In-memory API key storage for UI authentication.
 *
 * The key is stored only in a module-scoped variable — not in localStorage
 * or sessionStorage — to minimise XSS exposure. The session ends when the
 * tab is closed or the user logs out.
 */

type AuthChangeListener = () => void;

let apiKey: string | null = null;
let cookieSession = false;
let csrfToken: string | null = null;
const listeners: Set<AuthChangeListener> = new Set();

function notifyAuthChange(): void {
  listeners.forEach((fn) => fn());
}

function cloneHeaders(headers?: HeadersInit): Record<string, string> {
  if (!headers) {
    return {};
  }
  if (headers instanceof Headers) {
    return Object.fromEntries(headers.entries());
  }
  if (Array.isArray(headers)) {
    return Object.fromEntries(headers);
  }
  return { ...headers };
}

/** Store the API key in memory (never persisted to disk). */
export function setApiKey(key: string): void {
  apiKey = key;
  notifyAuthChange();
}

/** Clear the API key (logout). */
export function clearApiKey(): void {
  apiKey = null;
  cookieSession = false;
  csrfToken = null;
  notifyAuthChange();
}

/** Returns true if an API key is currently set. */
export function isAuthenticated(): boolean {
  return apiKey !== null || cookieSession;
}

/** Return the cached CSRF header for cookie-session requests. */
export function csrfHeader(): Record<string, string> {
  return csrfToken ? { "X-CSRF-Token": csrfToken } : {};
}

/**
 * Checks whether the browser has a valid cookie session and caches the
 * session-bound CSRF token returned by the server.
 */
export async function checkSession(): Promise<boolean> {
  try {
    const response = await fetch("/auth/whoami", { credentials: "include" });
    if (!response.ok) {
      if (cookieSession || csrfToken) {
        cookieSession = false;
        csrfToken = null;
        notifyAuthChange();
      }
      return false;
    }

    const body = (await response.json()) as { csrf_token?: string };
    csrfToken = body.csrf_token ?? null;
    if (!cookieSession) {
      cookieSession = true;
      notifyAuthChange();
    }
    return true;
  } catch {
    if (cookieSession || csrfToken) {
      cookieSession = false;
      csrfToken = null;
      notifyAuthChange();
    }
    return false;
  }
}

/**
 * Injects the in-memory API key into a request headers object without exposing
 * the raw key to other modules.
 */
export function withAuthHeaders(headers?: HeadersInit): Record<string, string> {
  const nextHeaders = cloneHeaders(headers);
  if (apiKey) {
    nextHeaders.Authorization = `Bearer ${apiKey}`;
  } else {
    Object.assign(nextHeaders, csrfHeader());
  }
  return nextHeaders;
}

/** Subscribe to auth state changes. Returns an unsubscribe function. */
export function onAuthChange(fn: AuthChangeListener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
