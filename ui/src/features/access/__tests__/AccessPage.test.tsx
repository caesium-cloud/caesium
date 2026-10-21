import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type AuthUser } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { AccessPage } from "../AccessPage";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

async function login(body: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: true, json: async () => ({ kind: "api_key", subject: "k", is_scoped: false, ...body }) }),
  );
  expect(await apiKeyLogin("csk_live_access")).toBe("success");
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <AccessPage />
    </QueryClientProvider>,
  );
}

const user = (over: Partial<AuthUser> = {}): AuthUser => ({
  id: "u-1",
  issuer: "https://idp",
  subject: "sub-1",
  email: "ada@example.com",
  email_verified: true,
  display_name: "Ada",
  groups: [],
  last_login_role: "viewer",
  created_at: "2026-01-01T00:00:00Z",
  last_login_at: "2026-02-01T00:00:00Z",
  disabled_at: null,
  ...over,
});

const namespaces = {
  source: "file",
  revision: "abcdef0123456789",
  loaded_at: "2026-01-01T00:00:00Z",
  last_reload: null,
  namespaces: [
    { name: "default", secrets: null },
    { name: "marketing", secrets: { allow: ["env/MARKETING_*"] }, quotas: { maxConcurrentRuns: 1 } },
  ],
};

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  vi.spyOn(api, "getAuthNamespaces").mockResolvedValue(namespaces);
  vi.spyOn(api, "getAuthPolicy").mockResolvedValue({
    source: "file",
    revision: "abcdef0123456789",
    loaded_at: "2026-01-01T00:00:00Z",
    warnings: [],
    last_reload: { at: "2026-01-02T00:00:00Z", trigger: "api", ok: true, changed: true },
  });
  vi.spyOn(api, "getAuthUsers").mockResolvedValue({ users: [user()], total: 1, nextOffset: null });
});
afterEach(() => vi.unstubAllGlobals());

