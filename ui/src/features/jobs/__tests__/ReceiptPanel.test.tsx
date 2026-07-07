import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Receipt } from "@/lib/api";
import { api } from "@/lib/api";
import { ReceiptPanel } from "../ReceiptPanel";

vi.mock("sonner", () => ({
  toast: {
    error: vi.fn(),
    success: vi.fn(),
  },
}));

vi.mock("@/lib/api", () => ({
  api: {
    getReceipt: vi.fn(),
    postVerify: vi.fn(),
  },
}));

const alarmClassPattern = /(?:bg|text|border)-(?:destructive|warning|danger)/;

describe("ReceiptPanel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders degraded unverifiable receipt state as informational", async () => {
    vi.mocked(api.getReceipt).mockResolvedValue(degradedReceipt);

    renderWithQueryClient(<ReceiptPanel jobId="job-1" runId="run-1" />);

    const status = await screen.findByTestId("receipt-degraded-status");
    expect(status).toHaveTextContent("degraded-unverifiable");
    expectInformational(status);

    const digestPinned = screen.getByTestId("receipt-task-digest-pinned-marker");
    expect(digestPinned).toHaveTextContent("digest_pinned=false");
    expectInformational(digestPinned);

    const unverifiable = screen.getByTestId("receipt-task-unverifiable-marker");
    expect(unverifiable).toHaveTextContent("unverifiable");
    expectInformational(unverifiable);

    expectInformational(screen.getByTestId("receipt-task-degraded-reason"));
    expectInformational(screen.getByTestId("receipt-unverifiable-summary"));
  });
});

function renderWithQueryClient(component: ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });

  return render(
    <QueryClientProvider client={queryClient}>
      {component}
    </QueryClientProvider>,
  );
}

function expectInformational(element: HTMLElement) {
  expect(element.className).not.toMatch(alarmClassPattern);
}

const degradedReceipt: Receipt = {
  receipt_version: 1,
  run_id: "run-1",
  job_id: "job-1",
  job_alias: "demo",
  git_commit: "abc123",
  manifest_content_hash: "manifest",
  tasks: [
    {
      task_name: "extract",
      identity_hash: "identity",
      image: "busybox:1.36.1",
      digest_pinned: false,
      degraded: true,
      degraded_reason: "image not digest-pinned",
    },
  ],
  degraded: true,
  degraded_tasks: ["extract"],
  receipt_digest: "sha256:receipt",
};
