import { act, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { JobDefsPage } from "../JobDefsPage";

vi.mock("@/lib/api", () => ({
  api: {
    lintJobDef: vi.fn(),
    diffJobDef: vi.fn(),
    applyJobDef: vi.fn(),
  },
}));

vi.mock("sonner", () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
    warning: vi.fn(),
  },
}));

vi.mock("@uiw/react-codemirror", () => ({
  default: ({
    value,
    onChange,
  }: {
    value: string;
    onChange?: (value: string) => void;
  }) => (
    <textarea
      aria-label="job.yaml editor"
      value={value}
      onChange={(event) => onChange?.(event.currentTarget.value)}
    />
  ),
}));

vi.mock("@codemirror/lang-yaml", () => ({
  yaml: () => [],
}));

vi.mock("@codemirror/lint", () => ({
  linter: () => [],
}));

vi.mock("@codemirror/view", () => ({
  EditorView: {
    theme: () => [],
  },
}));

import { api } from "@/lib/api";

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

async function settleLintAndDiff() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(350);
  });
}

function openDiffTab() {
  const tab = screen.getByRole("tab", { name: /Diff vs server/i });
  fireEvent.pointerDown(tab, { button: 0, ctrlKey: false });
  fireEvent.click(tab);
  fireEvent.keyDown(tab, { key: "Enter", code: "Enter" });
}

const breakingFinding = {
  edgeId: "inferred:producer:consumer",
  edgeClass: "inferred" as const,
  from: "job:producer",
  to: "job:consumer",
  verdict: "breaking" as const,
  key: "customer_id",
  path: "trigger.configuration.paramMapping.customer",
  detail: "paramMapping customer references output key customer_id, but that key is missing",
  consumer_team: "reporting",
};

describe("JobDefsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    vi.mocked(api.lintJobDef).mockResolvedValue({
      errors: [],
      warnings: [],
      summary: { steps: 2 },
    });
    vi.mocked(api.diffJobDef).mockResolvedValue({
      added: [],
      removed: [],
      modified: [],
    });
    vi.mocked(api.applyJobDef).mockResolvedValue({
      applied: 1,
      contract_warnings: [],
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders volume and workload identity authoring support", () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });

    expect(screen.getByText("Runtime support")).toBeInTheDocument();
    expect(screen.getByText("1 declared")).toBeInTheDocument();
    expect(screen.getByText("2 mounts across 2 steps.")).toBeInTheDocument();
    expect(screen.getByText("caesium-planner, caesium-deployer")).toBeInTheDocument();
    expect(screen.getByText("Includes pod annotations and token setting.")).toBeInTheDocument();
    expect(screen.getAllByText("Volumes").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Kubernetes identity").length).toBeGreaterThanOrEqual(1);
  });

  it("ships a starter manifest with volumes and Kubernetes identity fields", () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });

    const editor = screen.getByLabelText("job.yaml editor") as HTMLTextAreaElement;
    expect(editor.value).toContain("volumes:");
    expect(editor.value).toContain("volumeMounts:");
    expect(editor.value).toContain("serviceAccountName: caesium-deployer");
    expect(editor.value).toContain("automountServiceAccountToken: true");
    expect(editor.value).toContain("kubernetes:");
    expect(editor.value).toContain("pvc: ci-shared-rwx");
  });

  it("updates runtime hints as the manifest changes", () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });

    fireEvent.change(screen.getByLabelText("job.yaml editor"), {
      target: {
        value: `apiVersion: v1
kind: Job
metadata:
  alias: simple
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: run
    image: alpine:3.23
`,
      },
    });

    expect(screen.getByText("No volumes declared")).toBeInTheDocument();
    expect(screen.getByText("No service account")).toBeInTheDocument();
  });

  it("renders contract finding badges from the diff response", async () => {
    vi.mocked(api.diffJobDef).mockResolvedValue({
      added: [],
      removed: [],
      modified: [
        {
          alias: "producer",
          diff: "- old\n+ new",
          contractFindings: [
            breakingFinding,
            {
              ...breakingFinding,
              edgeId: "declared:producer:consumer:lake/customers",
              edgeClass: "declared",
              verdict: "compatible",
              dataset: { namespace: "lake", name: "customers" },
              detail: "optional field added",
            },
            {
              ...breakingFinding,
              edgeId: "inferred:producer:consumer:unknown",
              verdict: "unknown",
              key: "order_id",
              detail: "consumer requirement cannot be proven",
            },
          ],
        },
      ],
    });

    render(<JobDefsPage />, { wrapper: createWrapper() });
    await settleLintAndDiff();
    openDiffTab();

    const badges = screen.getAllByTestId("contract-finding-badge");
    expect(badges).toHaveLength(3);
    expect(badges[0]).toHaveAttribute("data-verdict", "breaking");
    expect(screen.getByText("producer.output.customer_id")).toBeInTheDocument();
    expect(screen.getByText("lake/customers")).toBeInTheDocument();
    expect(screen.getAllByText("consumer: consumer").length).toBeGreaterThan(0);
    expect(screen.getAllByText("team: reporting").length).toBeGreaterThan(0);
    expect(badges[0].getAttribute("title")).toContain("missing");
  });

  it("requires an acknowledgement reason for breaking findings and passes it to apply", async () => {
    vi.mocked(api.diffJobDef).mockResolvedValue({
      added: [],
      removed: [],
      modified: [
        {
          alias: "producer",
          diff: "- old\n+ new",
          contractFindings: [breakingFinding],
        },
      ],
    });

    render(<JobDefsPage />, { wrapper: createWrapper() });
    await settleLintAndDiff();

    const applyButton = screen.getByRole("button", { name: /Apply definition/i });
    expect(applyButton).toBeDisabled();

    fireEvent.change(screen.getByLabelText("Breaking change acknowledgement reason"), {
      target: { value: "customer migration accepted" },
    });
    expect(applyButton).toBeEnabled();

    await act(async () => {
      fireEvent.click(applyButton);
      await Promise.resolve();
    });
    expect(api.applyJobDef).toHaveBeenCalledWith(expect.any(String), {
      dataset: "producer.output.customer_id",
      reason: "customer migration accepted",
    });
  });
});
