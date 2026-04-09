/**
 * In-memory API key storage for UI authentication.
 *
 * The key is stored only in a module-scoped variable — not in localStorage
 * or sessionStorage — to minimise XSS exposure. The session ends when the
 * tab is closed or the user logs out.
 */

type AuthChangeListener = () => void;

let apiKey: string | null = null;
const listeners: Set<AuthChangeListener> = new Set();

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
  listeners.forEach((fn) => fn());
}

/** Clear the API key (logout). */
export function clearApiKey(): void {
  apiKey = null;
  listeners.forEach((fn) => fn());
}

/** Returns true if an API key is currently set. */
export function isAuthenticated(): boolean {
  return apiKey !== null;
}

/**
 * Injects the in-memory API key into a request headers object without exposing
 * the raw key to other modules.
 */
export function withAuthHeaders(headers?: HeadersInit): Record<string, string> {
  const nextHeaders = cloneHeaders(headers);
  if (apiKey) {
    nextHeaders.Authorization = `Bearer ${apiKey}`;
  }
  return nextHeaders;
}

/** Subscribe to auth state changes. Returns an unsubscribe function. */
export function onAuthChange(fn: AuthChangeListener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
