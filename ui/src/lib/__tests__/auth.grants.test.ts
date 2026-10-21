import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  apiKeyLogin,
  clearApiKey,
  clearLoginError,
  consumeLoginError,
  credentialLogin,
  getPrincipal,
} from "@/lib/auth";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function okWhoami(body: Record<string, unknown>) {
  mockFetch.mockResolvedValueOnce({ ok: true, status: 200, json: async () => body });
}

describe("principal grants", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearApiKey();
  });

  it("parses the current whoami shape", async () => {
    okWhoami({
      kind: "user",
      subject: "ada@example.com",
      role: "",
      cluster_role: "",
      grants: { marketing: "operator", finance: "viewer", bogus: "root" },
      namespaces: ["finance", "marketing"],
      is_scoped: false,
    });
    expect(await apiKeyLogin("csk_live_x")).toBe("success");
    const p = getPrincipal();
    expect(p.role).toBeNull();
    expect(p.clusterRole).toBeNull();
    expect(p.canRunner).toBe(false);
    expect(p.grants).toEqual({ marketing: "operator", finance: "viewer" });
    expect(p.namespaces).toEqual(["finance", "marketing"]);
    expect(p.roleIn("marketing")).toBe("operator");
    expect(p.roleIn("finance")).toBe("viewer");
    expect(p.roleIn("default")).toBeNull();
    expect(p.roleIn("")).toBeNull();
  });

  it("lets a wildcard grant answer for any namespace, including absent ones", async () => {
    okWhoami({
      kind: "api_key",
      subject: "key",
      role: "viewer",
      cluster_role: "viewer",
      grants: { "*": "viewer", marketing: "admin" },
      namespaces: ["*"],
      is_scoped: false,
    });
    await apiKeyLogin("csk_live_x");
    const p = getPrincipal();
    expect(p.clusterRole).toBe("viewer");
    expect(p.roleIn("never-declared")).toBe("viewer");
    expect(p.roleIn("")).toBe("viewer");
    expect(p.roleIn("marketing")).toBe("admin");
  });

  it("degrades safely on an older server that sends only role", async () => {
    okWhoami({ kind: "api_key", subject: "key", role: "runner" });
    await apiKeyLogin("csk_live_x");
    const p = getPrincipal();
    expect(p.role).toBe("runner");
    expect(p.grants).toEqual({ "*": "runner" });
    expect(p.namespaces).toEqual(["*"]);
    expect(p.roleIn("anything")).toBe("runner");
  });

  it("grants nothing on an older server with an explicit empty role", async () => {
    okWhoami({ kind: "user", subject: "u", role: "" });
    await apiKeyLogin("csk_live_x");
    const p = getPrincipal();
    expect(p.grants).toEqual({});
    expect(p.roleIn("default")).toBeNull();
  });

  it("ignores a malformed grants field", async () => {
    okWhoami({ kind: "api_key", subject: "k", role: "viewer", grants: ["marketing"] });
    await apiKeyLogin("csk_live_x");
    expect(getPrincipal().grants).toEqual({ "*": "viewer" });
  });
});

describe("credential login denial reasons", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearApiKey();
  });

  it("maps a no_binding 403 body to its own result", async () => {
    mockFetch.mockResolvedValueOnce({
      status: 403,
      ok: false,
      json: async () => ({ message: "login denied", reason: "no_binding" }),
    });
    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "pw")).resolves.toBe("no_binding");
  });

  it("keeps every other denial generic", async () => {
    mockFetch.mockResolvedValueOnce({
      status: 403,
      ok: false,
      json: async () => ({ message: "login denied" }),
    });
    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "pw")).resolves.toBe("denied");
    mockFetch.mockResolvedValueOnce({
      status: 403,
      ok: false,
      json: async () => ({ message: "login denied", reason: "user_disabled" }),
    });
    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "pw")).resolves.toBe("denied");
  });

  it("treats a malformed 403 body as an ordinary denial", async () => {
    mockFetch.mockResolvedValueOnce({
      status: 403,
      ok: false,
      json: async () => {
        throw new SyntaxError("not json");
      },
    });
    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "pw")).resolves.toBe("denied");
    mockFetch.mockResolvedValueOnce({ status: 403, ok: false, json: async () => "no_binding" });
    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "pw")).resolves.toBe("denied");
  });
});

describe("consumeLoginError", () => {
  beforeEach(() => clearLoginError());

  function at(search: string) {
    const replaceState = vi.fn();
    const location = { pathname: "/jobs", search, hash: "#x" } as Location;
    return { location, history: { replaceState } };
  }

  it("reads and strips ?login_error=no_binding, remembering it for a re-read", () => {
    const { location, history } = at("?login_error=no_binding&keep=1");
    expect(consumeLoginError(location, history)).toBe("no_binding");
    expect(history.replaceState).toHaveBeenCalledWith(null, "", "/jobs?keep=1#x");
    // A strict-mode double initialiser sees the already-stripped URL.
    expect(consumeLoginError(at("?keep=1").location, history)).toBe("no_binding");
    clearLoginError();
    expect(consumeLoginError(at("").location, history)).toBeNull();
  });

  it("forgets the denial once any sign-in succeeds, so it cannot return after logout", async () => {
    const { location, history } = at("?login_error=no_binding");
    expect(consumeLoginError(location, history)).toBe("no_binding");
    okWhoami({ kind: "api_key", subject: "k", role: "viewer" });
    expect(await apiKeyLogin("csk_live_x")).toBe("success");
    clearApiKey();
    expect(consumeLoginError(at("").location, history)).toBeNull();
  });

  it("ignores values it does not understand", () => {
    const { location, history } = at("?login_error=drop_table");
    expect(consumeLoginError(location, history)).toBeNull();
  });
});