describe("AccessPage", () => {
  it("explains that authentication is disabled and fetches nothing", async () => {
    show();
    expect(await screen.findByTestId("access-auth-disabled")).toBeInTheDocument();
    expect(api.getAuthNamespaces).not.toHaveBeenCalled();
    expect(api.getAuthUsers).not.toHaveBeenCalled();
    expect(screen.queryByTestId("access-users")).not.toBeInTheDocument();
  });

  it("shows a cluster admin its grants, namespaces, policy and users", async () => {
    await login({ role: "admin", cluster_role: "admin", grants: { "*": "admin" }, namespaces: ["*"] });
    show();
    expect(screen.getByTestId("access-cluster-role")).toHaveTextContent("admin");
    expect(screen.getByTestId("access-grant-row")).toHaveTextContent("all namespaces");
    expect(await screen.findAllByTestId("access-namespace-row")).toHaveLength(2);
    expect(screen.getByTestId("access-namespaces")).toHaveTextContent("env/MARKETING_*");
    expect(await screen.findByTestId("access-policy-revision")).toHaveTextContent("abcdef0123456789");
    expect(screen.getByTestId("access-policy-last-reload")).toHaveTextContent("reloaded (api)");
    expect(await screen.findByTestId("access-user-row")).toHaveTextContent("ada@example.com");
  });

  it("shows a cluster viewer namespaces but no policy or users", async () => {
    await login({ role: "viewer", cluster_role: "viewer", grants: { "*": "viewer" }, namespaces: ["*"] });
    show();
    expect(await screen.findAllByTestId("access-namespace-row")).toHaveLength(2);
    expect(screen.queryByTestId("access-policy")).not.toBeInTheDocument();
    expect(screen.queryByTestId("access-users")).not.toBeInTheDocument();
    expect(api.getAuthPolicy).not.toHaveBeenCalled();
    expect(api.getAuthUsers).not.toHaveBeenCalled();
  });

  it("shows a namespace-confined principal only its own access", async () => {
    await login({ role: "", cluster_role: "", grants: { marketing: "operator" }, namespaces: ["marketing"] });
    show();
    const rows = screen.getAllByTestId("access-grant-row");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("marketing");
    expect(rows[0]).toHaveTextContent("operator");
    expect(screen.getByTestId("access-cluster-role")).toHaveTextContent("none");
    expect(screen.queryByTestId("access-namespaces")).not.toBeInTheDocument();
    expect(screen.queryByTestId("access-users")).not.toBeInTheDocument();
    expect(api.getAuthNamespaces).not.toHaveBeenCalled();
    expect(api.getAuthPolicy).not.toHaveBeenCalled();
  });

  it("keeps a job-scoped key to its own access", async () => {
    await login({ role: "viewer", cluster_role: "viewer", grants: { "*": "viewer" }, namespaces: ["*"], is_scoped: true });
    show();
    expect(screen.getByTestId("access-scoped-note")).toBeInTheDocument();
    expect(screen.queryByTestId("access-namespaces")).not.toBeInTheDocument();
    expect(api.getAuthNamespaces).not.toHaveBeenCalled();
  });

  describe("user administration", () => {
    beforeEach(() =>
      login({ role: "admin", cluster_role: "admin", grants: { "*": "admin" }, namespaces: ["*"] }),
    );

    it("disables a user only after confirmation, then refetches", async () => {
      const disable = vi.spyOn(api, "disableAuthUser").mockResolvedValue({
        user: user({ disabled_at: "2026-03-01T00:00:00Z" }),
        changed: true,
        sessions_revoked: 2,
      });
      show();
      const row = await screen.findByTestId("access-user-row");
      fireEvent.click(within(row).getByRole("button", { name: "Disable" }));
      expect(disable).not.toHaveBeenCalled();
      fireEvent.click(await screen.findByTestId("access-confirm"));
      await waitFor(() => expect(disable).toHaveBeenCalledWith("u-1"));
      await waitFor(() => expect(api.getAuthUsers).toHaveBeenCalledTimes(2));
    });

    it("offers Enable for a disabled user", async () => {
      vi.mocked(api.getAuthUsers).mockResolvedValue({
        users: [user({ disabled_at: "2026-03-01T00:00:00Z" })],
        total: 1,
        nextOffset: null,
      });
      const enable = vi.spyOn(api, "enableAuthUser").mockResolvedValue({ user: user(), changed: true, sessions_revoked: 0 });
      show();
      const row = await screen.findByTestId("access-user-row");
      expect(row).toHaveTextContent("disabled");
      fireEvent.click(within(row).getByRole("button", { name: "Enable" }));
      fireEvent.click(await screen.findByTestId("access-confirm"));
      await waitFor(() => expect(enable).toHaveBeenCalledWith("u-1"));
    });

    it("logs a user out everywhere", async () => {
      const revoke = vi.spyOn(api, "revokeAuthUserSessions").mockResolvedValue({ user_id: "u-1", sessions_revoked: 3 });
      show();
      const row = await screen.findByTestId("access-user-row");
      fireEvent.click(within(row).getByRole("button", { name: "Log out everywhere" }));
      fireEvent.click(await screen.findByTestId("access-confirm"));
      await waitFor(() => expect(revoke).toHaveBeenCalledWith("u-1"));
    });

    it("surfaces a refused action (self-disable 409) inside the dialog", async () => {
      vi.spyOn(api, "disableAuthUser").mockRejectedValue(new Error("cannot disable the user making this request"));
      show();
      const row = await screen.findByTestId("access-user-row");
      fireEvent.click(within(row).getByRole("button", { name: "Disable" }));
      fireEvent.click(await screen.findByTestId("access-confirm"));
      expect(await screen.findByRole("alert")).toHaveTextContent("cannot disable the user making this request");
    });

    it("pages through the next-offset cursor", async () => {
      vi.mocked(api.getAuthUsers)
        .mockResolvedValueOnce({ users: [user()], total: 2, nextOffset: 1 })
        .mockResolvedValueOnce({ users: [user({ id: "u-2", email: "bo@example.com" })], total: 2, nextOffset: null });
      show();
      await screen.findByTestId("access-user-row");
      fireEvent.click(screen.getByRole("button", { name: "Load more" }));
      await waitFor(() => expect(screen.getAllByTestId("access-user-row")).toHaveLength(2));
      expect(api.getAuthUsers).toHaveBeenLastCalledWith(1);
      expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
    });
  });
});
