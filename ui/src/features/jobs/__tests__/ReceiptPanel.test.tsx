import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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

vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
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

  it("keeps the committed command and renders image and command drift from the verify response", async () => {
    const command = '["sh","-c","echo a"]';
    const receipt: Receipt = {
      ...degradedReceipt,
      degraded: false,
      degraded_tasks: undefined,
      tasks: [{ ...degradedReceipt.tasks[0], command, digest_pinned: true, degraded: false, degraded_reason: undefined }],
    };
    vi.mocked(api.getReceipt).mockResolvedValue(receipt);
    vi.mocked(api.postVerify).mockResolvedValue({
      run_id: "run-1",
      match: false,
      degraded: false,
      expected_digest: "receipt-sha",
      actual_digest: "receipt-sha",
      drifts: [
        {
          kind: "image_changed",
          task: "extract",
          expected: "busybox:1.36.1",
          actual: "alpine:3.23",
          detail: "task image reference on the completed run differs from the committed receipt",
        },
        {
          kind: "command_changed",
          task: "extract",
          expected: command,
          actual: '["sh","-c","echo b"]',
          detail: "task command on the completed run differs from the committed receipt",
        },
      ],
      rederived: receipt,
    });

    renderWithQueryClient(<ReceiptPanel jobId="job-1" runId="run-1" />);

    expect(await screen.findByTestId("receipt-task-command")).toHaveTextContent(command);
    fireEvent.change(screen.getByTestId("receipt-verify-input"), {
      target: { value: JSON.stringify(receipt) },
    });
    fireEvent.click(screen.getByTestId("receipt-verify-submit"));

    await waitFor(() => expect(api.postVerify).toHaveBeenCalled());
    expect(vi.mocked(api.postVerify).mock.calls[0]?.[0].tasks[0]?.command).toBe(command);
    expect(await screen.findByTestId("receipt-verify-verdict")).toHaveTextContent("image_changed");
    const kinds = screen.getAllByTestId("receipt-verify-drift-kind").map((element) => element.textContent);
    expect(kinds).toEqual(expect.arrayContaining(["image_changed", "command_changed"]));
    expect(screen.getAllByText(command).length).toBeGreaterThan(0);
    expect(screen.getByText('["sh","-c","echo b"]')).toBeInTheDocument();
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
