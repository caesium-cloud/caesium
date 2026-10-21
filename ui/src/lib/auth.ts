/**
 * In-memory API key storage for UI authentication.
 *
 * The key is stored only in a module-scoped variable — not in localStorage
 * or sessionStorage — to minimise XSS exposure. The session ends when the
 * tab is closed or the user logs out.
 */

import { useSyncExternalStore } from "react";
import { CLUSTER_NAMESPACE, resolveRoleIn, roleLevel, type PrincipalRole } from "./roles";

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

export type RedirectAuthMethod = AuthMethod & { loginUrl: string; mode: "redirect" };
export type CredentialAuthMethod = AuthMethod & { loginUrl: string; mode: "credential" };
export type CredentialLoginResult =
  | "success"
  | "invalid"
  | "denied"
  | "no_binding"
  | "error"
  | "network";
export type ApiKeyLoginResult = CredentialLoginResult;
export type { PrincipalRole } from "./roles";

export interface PrincipalState {
  kind: string | null;
  stableId?: string | null;
  subject: string | null;
  /** The cluster-wide role (what `role` always meant); null when confined. */
  role: PrincipalRole | null;
  /** Roles by namespace; "*" holds a cluster-wide role. */
  grants: Record<string, PrincipalRole>;
  /** Namespaces the principal may read; ["*"] when cluster-wide. */
  namespaces: string[];
  /** Same as `role`, named for the namespaced model. */
  clusterRole: PrincipalRole | null;
  /** The role held in `namespace`: max(grants["*"], grants[namespace]), or null. */
  roleIn: (namespace: string | null | undefined) => PrincipalRole | null;
  canRunner: boolean;
  isScoped: boolean;
  scopeKnown: boolean;
}

let apiKey: string | null = null;
let cookieSession = false;
let csrfToken: string | null = null;
const anonymousPrincipal: PrincipalState = Object.freeze({
  kind: null,
  stableId: null,
  subject: null,
  role: null,
  grants: Object.freeze({}) as Record<string, PrincipalRole>,
  namespaces: Object.freeze([]) as unknown as string[],
  clusterRole: null,
  roleIn: () => null,
  canRunner: false,
  isScoped: false,
  scopeKnown: false,
});
let principal: PrincipalState = anonymousPrincipal;
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


function normalizeRole(value: unknown): PrincipalRole | null {
  if (typeof value !== "string") {
    return null;
  }
  const role = value.trim().toLowerCase();
  switch (role) {
    case "viewer":
    case "runner":
    case "operator":
    case "admin":
      return role;
    default:
      return null;
  }
}

function grantsFromWhoami(
  obj: Record<string, unknown>,
  clusterRole: PrincipalRole | null,
): { grants: Record<string, PrincipalRole>; namespaces: string[] } {
  const raw = obj.grants;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    // An older server: only the cluster role is known, so it applies everywhere.
    return clusterRole
      ? { grants: { [CLUSTER_NAMESPACE]: clusterRole }, namespaces: [CLUSTER_NAMESPACE] }
      : { grants: {}, namespaces: [] };
  }
  const grants: Record<string, PrincipalRole> = {};
  for (const [namespace, value] of Object.entries(raw as Record<string, unknown>)) {
    const role = normalizeRole(value);
    if (namespace !== "" && role) {
      grants[namespace] = role;
    }
  }
  const namespaces = Array.isArray(obj.namespaces)
    ? obj.namespaces.filter((n): n is string => typeof n === "string" && n !== "")
    : Object.keys(grants).sort();
  return { grants, namespaces };
}

