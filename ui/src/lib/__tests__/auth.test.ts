import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  authMethodKey,
  authMethodLabel,
  checkSession,
  clearApiKey,
  currentReturnTo,
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

  it("identifies browser redirect auth methods and labels OIDC and SAML", () => {
    const oidc: RedirectAuthMethod = {
      type: "oidc",
      id: "corp-oidc",
      label: "  Sign in with Corp SSO  ",
      loginUrl: "/auth/sso/oidc/login",
    };
    const saml: RedirectAuthMethod = {
      type: "saml",
      loginUrl: "/auth/sso/saml/login",
    };

    expect(isRedirectAuthMethod(oidc)).toBe(true);
    expect(isRedirectAuthMethod(saml)).toBe(true);
    expect(isRedirectAuthMethod({ type: "api-key" })).toBe(false);
    expect(authMethodKey(oidc)).toBe("corp-oidc");
    expect(authMethodKey(saml)).toBe("saml:/auth/sso/saml/login:");
    expect(authMethodLabel(oidc)).toBe("Sign in with Corp SSO");
    expect(authMethodLabel(saml)).toBe("Sign in with SAML");
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
