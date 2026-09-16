import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Trigger } from "@/lib/api";
import { api } from "@/lib/api";
import { TriggersPage } from "../TriggersPage";

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    api: {
      getTriggers: vi.fn(),
      getSystemFeatures: vi.fn(),
      createTrigger: vi.fn(),
      updateTrigger: vi.fn(),
    },
  };
});

const httpTrigger: Trigger = {
  id: "trigger-http-1",
  alias: "deploy-hook",
  type: "http",
  configuration: JSON.stringify({ path: "/hooks/deploy" }),
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <TriggersPage />
    </QueryClientProvider>,
  );
}

describe("TriggersPage icon button names", () => {
  beforeEach(() => {
    vi.mocked(api.getTriggers).mockReset();
    vi.mocked(api.getSystemFeatures).mockReset();
    vi.mocked(api.getTriggers).mockResolvedValue([httpTrigger]);
    vi.mocked(api.getSystemFeatures).mockResolvedValue({
      database_console_enabled: false,
      log_console_enabled: false,
      agent_remediation_enabled: false,
      freshness_enabled: false,
      contract_enforcement_enabled: false,
      external_url: "https://caesium.example",
    });
  });

  it("names the copy and expand icon buttons", async () => {
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("trigger-card")).toBeInTheDocument();
    });

    expect(screen.getByRole("button", { name: "Copy webhook URL" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Expand trigger details" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
  });
});
