import { type ApiError } from "@/lib/api";
import type { PrincipalState } from "@/lib/auth";
import { roleAtLeast, roleInNamespace, type PrincipalRole } from "@/lib/roles";

export function isAnonymous(principal: Pick<PrincipalState, "role" | "kind" | "subject">): boolean {
  return principal.role === null && principal.kind === null && principal.subject === null;
}

/**
 * Whether the principal holds at least `minimum` in every given namespace
 * (UI convenience; the server stays the authority). Mirrors the server rule
 * for a namespaced route: the role must hold in the job's namespace and, for a
 * historical run/backfill mutation, in the persisted namespace too.
 *
 * - No namespace known yet (job still loading, older server): the cluster
 *   role decides, so a confined principal sees fewer actions, never more.
 * - Anonymous principal (authentication disabled): allowed, as before.
 */
export function canActIn(
  principal: PrincipalState,
  minimum: PrincipalRole,
  ...namespaces: Array<string | null | undefined>
): boolean {
  if (isAnonymous(principal)) {
    return true;
  }
  const known = namespaces.filter((ns): ns is string => typeof ns === "string" && ns !== "");
  if (known.length === 0) {
    return roleAtLeast(roleInNamespace(principal, null), minimum);
  }
  return known.every((ns) => roleAtLeast(roleInNamespace(principal, ns), minimum));
}

/** The tooltip for an action the principal's grants do not allow. */
export function requiresRoleReason(minimum: PrincipalRole, namespace?: string | null): string {
  return namespace ? `Requires ${minimum} role in ${namespace}` : `Requires ${minimum} role`;
}

/** Replay stays available without auth; a runner in the job's namespace may launch it. */
export function canReplayRuns(principal: PrincipalState, namespace?: string | null): boolean {
  return canActIn(principal, "runner", namespace);
}

/** Connector reads require an authenticated, unscoped cluster principal. */
export function canReadConnectors(principal: PrincipalState): boolean {
  return (principal.kind === "user" || principal.kind === "api_key") && !!principal.subject && principal.scopeKnown && !principal.isScoped &&
    ["viewer", "runner", "operator", "admin"].includes(principal.role ?? "");
}

/**
 * Type-guard for a typed 403 insufficient-access error from the API client.
 * Lives in a non-component module so the `InsufficientAccess` component file
 * only exports components (react-refresh/only-export-components). Stream B/F
 * import this to detect 403s.
 */
export function isInsufficientAccessError(error: unknown): error is ApiError {
  return (
    !!error &&
    typeof error === "object" &&
    (error as { status?: unknown }).status === 403 &&
    (error as { kind?: unknown }).kind === "insufficient_access"
  );
}

/** react-query `retry` that does not retry a denial: a 403 will not change on retry. */
export function retryUnlessDenied(failureCount: number, error: unknown): boolean {
  return !isInsufficientAccessError(error) && failureCount < 3;
}
