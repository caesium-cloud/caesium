import { beforeEach, describe, expect, it, vi } from "vitest";
import { clearApiKey, getApiKey, isAuthenticated, onAuthChange, setApiKey } from "@/lib/auth";

describe("auth", () => {
  beforeEach(() => {
    clearApiKey();
    vi.restoreAllMocks();
  });

  it("stores and clears the api key in memory", () => {
    expect(isAuthenticated()).toBe(false);

    setApiKey("csk_live_secret");
    expect(getApiKey()).toBe("csk_live_secret");
    expect(isAuthenticated()).toBe(true);

    clearApiKey();
    expect(getApiKey()).toBeNull();
    expect(isAuthenticated()).toBe(false);
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
