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
    mockFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ enabled: true }),
    });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("Enter your API key to continue")).toBeInTheDocument();
    });
    expect(mockFetch).toHaveBeenCalledWith("/auth/status");
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
    mockFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ enabled: true }),
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
});
