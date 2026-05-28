import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  authMethodKey,
  authMethodLabel,
  checkSession,
  clearApiKey,
  credentialLogin,
  currentReturnTo,
  isCredentialAuthMethod,
  isRedirectAuthMethod,
  isAuthenticated,
  onAuthChange,
  type RedirectAuthMethod,
  setApiKey,
  ssoLoginUrl,
  withAuthHeaders,
} from "@/lib/auth";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

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
      json: async () => ({ csrf_token: "csrf-secret" }),
    });

    await expect(checkSession()).resolves.toBe(true);

    expect(mockFetch).toHaveBeenCalledWith("/auth/whoami", { credentials: "include" });
    expect(isAuthenticated()).toBe(true);
    expect(withAuthHeaders()).toMatchObject({
      "X-CSRF-Token": "csrf-secret",
    });
    expect(withAuthHeaders()).not.toHaveProperty("Authorization");
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
        json: async () => ({ csrf_token: "csrf-secret" }),
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
});
