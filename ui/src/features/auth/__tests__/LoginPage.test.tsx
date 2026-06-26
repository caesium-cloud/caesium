import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { LoginPage } from "../LoginPage";
import { clearApiKey, isAuthenticated } from "@/lib/auth";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

describe("LoginPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearApiKey();
  });

  it("rejects keys without the expected prefix", async () => {
    render(<LoginPage onLogin={vi.fn()} />);

    fireEvent.change(screen.getByPlaceholderText("csk_live_..."), {
      target: { value: "invalid" },
    });
    fireEvent.click(screen.getByText("Sign In"));

    expect(await screen.findByText("Invalid key format (expected csk_... prefix)")).toBeInTheDocument();
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("shows an auth error when the server returns 401", async () => {
    mockFetch.mockResolvedValue({ status: 401 });

    render(<LoginPage onLogin={vi.fn()} />);

    fireEvent.change(screen.getByPlaceholderText("csk_live_..."), {
      target: { value: "csk_live_bad" },
    });
    fireEvent.click(screen.getByText("Sign In"));

    expect(await screen.findByText("Invalid or expired API key")).toBeInTheDocument();
    expect(isAuthenticated()).toBe(false);
  });

  it("renders advertised browser redirect methods and navigates with returnTo", () => {
    const navigate = vi.fn();

    render(
      <LoginPage
        methods={[
          { type: "api-key" },
          {
            type: "oidc",
            id: "corp-oidc",
            label: "Sign in with Corp SSO",
            loginUrl: "/auth/sso/oidc/login",
            mode: "redirect",
          },
          {
            type: "saml",
            id: "corp-saml",
            label: "Sign in with Corp SAML",
            loginUrl: "/auth/sso/saml/login",
            mode: "redirect",
          },
          {
            type: "ldap",
            mode: "credential",
            id: "corp-ldap",
            label: "Sign in with LDAP",
            loginUrl: "/auth/sso/ldap/login",
          },
        ]}
        navigate={navigate}
        onLogin={vi.fn()}
        returnTo={() => "/runs?status=failed#latest"}
      />,
    );

    expect(screen.getByRole("button", { name: "Sign in with Corp SSO" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in with Corp SAML" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Username")).toHaveFocus();
    expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Sign in with Corp SAML" }));

    expect(navigate).toHaveBeenCalledWith(
      "/auth/sso/saml/login?returnTo=%2Fruns%3Fstatus%3Dfailed%23latest",
    );
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("posts LDAP credentials with cookies and calls onLogin after the session is visible", async () => {
    const onLogin = vi.fn();
    mockFetch
      .mockResolvedValueOnce({ status: 204, ok: true })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          kind: "user",
          subject: "ops@example.com",
          role: "viewer",
          csrf_token: "csrf-secret",
        }),
      });

    render(
      <LoginPage
        methods={[
          {
            type: "ldap",
            mode: "credential",
            id: "corp-ldap",
            label: "Sign in with LDAP",
            loginUrl: "/auth/sso/ldap/login",
          },
        ]}
        onLogin={onLogin}
      />,
    );

    fireEvent.change(screen.getByPlaceholderText("Username"), {
      target: { value: " ada " },
    });
    fireEvent.change(screen.getByPlaceholderText("Password"), {
      target: { value: "secret" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Sign in with LDAP" }));

    await waitFor(() => {
      expect(onLogin).toHaveBeenCalledTimes(1);
    });

    expect(mockFetch).toHaveBeenNthCalledWith(1, "/auth/sso/ldap/login", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: "ada", password: "secret" }),
    });
    expect(mockFetch).toHaveBeenNthCalledWith(2, "/auth/whoami", { credentials: "include" });
    expect(isAuthenticated()).toBe(true);
  });

  it("shows an LDAP denial without authenticating", async () => {
    mockFetch.mockResolvedValueOnce({ status: 403, ok: false });

    render(
      <LoginPage
        methods={[
          {
            type: "ldap",
            mode: "credential",
            label: "Corporate Directory",
            loginUrl: "/auth/sso/ldap/login",
          },
        ]}
        onLogin={vi.fn()}
      />,
    );

    fireEvent.change(screen.getByPlaceholderText("Username"), {
      target: { value: "ada" },
    });
    fireEvent.change(screen.getByPlaceholderText("Password"), {
      target: { value: "secret" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Corporate Directory" }));

    expect(await screen.findByText("Corporate Directory login is not allowed for this account")).toBeInTheDocument();
    expect(isAuthenticated()).toBe(false);
  });

  it("uses the credential method label in generic login errors", async () => {
    mockFetch.mockResolvedValueOnce({ status: 500, ok: false });

    render(
      <LoginPage
        methods={[
          {
            type: "ldap",
            mode: "credential",
            label: "Corporate Directory",
            loginUrl: "/auth/sso/ldap/login",
          },
        ]}
        onLogin={vi.fn()}
      />,
    );

    fireEvent.change(screen.getByPlaceholderText("Username"), {
      target: { value: "ada" },
    });
    fireEvent.change(screen.getByPlaceholderText("Password"), {
      target: { value: "secret" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Corporate Directory" }));

    expect(await screen.findByText("Unable to sign in with Corporate Directory")).toBeInTheDocument();
    expect(isAuthenticated()).toBe(false);
  });

  it("stores the api key and calls onLogin on success", async () => {
    const onLogin = vi.fn();
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        kind: "api_key",
        subject: "csk_live_good",
        role: "runner",
      }),
    });

    render(<LoginPage onLogin={onLogin} />);

    fireEvent.change(screen.getByPlaceholderText("csk_live_..."), {
      target: { value: "csk_live_good" },
    });
    fireEvent.click(screen.getByText("Sign In"));

    await waitFor(() => {
      expect(onLogin).toHaveBeenCalledTimes(1);
    });

    expect(mockFetch).toHaveBeenCalledWith("/auth/whoami", {
      credentials: "include",
      headers: { Authorization: "Bearer csk_live_good" },
    });
    expect(isAuthenticated()).toBe(true);
  });
});
