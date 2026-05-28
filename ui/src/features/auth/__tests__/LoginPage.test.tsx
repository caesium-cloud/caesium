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

  it("renders an advertised OIDC method and navigates with returnTo", () => {
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
          },
        ]}
        navigate={navigate}
        onLogin={vi.fn()}
        returnTo={() => "/runs?status=failed#latest"}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Sign in with Corp SSO" }));

    expect(navigate).toHaveBeenCalledWith(
      "/auth/sso/oidc/login?returnTo=%2Fruns%3Fstatus%3Dfailed%23latest",
    );
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("stores the api key and calls onLogin on success", async () => {
    const onLogin = vi.fn();
    mockFetch.mockResolvedValue({ status: 200 });

    render(<LoginPage onLogin={onLogin} />);

    fireEvent.change(screen.getByPlaceholderText("csk_live_..."), {
      target: { value: "csk_live_good" },
    });
    fireEvent.click(screen.getByText("Sign In"));

    await waitFor(() => {
      expect(onLogin).toHaveBeenCalledTimes(1);
    });

    expect(mockFetch).toHaveBeenCalledWith("/v1/jobs?limit=1", {
      headers: { Authorization: "Bearer csk_live_good" },
    });
    expect(isAuthenticated()).toBe(true);
  });
});
