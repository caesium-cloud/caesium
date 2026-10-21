/**
 * Pure role arithmetic shared by the principal (lib/auth.ts) and the action
 * gates (features/auth/access.ts). No state, no network: the server stays the
 * authority, this only decides which controls to offer.
 */

export type PrincipalRole = "viewer" | "runner" | "operator" | "admin";

/** Wildcard namespace key in a principal's grants: a role held cluster-wide. */
export const CLUSTER_NAMESPACE = "*";

export function roleLevel(role: PrincipalRole | null): number {
  switch (role) {
    case "admin":
      return 40;
    case "operator":
      return 30;
    case "runner":
      return 20;
    case "viewer":
      return 10;
    default:
      return 0;
  }
}

/** True when `role` is at least `minimum`. */
export function roleAtLeast(role: PrincipalRole | null, minimum: PrincipalRole): boolean {
  return roleLevel(role) >= roleLevel(minimum);
}

/** max(grants["*"], grants[namespace]); null only when neither exists. */
export function resolveRoleIn(
  grants: Record<string, PrincipalRole>,
  namespace: string | null | undefined,
): PrincipalRole | null {
  const wildcard = grants[CLUSTER_NAMESPACE] ?? null;
  const specific =
    typeof namespace === "string" &&
    namespace !== "" &&
    namespace !== CLUSTER_NAMESPACE &&
    Object.prototype.hasOwnProperty.call(grants, namespace)
      ? grants[namespace]
      : null;
  return roleLevel(specific) > roleLevel(wildcard) ? specific : wildcard;
}

/**
 * The role a principal holds in `namespace`. Tolerates a partial principal
 * (no grants field): then its cluster role applies everywhere.
 */
export function roleInNamespace(
  principal: { role: PrincipalRole | null; grants?: Record<string, PrincipalRole> },
  namespace: string | null | undefined,
): PrincipalRole | null {
  if (principal.grants) {
    return resolveRoleIn(principal.grants, namespace);
  }
  return principal.role;
}
