import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { JobDefsPage } from "../JobDefsPage";

const codeMirrorHarness = vi.hoisted(() => {
  const state: {
    editorText: string;
    delayOnChange: boolean;
    pendingOnChange?: () => void;
  } = {
    editorText: "",
    delayOnChange: false,
    pendingOnChange: undefined,
  };
  const view = {
    state: {
      doc: {
        toString: () => state.editorText,
      },
    },
  };
  return { state, view };
});

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
    onCreateEditor,
    onUpdate,
  }: {
    value: string;
    onChange?: (value: string) => void;
    onCreateEditor?: (view: typeof codeMirrorHarness.view, state: typeof codeMirrorHarness.view.state) => void;
    onUpdate?: (update: { docChanged: boolean; state: typeof codeMirrorHarness.view.state }) => void;
  }) => {
    codeMirrorHarness.state.editorText = value;
    onCreateEditor?.(codeMirrorHarness.view, codeMirrorHarness.view.state);

    return (
      <textarea
        aria-label="job.yaml editor"
        value={value}
        onChange={(event) => {
          const nextValue = event.currentTarget.value;
          codeMirrorHarness.state.editorText = nextValue;
          onUpdate?.({ docChanged: true, state: codeMirrorHarness.view.state });

          const emitChange = () => onChange?.(nextValue);
          if (codeMirrorHarness.state.delayOnChange) {
            codeMirrorHarness.state.pendingOnChange = emitChange;
            return;
          }
          emitChange();
        }}
      />
    );
  },
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
import { toast } from "sonner";

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

function singleJobYaml(alias: string) {
  return `apiVersion: v1
kind: Job
metadata:
  alias: ${alias}
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: run
    image: alpine:3.23
`;
}

