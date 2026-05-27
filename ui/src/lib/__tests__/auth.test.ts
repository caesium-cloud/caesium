import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  checkSession,
  clearApiKey,
  isAuthenticated,
  onAuthChange,
  setApiKey,
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
