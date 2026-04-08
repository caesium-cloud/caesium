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

/** Get the current API key, or null if not authenticated. */
export function getApiKey(): string | null {
  return apiKey;
}

/** Returns true if an API key is currently set. */
export function isAuthenticated(): boolean {
  return apiKey !== null;
}

/** Subscribe to auth state changes. Returns an unsubscribe function. */
export function onAuthChange(fn: AuthChangeListener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
