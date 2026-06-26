import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  apiKeyLogin,
  authMethodKey,
  authMethodLabel,
  checkSession,
  clearApiKey,
  credentialLogin,
  currentReturnTo,
  isCredentialAuthMethod,
  isRedirectAuthMethod,
  isAuthenticated,
  logout,
  onAuthChange,
  type PrincipalRole,
  type RedirectAuthMethod,
  setApiKey,
  ssoLoginUrl,
  usePrincipal,
  withAuthHeaders,
} from "@/lib/auth";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function whoami(role: PrincipalRole, extra: Record<string, unknown> = {}) {
  return {
    kind: "api_key",
    subject: "csk_live_good",
    role,
    ...extra,
  };
}

describe("auth", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearApiKey();
  });

  it("stores and clears the api key in memory", () => {
    expect(isAuthenticated()).toBe(false);

    setApiKey("csk_live_secret");
    expect(isAuthenticated()).toBe(true);
    expect(withAuthHeaders()).toMatchObject({
      Authorization: "Bearer csk_live_secret",
    });

    clearApiKey();
    expect(isAuthenticated()).toBe(false);
    expect(withAuthHeaders()).not.toHaveProperty("Authorization");
  });

  it("notifies listeners on auth changes", () => {
    const listener = vi.fn();
    const unsubscribe = onAuthChange(listener);

    setApiKey("csk_live_secret");
    clearApiKey();

    expect(listener).toHaveBeenCalledTimes(2);

    unsubscribe();
    setApiKey("csk_live_other");
    expect(listener).toHaveBeenCalledTimes(2);
  });

  it("checks cookie sessions with credentials and caches the csrf token", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: async () =>
        whoami("viewer", {
          kind: "user",
          subject: "ops@example.com",
          csrf_token: "csrf-secret",
        }),
    });

    await expect(checkSession()).resolves.toBe(true);

    expect(mockFetch).toHaveBeenCalledWith("/auth/whoami", { credentials: "include" });
    expect(isAuthenticated()).toBe(true);
    expect(withAuthHeaders()).toMatchObject({
      "X-CSRF-Token": "csrf-secret",
    });
    expect(withAuthHeaders()).not.toHaveProperty("Authorization");
  });

  it("fails closed when a cookie session response omits the csrf token", async () => {
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () =>
          whoami("viewer", {
            kind: "user",
            subject: "ops@example.com",
            csrf_token: "csrf-secret",
          }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ kind: "user", email: "ops@example.com" }),
      });

    await expect(checkSession()).resolves.toBe(true);
    await expect(checkSession()).resolves.toBe(false);

    expect(isAuthenticated()).toBe(false);
    expect(withAuthHeaders()).not.toHaveProperty("X-CSRF-Token");
  });

  it("builds a same-page return target for SSO callbacks", () => {
    expect(
      currentReturnTo({
        pathname: "/runs",
        search: "?status=failed",
        hash: "#latest",
      }),
    ).toBe("/runs?status=failed#latest");
  });

  it("adds returnTo to SSO login URLs", () => {
    expect(ssoLoginUrl("/auth/sso/oidc/login?prompt=login", "/jobs#new")).toBe(
      "/auth/sso/oidc/login?prompt=login&returnTo=%2Fjobs%23new",
    );
  });

  it("identifies browser redirect auth methods and labels OIDC, SAML, and LDAP", () => {
    const oidc: RedirectAuthMethod = {
      type: "oidc",
      id: "corp-oidc",
      label: "  Sign in with Corp SSO  ",
      loginUrl: "/auth/sso/oidc/login",
      mode: "redirect",
    };
    const saml: RedirectAuthMethod = {
      type: "saml",
      loginUrl: "/auth/sso/saml/login",
      mode: "redirect",
    };
    const ldap = {
      type: "ldap",
      mode: "credential",
      loginUrl: "/auth/sso/ldap/login",
    };

    expect(isRedirectAuthMethod(oidc)).toBe(true);
    expect(isRedirectAuthMethod(saml)).toBe(true);
    expect(isRedirectAuthMethod(ldap)).toBe(false);
    expect(isCredentialAuthMethod(ldap)).toBe(true);
    expect(isCredentialAuthMethod({ type: "ldap", loginUrl: "/auth/sso/ldap/login" })).toBe(false);
    expect(isRedirectAuthMethod({ type: "api-key" })).toBe(false);
    expect(authMethodKey(oidc)).toBe("corp-oidc");
    expect(authMethodKey(saml)).toBe("saml:/auth/sso/saml/login:");
    expect(authMethodLabel(oidc)).toBe("Sign in with Corp SSO");
    expect(authMethodLabel(saml)).toBe("Sign in with SAML");
    expect(authMethodLabel(ldap)).toBe("Sign in with LDAP");
  });

  it("posts credential logins with cookies and caches the resulting session", async () => {
    mockFetch
      .mockResolvedValueOnce({ status: 204, ok: true })
      .mockResolvedValueOnce({
        ok: true,
        json: async () =>
          whoami("viewer", {
            kind: "user",
            subject: "ops@example.com",
            csrf_token: "csrf-secret",
          }),
      });

    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "secret")).resolves.toBe("success");

    expect(mockFetch).toHaveBeenNthCalledWith(1, "/auth/sso/ldap/login", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: "ada", password: "secret" }),
    });
    expect(mockFetch).toHaveBeenNthCalledWith(2, "/auth/whoami", { credentials: "include" });
    expect(isAuthenticated()).toBe(true);
    expect(withAuthHeaders()).toMatchObject({
      "X-CSRF-Token": "csrf-secret",
    });
  });

  it("posts logout with the csrf header and clears local state on 401", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () =>
        whoami("viewer", {
          kind: "user",
          subject: "ops@example.com",
          csrf_token: "csrf-secret",
        }),
    });

    await expect(checkSession()).resolves.toBe(true);

    const listener = vi.fn();
    const unsubscribe = onAuthChange(listener);
    mockFetch.mockClear();
    mockFetch.mockResolvedValueOnce({ status: 401, ok: false });

    await expect(logout()).resolves.toBeUndefined();

    expect(mockFetch).toHaveBeenCalledWith("/auth/logout", {
      method: "POST",
      credentials: "include",
      headers: { "X-CSRF-Token": "csrf-secret" },
    });
    expect(isAuthenticated()).toBe(false);
    expect(withAuthHeaders()).not.toHaveProperty("X-CSRF-Token");
    expect(listener).toHaveBeenCalledTimes(1);

    unsubscribe();
  });

  it("clears local auth state when logout cannot reach the server", async () => {
    setApiKey("csk_live_secret");

    const listener = vi.fn();
    const unsubscribe = onAuthChange(listener);
    mockFetch.mockRejectedValueOnce(new Error("offline"));

    await expect(logout()).resolves.toBeUndefined();

    expect(mockFetch).toHaveBeenCalledWith("/auth/logout", {
      method: "POST",
      credentials: "include",
      headers: {},
    });
    expect(isAuthenticated()).toBe(false);
    expect(withAuthHeaders()).not.toHaveProperty("Authorization");
    expect(listener).toHaveBeenCalledTimes(1);

    unsubscribe();
  });

  it("maps credential login denial statuses without caching a session", async () => {
    mockFetch.mockResolvedValueOnce({ status: 403, ok: false });

    await expect(credentialLogin("/auth/sso/ldap/login", "ada", "secret")).resolves.toBe("denied");

    expect(mockFetch).toHaveBeenCalledTimes(1);
    expect(isAuthenticated()).toBe(false);
  });

  it("falls back to SSO labels for malformed auth methods", () => {
    const missingType = { loginUrl: "/auth/sso/custom/login" } as unknown as RedirectAuthMethod;
    const objectType = {
      type: { nested: "bad" },
      label: 123,
      loginUrl: "/auth/sso/custom/login",
    } as unknown as RedirectAuthMethod;

    expect(authMethodLabel(missingType)).toBe("Sign in with SSO");
    expect(authMethodLabel(objectType)).toBe("Sign in with SSO");
  });

  it("does not mark failed cookie session checks as authenticated", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      json: async () => ({}),
    });

    await expect(checkSession()).resolves.toBe(false);

    expect(isAuthenticated()).toBe(false);
    expect(withAuthHeaders()).not.toHaveProperty("X-CSRF-Token");
  });

  it.each([
    { role: "viewer" as const, canRunner: false },
    { role: "runner" as const, canRunner: true },
    { role: "operator" as const, canRunner: true },
    { role: "admin" as const, canRunner: true },
  ])("usePrincipal reflects a $role whoami response", async ({ role, canRunner }) => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => whoami(role),
    });
    const { result } = renderHook(() => usePrincipal());

    await act(async () => {
      await expect(apiKeyLogin("csk_live_secret")).resolves.toBe("success");
    });

    expect(mockFetch).toHaveBeenCalledWith("/auth/whoami", {
      credentials: "include",
      headers: { Authorization: "Bearer csk_live_secret" },
    });
    expect(result.current).toMatchObject({
      kind: "api_key",
      subject: "csk_live_good",
      role,
      canRunner,
      isScoped: false,
      scopeKnown: false,
    });
  });

  it.each([
    {
      label: "explicit is_scoped true",
      extra: { is_scoped: true },
      isScoped: true,
      scopeKnown: true,
    },
    {
      label: "non-empty scope.jobs",
      extra: { scope: { jobs: ["billing"] } },
      isScoped: true,
      scopeKnown: true,
    },
    {
      label: "empty scope.jobs",
      extra: { scope: { jobs: [] } },
      isScoped: false,
      scopeKnown: true,
    },
  ])(
    "usePrincipal derives scope from whoami ($label)",
    async ({ extra, isScoped, scopeKnown }) => {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => whoami("runner", extra),
      });
      const { result } = renderHook(() => usePrincipal());

      await act(async () => {
        await expect(apiKeyLogin("csk_live_secret")).resolves.toBe("success");
      });

      expect(result.current).toMatchObject({ role: "runner", isScoped, scopeKnown });
    },
  );

  it("clears the retained principal role on logout", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => whoami("runner"),
    });
    const { result } = renderHook(() => usePrincipal());

    await act(async () => {
      await expect(apiKeyLogin("csk_live_secret")).resolves.toBe("success");
    });
    expect(result.current).toMatchObject({ role: "runner", canRunner: true });

    mockFetch.mockResolvedValueOnce({ ok: true, status: 200 });
    await act(async () => {
      await expect(logout()).resolves.toBeUndefined();
    });
    expect(result.current).toMatchObject({ role: null, canRunner: false });
  });

  it("clears the principal snapshot when auth state is cleared", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => whoami("runner"),
    });
    const { result } = renderHook(() => usePrincipal());

    await act(async () => {
      await apiKeyLogin("csk_live_secret");
    });
    expect(result.current.role).toBe("runner");

    act(() => {
      clearApiKey();
    });

    expect(result.current).toMatchObject({
      role: null,
      canRunner: false,
      isScoped: false,
    });
  });
});