function scopeFromWhoami(
  body: Record<string, unknown>,
  kind: string | null,
): Pick<PrincipalState, "isScoped" | "scopeKnown"> {
  const explicitScoped = body.is_scoped ?? body.isScoped ?? body.scoped;
  if (typeof explicitScoped === "boolean") {
    return { isScoped: explicitScoped, scopeKnown: true };
  }

  const scope = body.scope;
  if (scope && typeof scope === "object" && !Array.isArray(scope)) {
    const jobs = (scope as { jobs?: unknown }).jobs;
    if (Array.isArray(jobs)) {
      return {
        isScoped: jobs.some((job) => typeof job === "string" && job.trim() !== ""),
        scopeKnown: true,
      };
    }
  }

  // /auth/whoami emits `scope` only for a job-scoped API key (the scope object
  // is omitted, not emitted empty, for unscoped principals), so reaching here
  // means no restriction was declared. User principals are unscoped by
  // construction; for an api-key principal we still cannot distinguish
  // "unscoped" from "an older server that never serialised scope", so leave
  // scopeKnown false rather than asserting an unscoped principal.
  if (kind === "user") {
    return { isScoped: false, scopeKnown: true };
  }

  return { isScoped: false, scopeKnown: false };
}

function principalFromWhoami(body: unknown): PrincipalState | null {
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    return null;
  }

  const obj = body as Record<string, unknown>;
  // An explicit empty role means this principal has no cluster grant (for
  // example a namespace-only user). Keep it distinct from an older server
  // that omitted the role field.
  const role = obj.role === "" ? null : normalizeRole(obj.role) ?? "viewer";

  const kind =
    typeof obj.kind === "string" && obj.kind.trim() !== "" ? obj.kind.trim() : null;
  const subject =
    typeof obj.subject === "string" && obj.subject.trim() !== ""
      ? obj.subject.trim()
      : null;
  const stableId =
    typeof obj.stable_id === "string" && obj.stable_id.trim() !== ""
      ? obj.stable_id.trim()
      : null;
  const scope = scopeFromWhoami(obj, kind);
  // cluster_role equals role on a current server.
  const clusterRole =
    obj.cluster_role === "" ? null : normalizeRole(obj.cluster_role) ?? role;
  const { grants, namespaces } = grantsFromWhoami(obj, role);
  return {
    kind,
    stableId,
    subject,
    role,
    grants,
    namespaces,
    clusterRole,
    roleIn: (namespace) => resolveRoleIn(grants, namespace),
    canRunner: roleLevel(role) >= roleLevel("runner"),
    isScoped: scope.isScoped,
    scopeKnown: scope.scopeKnown,
  };
}

function clearPrincipal(): void {
  principal = anonymousPrincipal;
}

/** The consumed `?login_error=` reason, kept until a sign-in succeeds or is cleared. */
let pendingLoginError: LoginErrorReason | null = null;

function setPrincipal(nextPrincipal: PrincipalState): void {
  principal = nextPrincipal;
  // Any successful sign-in ends an earlier denial's hand-off, so it cannot
  // resurface on the login page after a later sign-out.
  pendingLoginError = null;
}

async function readWhoami(response: Response): Promise<PrincipalState | null> {
  return principalFromWhoami(await response.json());
}

/** Store the API key in memory (never persisted to disk). */
export function setApiKey(key: string): void {
  apiKey = key;
  cookieSession = false;
  csrfToken = null;
  clearPrincipal();
  notifyAuthChange();
}

/** Clear the API key (logout). */
export function clearApiKey(): void {
  apiKey = null;
  cookieSession = false;
  csrfToken = null;
  clearPrincipal();
  notifyAuthChange();
}

function clearCookieSession(): void {
  if (cookieSession || csrfToken || (apiKey === null && principal !== anonymousPrincipal)) {
    cookieSession = false;
    csrfToken = null;
    if (apiKey === null) {
      clearPrincipal();
    }
    notifyAuthChange();
  }
}

/** Returns true if an API key is currently set. */
export function isAuthenticated(): boolean {
  return apiKey !== null || cookieSession;
}

/** Return the cached CSRF header for cookie-session requests. */
export function csrfHeader(): Record<string, string> {
  return csrfToken ? { "X-CSRF-Token": csrfToken } : {};
}

/** Return the current principal snapshot for non-React call sites. */
export function getPrincipal(): PrincipalState {
  return principal;
}

/** React accessor for the current principal and derived affordance flags. */
export function usePrincipal(): PrincipalState {
  return useSyncExternalStore(onAuthChange, getPrincipal, getPrincipal);
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
  return authMethodMode(method) === "redirect" && hasLoginUrl(method);
}

