import { beforeEach, describe, expect, it, vi } from "vitest";
import { clearApiKey, isAuthenticated, onAuthChange, setApiKey, withAuthHeaders } from "@/lib/auth";

describe("auth", () => {
  beforeEach(() => {
    clearApiKey();
    vi.restoreAllMocks();
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
});
