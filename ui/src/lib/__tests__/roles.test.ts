import { describe, expect, it } from "vitest";
import { resolveRoleIn, roleAtLeast, roleInNamespace, type PrincipalRole } from "../roles";

describe("resolveRoleIn", () => {
  const grants: Record<string, PrincipalRole> = { "*": "viewer", marketing: "operator", finance: "runner" };

  it("takes the highest of the wildcard and the namespace grant", () => {
    expect(resolveRoleIn(grants, "marketing")).toBe("operator");
    expect(resolveRoleIn(grants, "finance")).toBe("runner");
    // A narrower namespace grant never lowers the wildcard role.
    expect(resolveRoleIn({ "*": "admin", marketing: "viewer" }, "marketing")).toBe("admin");
  });

  it("lets the wildcard grant answer for a namespace absent from the map", () => {
    expect(resolveRoleIn(grants, "undeclared")).toBe("viewer");
    expect(resolveRoleIn(grants, "")).toBe("viewer");
    expect(resolveRoleIn(grants, null)).toBe("viewer");
    expect(resolveRoleIn(grants, undefined)).toBe("viewer");
    expect(resolveRoleIn(grants, "*")).toBe("viewer");
  });

  it("is null only when neither a wildcard nor a namespace grant exists", () => {
    const confined: Record<string, PrincipalRole> = { marketing: "operator" };
    expect(resolveRoleIn(confined, "finance")).toBeNull();
    expect(resolveRoleIn(confined, "")).toBeNull();
    expect(resolveRoleIn(confined, null)).toBeNull();
    expect(resolveRoleIn({}, "marketing")).toBeNull();
  });

  it("does not read inherited object keys as grants", () => {
    expect(resolveRoleIn({}, "constructor")).toBeNull();
    expect(resolveRoleIn({}, "__proto__")).toBeNull();
  });
});

describe("roleAtLeast / roleInNamespace", () => {
  it("orders the four roles", () => {
    expect(roleAtLeast("admin", "operator")).toBe(true);
    expect(roleAtLeast("runner", "operator")).toBe(false);
    expect(roleAtLeast(null, "viewer")).toBe(false);
  });

  it("falls back to the cluster role for a principal without grants", () => {
    expect(roleInNamespace({ role: "runner" }, "anywhere")).toBe("runner");
    expect(roleInNamespace({ role: null }, "anywhere")).toBeNull();
    expect(roleInNamespace({ role: "viewer", grants: { marketing: "admin" } }, "finance")).toBeNull();
  });
});
