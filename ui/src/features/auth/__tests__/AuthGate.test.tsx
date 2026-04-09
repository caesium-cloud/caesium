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

  it("renders the login page when the auth probe returns 401", async () => {
    mockFetch.mockResolvedValue({ status: 401 });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("Enter your API key to continue")).toBeInTheDocument();
    });
    expect(mockFetch).toHaveBeenCalledWith("/v1/jobs?limit=1");
  });

  it("renders children when auth is not required", async () => {
    mockFetch.mockResolvedValue({ status: 200 });

    render(
      <AuthGate>
        <div>protected</div>
      </AuthGate>,
    );

    await waitFor(() => {
      expect(screen.getByText("protected")).toBeInTheDocument();
    });
  });

  it("reacts to auth state changes after a 401 probe", async () => {
    mockFetch.mockResolvedValue({ status: 401 });

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