async function flushUntil(assert: () => void, attempts = 30) {
  let lastError: unknown;
  for (let i = 0; i < attempts; i++) {
    try {
      assert();
      return;
    } catch (err) {
      lastError = err;
    }
    await act(async () => {
      await Promise.resolve();
    });
  }
  throw lastError;
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
    codeMirrorHarness.state.editorText = "";
    codeMirrorHarness.state.delayOnChange = false;
    codeMirrorHarness.state.pendingOnChange = undefined;
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

  it("diffs the current editor document when the tab opens before onChange settles", async () => {
    codeMirrorHarness.state.delayOnChange = true;
    const nextYaml = `apiVersion: v1
kind: Job
metadata:
  alias: race-producer
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: export
    image: alpine:3.23
`;

    render(<JobDefsPage />, { wrapper: createWrapper() });
    await settleLintAndDiff();
    vi.mocked(api.lintJobDef).mockClear();
    vi.mocked(api.diffJobDef).mockClear();

    fireEvent.change(screen.getByLabelText("job.yaml editor"), {
      target: { value: nextYaml },
    });
    openDiffTab();

    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(codeMirrorHarness.state.pendingOnChange).toBeTypeOf("function");
    expect(api.lintJobDef).toHaveBeenCalledWith(nextYaml);
    expect(api.diffJobDef).toHaveBeenCalledWith(nextYaml);
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

  it("excludes prune-only removals from the pending apply preview and badge", async () => {
    const addedAlias = "qa-diff-scope";
    const unrelated = ["cron-nightly", "http-ingest", "k8s-deploy"];
    vi.mocked(api.diffJobDef).mockResolvedValue({
      added: [{ alias: addedAlias }],
      removed: unrelated.map((alias) => ({ alias })),
      modified: [],
    });

    render(<JobDefsPage />, { wrapper: createWrapper() });
    fireEvent.change(screen.getByLabelText("job.yaml editor"), {
      target: { value: singleJobYaml(addedAlias) },
    });
    await settleLintAndDiff();
    openDiffTab();

    expect(screen.getByTestId("diff-pending-summary")).toHaveTextContent("1 change pending apply");
    expect(screen.queryByText(/4 changes pending apply/i)).not.toBeInTheDocument();
    expect(screen.getByTestId("diff-tab-badge")).toHaveTextContent("1");
    expect(screen.getByTestId("diff-tab-badge")).not.toHaveTextContent("4");
    expect(screen.queryByText("Job will be deleted (if prune enabled)")).not.toBeInTheDocument();
    expect(screen.getByTestId("diff-pending-add")).toHaveAttribute("data-alias", addedAlias);
    expect(screen.getByText("Job will be created")).toBeInTheDocument();

    const pruneSection = screen.getByTestId("diff-prune-candidates");
    expect(pruneSection).toHaveTextContent(
      "3 jobs on the server are not in this editor (unchanged unless you prune via CLI)",
    );
    for (const alias of unrelated) {
      expect(screen.getByTestId("diff-prune-candidates")).toHaveTextContent(alias);
    }
  });

  it("recomputes the diff after apply so the applied job is no longer a pending add", async () => {
    const addedAlias = "qa-diff-scope";
    const unrelated = [{ alias: "cron-nightly" }, { alias: "http-ingest" }];
    const pendingDiff = {
      added: [{ alias: addedAlias }],
      removed: unrelated,
      modified: [],
    };
    const postApplyDiff = {
      added: [],
      removed: unrelated,
      modified: [],
    };
    let applied = false;
    vi.mocked(api.diffJobDef).mockImplementation(async () => (
      applied ? postApplyDiff : pendingDiff
    ));
    vi.mocked(api.applyJobDef).mockImplementation(async (yaml: string) => {
      expect(yaml).toContain(`alias: ${addedAlias}`);
      applied = true;
      return { applied: 1, contract_warnings: [] };
    });

    render(<JobDefsPage />, { wrapper: createWrapper() });
    fireEvent.change(screen.getByLabelText("job.yaml editor"), {
      target: { value: singleJobYaml(addedAlias) },
    });
    await settleLintAndDiff();
    openDiffTab();
    await flushUntil(() => {
      expect(screen.getByTestId("diff-pending-add")).toHaveAttribute("data-alias", addedAlias);
      expect(screen.getByRole("button", { name: /Apply definition/i })).toBeEnabled();
    });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Apply definition/i }));
    });
    await flushUntil(() => {
      expect(api.applyJobDef).toHaveBeenCalled();
      expect(screen.queryByTestId("diff-pending-add")).not.toBeInTheDocument();
    });

    expect(screen.getByTestId("diff-pending-summary")).toHaveTextContent("0 changes pending apply");
    expect(screen.queryByTestId("diff-tab-badge")).not.toBeInTheDocument();
    expect(screen.getByText("No changes pending for the definitions in this editor.")).toBeInTheDocument();
    expect(screen.queryByText("Job will be created")).not.toBeInTheDocument();
    expect(screen.getByTestId("diff-prune-candidates")).toHaveTextContent(
      "2 jobs on the server are not in this editor (unchanged unless you prune via CLI)",
    );
  });

  it("opens the file chooser from Upload and loads YAML into the editor", async () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });

    const input = screen.getByTestId("jobdefs-upload-input") as HTMLInputElement;
    const clickSpy = vi.spyOn(input, "click").mockImplementation(() => {});
    fireEvent.click(screen.getByTestId("jobdefs-upload"));
    expect(clickSpy).toHaveBeenCalled();
    clickSpy.mockRestore();

    const uploaded = singleJobYaml("uploaded-job");
    await changeUploadFile(new File([uploaded], "uploaded.job.yaml", { type: "text/yaml" }));

    expect(screen.getByLabelText("job.yaml editor")).toHaveValue(uploaded);
    expect(toast.success).toHaveBeenCalledWith("Loaded uploaded.job.yaml");
  });

  it("confirms before replacing dirty editor contents on upload", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<JobDefsPage />, { wrapper: createWrapper() });

    const editor = screen.getByLabelText("job.yaml editor") as HTMLTextAreaElement;
    fireEvent.change(editor, { target: { value: "dirty: true\n" } });

    const uploaded = singleJobYaml("uploaded-job");
    await changeUploadFile(new File([uploaded], "uploaded.job.yaml", { type: "text/yaml" }));

    expect(confirmSpy).toHaveBeenCalled();
    expect(editor.value).toBe("dirty: true\n");
    expect(toast.success).not.toHaveBeenCalled();

    confirmSpy.mockReturnValue(true);
    await changeUploadFile(new File([uploaded], "uploaded.job.yaml", { type: "text/yaml" }));

    expect(editor.value).toBe(uploaded);
    expect(toast.success).toHaveBeenCalledWith("Loaded uploaded.job.yaml");
    confirmSpy.mockRestore();
  });

  it("rejects binary or oversized uploads", async () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });
    const editor = screen.getByLabelText("job.yaml editor") as HTMLTextAreaElement;
    const original = editor.value;

    await changeUploadFile(new File([new Uint8Array([0, 1, 2, 255, 0])], "blob.bin", { type: "application/octet-stream" }));
    expect(toast.error).toHaveBeenCalledWith("File looks binary and cannot be loaded as YAML");
    expect(editor.value).toBe(original);

    const huge = new File(["apiVersion: v1\n"], "huge.yaml", { type: "text/yaml" });
    Object.defineProperty(huge, "size", { value: 2 * 1024 * 1024 });
    await changeUploadFile(huge);
    expect(toast.error).toHaveBeenCalledWith("File is too large to load in the editor (max 1 MB)");
    expect(editor.value).toBe(original);
  });

  it("opens a Git sync dialog explaining server configuration and the apply CLI", () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });

    fireEvent.click(screen.getByTestId("jobdefs-git-sync"));

    const dialog = screen.getByRole("dialog", { name: "Git sync" });
    expect(dialog).toBeVisible();
    expect(dialog).toHaveTextContent("CAESIUM_JOBDEF_GIT_SOURCES");
    expect(dialog).toHaveTextContent("CAESIUM_JOBDEF_GIT_ENABLED=true");
    expect(dialog).toHaveTextContent("caesium job apply --path");
  });

  it("keeps Apply enabled when the same file is uploaded again", async () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });
    const uploaded = singleJobYaml("uploaded-job");
    await changeUploadFile(new File([uploaded], "uploaded.job.yaml", { type: "text/yaml" }));
    await settleLintAndDiff();

    const apply = screen.getByRole("button", { name: /Apply definition/i });
    expect(apply).toBeEnabled();
    const lintCalls = vi.mocked(api.lintJobDef).mock.calls.length;

    await changeUploadFile(new File([uploaded], "uploaded.job.yaml", { type: "text/yaml" }));
    await settleLintAndDiff();

    expect(apply).toBeEnabled();
    expect(vi.mocked(api.lintJobDef).mock.calls.length).toBe(lintCalls);
    expect(toast.success).toHaveBeenLastCalledWith("Loaded uploaded.job.yaml");
  });

  it("ignores completions from superseded file selections", async () => {
    render(<JobDefsPage />, { wrapper: createWrapper() });

    let resolveFirst: ((value: string) => void) | undefined;
    const first = new File(["stale: true\n"], "first.yaml", { type: "text/yaml" });
    vi.spyOn(first, "text").mockReturnValue(
      new Promise((resolve) => {
        resolveFirst = resolve;
      }),
    );
    const secondYaml = singleJobYaml("second-job");
    const second = new File([secondYaml], "second.yaml", { type: "text/yaml" });

    const input = screen.getByTestId("jobdefs-upload-input");
    await act(async () => {
      fireEvent.change(input, { target: { files: [first] } });
      await Promise.resolve();
    });
    await changeUploadFile(second);
    expect(screen.getByLabelText("job.yaml editor")).toHaveValue(secondYaml);

    await act(async () => {
      resolveFirst?.("stale: true\n");
      await Promise.resolve();
    });
    expect(screen.getByLabelText("job.yaml editor")).toHaveValue(secondYaml);
    expect(toast.success).toHaveBeenLastCalledWith("Loaded second.yaml");
  });

  it("restores focus to Git sync when the dialog closes", async () => {
    vi.useRealTimers();
    render(<JobDefsPage />, { wrapper: createWrapper() });

    const opener = screen.getByTestId("jobdefs-git-sync");
    opener.focus();
    fireEvent.click(opener);

    const dialog = await screen.findByRole("dialog", { name: "Git sync" });
    fireEvent.keyDown(dialog, { key: "Escape", code: "Escape" });

    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: "Git sync" })).not.toBeInTheDocument();
    });
    await waitFor(() => {
      expect(screen.getByTestId("jobdefs-git-sync")).toHaveFocus();
    });
  });
});

async function changeUploadFile(file: File) {
  const input = screen.getByTestId("jobdefs-upload-input");
  await act(async () => {
    fireEvent.change(input, { target: { files: [file] } });
    await Promise.resolve();
  });
}
