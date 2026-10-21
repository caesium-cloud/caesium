import { describe, expect, it } from "vitest";
import { canActIn, canReplayRuns, requiresRoleReason } from "../access";
import type { PrincipalState } from "@/lib/auth";
import { resolveRoleIn, type PrincipalRole } from "@/lib/roles";

function principal(grants: Record<string, PrincipalRole>): PrincipalState {
  const role = grants["*"] ?? null;
  return {
    kind: "api_key",
    subject: "k",
    role,
    clusterRole: role,
    grants,
    namespaces: Object.keys(grants),
    roleIn: (ns) => resolveRoleIn(grants, ns),
    canRunner: false,
    isScoped: false,
    scopeKnown: true,
  };
}

const anonymous = { kind: null, subject: null, role: null } as PrincipalState;

describe("canActIn", () => {
  it("allows everything when authentication is disabled", () => {
    expect(canActIn(anonymous, "admin", "marketing")).toBe(true);
    expect(canReplayRuns(anonymous)).toBe(true);
  });

  it("separates runner actions from operator actions per namespace", () => {
    const p = principal({ marketing: "runner", finance: "operator" });
    expect(canActIn(p, "runner", "marketing")).toBe(true);
    expect(canActIn(p, "operator", "marketing")).toBe(false);
    expect(canActIn(p, "operator", "finance")).toBe(true);
    expect(canActIn(p, "viewer", "default")).toBe(false);
  });

  it("requires the role in every namespace it is given (job and persisted run)", () => {
    const p = principal({ marketing: "operator", finance: "viewer" });
    expect(canActIn(p, "operator", "marketing", "marketing")).toBe(true);
    expect(canActIn(p, "operator", "marketing", "finance")).toBe(false);
    expect(canActIn(p, "operator", "marketing", undefined)).toBe(true);
  });

  it("falls back to the cluster role while the namespace is unknown", () => {
    expect(canActIn(principal({ marketing: "admin" }), "runner", undefined)).toBe(false);
    expect(canActIn(principal({ "*": "runner" }), "runner", undefined)).toBe(true);
    expect(canActIn(principal({ "*": "runner" }), "runner", "")).toBe(true);
  });

  it("keeps a cluster role authoritative everywhere", () => {
    const p = principal({ "*": "operator" });
    expect(canActIn(p, "operator", "anything", "else")).toBe(true);
    expect(canActIn(p, "admin", "anything")).toBe(false);
  });

  it("fails closed for an authenticated principal with no role", () => {
    expect(canActIn(principal({}), "viewer", "marketing")).toBe(false);
  });
});

describe("canReplayRuns / requiresRoleReason", () => {
  it("gates replay on runner in the job's namespace", () => {
    const p = principal({ marketing: "runner" });
    expect(canReplayRuns(p, "marketing")).toBe(true);
    expect(canReplayRuns(p, "finance")).toBe(false);
  });

  it("words the reason with the namespace when known", () => {
    expect(requiresRoleReason("operator", "marketing")).toBe("Requires operator role in marketing");
    expect(requiresRoleReason("runner")).toBe("Requires runner role");
  });
});
