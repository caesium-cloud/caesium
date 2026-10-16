import { type ApiError } from "@/lib/api";
import type { PrincipalState } from "@/lib/auth";

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
