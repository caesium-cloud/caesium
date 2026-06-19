import { fireEvent, render, screen } from "@testing-library/react";
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

describe("JobDefsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    vi.mocked(api.lintJobDef).mockResolvedValue({
      errors: [],
      warnings: [],
      summary: { steps: "2 steps" },
    });
    vi.mocked(api.diffJobDef).mockResolvedValue({
      added: [],
      removed: [],
      modified: [],
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
});
