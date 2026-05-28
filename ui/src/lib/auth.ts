/**
 * In-memory API key storage for UI authentication.
 *
 * The key is stored only in a module-scoped variable — not in localStorage
 * or sessionStorage — to minimise XSS exposure. The session ends when the
 * tab is closed or the user logs out.
 */

type AuthChangeListener = () => void;

export interface AuthMethod {
  type: string;
  id?: string;
  label?: string;
  loginUrl?: string;
  mode?: string;
}

export interface AuthStatus {
  enabled?: boolean;
  methods?: AuthMethod[];
}

export type RedirectAuthMethod = AuthMethod & { loginUrl: string };
export type CredentialAuthMethod = AuthMethod & { loginUrl: string };
export type CredentialLoginResult =
  | "success"
  | "invalid"
  | "denied"
  | "error"
  | "network";

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

/** Return the same-origin path the SSO callback should send the user back to. */
export function currentReturnTo(
  location: Pick<Location, "pathname" | "search" | "hash"> = window.location,
): string {
  return `${location.pathname || "/"}${location.search}${location.hash}`;
}

/** Add the SPA return target to a provider login URL. */
export function ssoLoginUrl(loginUrl: string, returnTo = currentReturnTo()): string {
  const url = new URL(loginUrl, window.location.origin);
  url.searchParams.set("returnTo", returnTo || "/");

  if (url.origin === window.location.origin) {
    return `${url.pathname}${url.search}${url.hash}`;
  }

  return url.toString();
}

/** Returns true for auth methods that should launch a browser redirect. */
export function isRedirectAuthMethod(method: AuthMethod): method is RedirectAuthMethod {
  return typeof method.loginUrl === "string" && method.loginUrl.length > 0 && !isCredentialAuthMethod(method);
}

/** Returns true for auth methods that should submit credentials in-place. */
export function isCredentialAuthMethod(method: AuthMethod): method is CredentialAuthMethod {
  const mode = typeof method.mode === "string" ? method.mode.trim().toLowerCase() : "";
  const type = typeof method.type === "string" ? method.type.trim().toLowerCase() : "";
  return (
    (mode === "credential" || type === "ldap") &&
    typeof method.loginUrl === "string" &&
    method.loginUrl.length > 0
  );
}

/** Stable React key for advertised browser-redirect auth methods. */
export function authMethodKey(method: RedirectAuthMethod): string {
  return method.id ?? `${method.type}:${method.loginUrl}:${method.label ?? ""}`;
}

function authMethodTypeLabel(type: unknown): string {
  if (typeof type !== "string") {
    return "SSO";
  }

  const normalizedType = type.trim();
  switch (normalizedType.toLowerCase()) {
    case "oidc":
      return "OIDC";
    case "saml":
      return "SAML";
    case "ldap":
      return "LDAP";
    default:
      return (
        normalizedType
          .split(/[-_\s]+/)
          .filter(Boolean)
          .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
          .join(" ") || "SSO"
      );
  }
}

/** User-facing label for an advertised auth method, preserving server labels. */
export function authMethodLabel(method: AuthMethod): string {
  const label = typeof method.label === "string" ? method.label.trim() : "";
  return label || `Sign in with ${authMethodTypeLabel(method.type)}`;
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

/** Submit username/password credentials and cache the resulting cookie session. */
export async function credentialLogin(
  loginUrl: string,
  username: string,
  password: string,
): Promise<CredentialLoginResult> {
  try {
    const response = await fetch(loginUrl, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });

    if (response.status === 401) {
      return "invalid";
    }
    if (response.status === 403) {
      return "denied";
    }
    if (!response.ok) {
      return "error";
    }

    return (await checkSession()) ? "success" : "error";
  } catch {
    return "network";
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
