import { act, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AuthGate } from "../AuthGate";
import { clearApiKey, setApiKey } from "@/lib/auth";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

describe("AuthGate", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearApiKey();
  });

  it("renders the login page when the auth status endpoint says auth is enabled", async () => {
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ enabled: true }),
      })
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({}),
      });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("Enter your API key to continue")).toBeInTheDocument();
    });
    expect(mockFetch).toHaveBeenNthCalledWith(1, "/auth/status");
    expect(mockFetch).toHaveBeenNthCalledWith(2, "/auth/whoami", { credentials: "include" });
  });

  it("renders advertised OIDC methods on the login page", async () => {
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          enabled: true,
          methods: [
            { type: "api-key" },
            { type: "oidc", loginUrl: "/auth/sso/oidc/login" },
          ],
        }),
      })
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({}),
      });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Sign in with OIDC" })).toBeInTheDocument();
    });
  });

  it("renders children when auth is not required", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ enabled: false }),
    });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("protected")).toBeInTheDocument();
    });
  });

  it("reacts to auth state changes after an enabled auth probe", async () => {
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ enabled: true }),
      })
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({}),
      });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("Enter your API key to continue")).toBeInTheDocument();
    });

    await act(async () => {
      setApiKey("csk_live_secret");
    });

    await waitFor(() => {
      expect(screen.getByText("protected")).toBeInTheDocument();
    });
  });

  it("renders children when an existing cookie session is valid", async () => {
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ enabled: true }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ kind: "user", email: "ops@example.com", csrf_token: "csrf-secret" }),
      });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("protected")).toBeInTheDocument();
    });
    expect(screen.queryByText("Enter your API key to continue")).not.toBeInTheDocument();
    expect(mockFetch).toHaveBeenNthCalledWith(2, "/auth/whoami", { credentials: "include" });
  });
});