/** Returns true for auth methods that should submit credentials in-place. */
export function isCredentialAuthMethod(method: AuthMethod): method is CredentialAuthMethod {
  return authMethodMode(method) === "credential" && hasLoginUrl(method);
}

function authMethodMode(method: AuthMethod): string {
  const mode = typeof method.mode === "string" ? method.mode.trim().toLowerCase() : "";
  return mode;
}

function hasLoginUrl(method: AuthMethod): method is AuthMethod & { loginUrl: string } {
  return typeof method.loginUrl === "string" && method.loginUrl.length > 0;
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
      clearCookieSession();
      return false;
    }

    const body = (await response.json()) as Record<string, unknown>;
    if (typeof body.csrf_token !== "string" || body.csrf_token.length === 0) {
      clearCookieSession();
      return false;
    }
    // Authentication is proven by the successful whoami response and CSRF token.
    // Missing or future roles fall back to viewer so UI affordances stay fail-closed.
    const nextPrincipal = principalFromWhoami(body);
    if (!nextPrincipal) {
      clearCookieSession();
      return false;
    }

    csrfToken = body.csrf_token;
    cookieSession = true;
    apiKey = null;
    setPrincipal(nextPrincipal);
    notifyAuthChange();
    return true;
  } catch {
    clearCookieSession();
    return false;
  }
}

/** Revoke the browser session if reachable, then always clear local auth state. */
export async function logout(): Promise<void> {
  try {
    await fetch("/auth/logout", {
      method: "POST",
      credentials: "include",
      headers: csrfHeader(),
    });
  } catch {
    // Local logout must still complete if the server is unreachable.
  } finally {
    clearApiKey();
  }
}

/** The `reason` of a 403 login-denied body; "" for any other or malformed body. */
async function deniedReason(response: Response): Promise<string> {
  try {
    const body: unknown = await response.json();
    if (body && typeof body === "object" && !Array.isArray(body)) {
      const reason = (body as { reason?: unknown }).reason;
      return typeof reason === "string" ? reason : "";
    }
  } catch {
    // A non-JSON body is an ordinary denial.
  }
  return "";
}

/** The login-error reasons the server hands the SPA on a redirect login. */
export type LoginErrorReason = "no_binding";

/**
 * Reads and strips `?login_error=` from the address bar (the redirect login's
 * denial hand-off). Returns a reason only for values this build understands.
 */
export function consumeLoginError(
  location: Location = window.location,
  history: Pick<History, "replaceState"> = window.history,
): LoginErrorReason | null {
  const params = new URLSearchParams(location.search);
  const value = params.get("login_error");
  if (value === null) {
    // Already stripped by an earlier read on this page load (React strict mode
    // double-invokes initialisers), so keep answering until it is cleared.
    return pendingLoginError;
  }
  params.delete("login_error");
  const query = params.toString();
  try {
    history.replaceState(null, "", `${location.pathname}${query ? `?${query}` : ""}${location.hash}`);
  } catch {
    // Stripping is cosmetic.
  }
  pendingLoginError = value === "no_binding" ? "no_binding" : null;
  return pendingLoginError;
}

/** Forget a consumed login error (after a sign-in attempt or dismissal). */
export function clearLoginError(): void {
  pendingLoginError = null;
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
      return (await deniedReason(response)) === "no_binding" ? "no_binding" : "denied";
    }
    if (!response.ok) {
      return "error";
    }

    return (await checkSession()) ? "success" : "error";
  } catch {
    return "network";
  }
}

/** Verify an API key via /auth/whoami, then cache the key and principal. */
export async function apiKeyLogin(key: string): Promise<ApiKeyLoginResult> {
  try {
    const response = await fetch("/auth/whoami", {
      credentials: "include",
      headers: { Authorization: `Bearer ${key}` },
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

    const nextPrincipal = await readWhoami(response);
    if (!nextPrincipal) {
      return "error";
    }

    apiKey = key;
    cookieSession = false;
    csrfToken = null;
    setPrincipal(nextPrincipal);
    notifyAuthChange();
    return "success";
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
