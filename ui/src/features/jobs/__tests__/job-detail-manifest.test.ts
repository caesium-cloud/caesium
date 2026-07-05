import { stringify as yamlStringify } from "yaml";
import { describe, expect, it } from "vitest";
import type { Atom, Job, JobDAGResponse, JobTask, Trigger } from "@/lib/api";
import { buildJobAuthoringManifest, formatCommandForDisplay } from "../job-detail-manifest";

describe("job detail command formatting", () => {
  it("decodes JSON-array command strings before joining for display", () => {
    expect(formatCommandForDisplay('["sh","-c","echo \\u003e /out/files.json \\u0026\\u0026 echo ok"]')).toBe(
      "sh -c echo > /out/files.json && echo ok",
    );
  });

  it("handles already decoded arrays and raw command strings", () => {
    expect(formatCommandForDisplay(["python", "-m", "pytest"])).toBe("python -m pytest");
    expect(formatCommandForDisplay("echo already > /tmp/out")).toBe("echo already > /tmp/out");
    expect(formatCommandForDisplay()).toBe("N/A");
  });
});

describe("buildJobAuthoringManifest", () => {
  it("reconstructs a clean jobdef-shaped manifest from loaded job-detail data", () => {
    const job = {
      id: "job-1",
      alias: "nightly-report",
      trigger_id: "trigger-1",
      labels: { team: "analytics" },
      annotations: {},
      cache_config: { enabled: true, ttl: "1h" },
      max_parallel_tasks: 2,
      task_timeout: 5_000_000_000,
      run_timeout: 60_000_000_000,
      paused: false,
      created_at: "2026-07-05T00:00:00Z",
      updated_at: "2026-07-05T00:00:00Z",
      latest_run: { id: "run-1", status: "failed" },
      priority: "high",
      concurrency: { maxRuns: 1, strategy: "queue" },
      rate_limits: [{ resource: "warehouse", limit: 3, window: "1m" }],
      schema_validation: "warn",
      replay_safe: true,
    } as Job & {
      priority: string;
      concurrency: unknown;
      rate_limits: unknown;
      schema_validation: string;
      replay_safe: boolean;
    };
    const trigger: Trigger = {
      id: "trigger-1",
      alias: "nightly-report",
      type: "cron",
      configuration: '{"cron":"*/5 * * * *","timezone":"UTC","defaultParams":{"env":"prod"}}',
      created_at: "2026-07-05T00:00:00Z",
      updated_at: "2026-07-05T00:00:00Z",
    };
    const tasks: JobTask[] = [
      {
        id: "task-1",
        job_id: "job-1",
        atom_id: "atom-1",
        name: "extract",
        node_selector: { pool: "cpu" },
        retries: 2,
        retry_delay: 2_000_000_000,
        retry_backoff: true,
        trigger_rule: "all_success",
        cache_config: false,
        output_schema: { type: "object", properties: { rows: { type: "integer" } } },
        created_at: "2026-07-05T00:00:00Z",
        updated_at: "2026-07-05T00:00:00Z",
      },
      {
        id: "task-2",
        job_id: "job-1",
        atom_id: "atom-2",
        name: "load",
        node_selector: {},
        retries: 0,
        retry_delay: 0,
        retry_backoff: false,
        trigger_rule: "all_done",
        input_schema: { extract: { type: "object", required: ["rows"] } },
        created_at: "2026-07-05T00:00:00Z",
        updated_at: "2026-07-05T00:00:00Z",
      },
    ];
    const atoms: Record<string, Atom> = {
      "atom-1": {
        id: "atom-1",
        engine: "docker",
        image: "alpine:3.23",
        command: '["sh","-c","echo \\u003e /out/files.json"]',
        spec: { env: { FOO: "bar" }, workdir: "/work" },
        created_at: "2026-07-05T00:00:00Z",
        updated_at: "2026-07-05T00:00:00Z",
      },
      "atom-2": {
        id: "atom-2",
        engine: "kubernetes",
        image: "alpine:3.23",
        command: '["sh","-c","exit 1"]',
        spec: {
          kubernetes: {
            serviceAccountName: "report-runner",
            podAnnotations: { "caesium.dev/workload": "report" },
            automountServiceAccountToken: false,
            queueName: "batch",
          },
        },
        created_at: "2026-07-05T00:00:00Z",
        updated_at: "2026-07-05T00:00:00Z",
      },
    };
    const dag: JobDAGResponse = {
      job_id: "job-1",
      nodes: [
        { id: "task-1", atom_id: "atom-1", successors: ["task-2"] },
        { id: "task-2", atom_id: "atom-2", successors: [] },
      ],
      edges: [{ from: "task-1", to: "task-2" }],
    };

    const manifest = buildJobAuthoringManifest({ job, tasks, trigger, atoms, dag });

    expect(manifest).toEqual({
      apiVersion: "v1",
      kind: "Job",
      metadata: {
        alias: "nightly-report",
        labels: { team: "analytics" },
        maxParallelTasks: 2,
        taskTimeout: "5s",
        runTimeout: "1m",
        priority: "high",
        concurrency: { maxRuns: 1, strategy: "queue" },
        rateLimits: [{ resource: "warehouse", limit: 3, window: "1m" }],
        schemaValidation: "warn",
        replaySafe: true,
        cache: { enabled: true, ttl: "1h" },
      },
      trigger: {
        type: "cron",
        configuration: { cron: "*/5 * * * *", timezone: "UTC" },
        defaultParams: { env: "prod" },
      },
      steps: [
        {
          name: "extract",
          image: "alpine:3.23",
          command: ["sh", "-c", "echo > /out/files.json"],
          nodeSelector: { pool: "cpu" },
          next: ["load"],
          retries: 2,
          retryDelay: "2s",
          retryBackoff: true,
          cache: false,
          outputSchema: { type: "object", properties: { rows: { type: "integer" } } },
          env: { FOO: "bar" },
          workdir: "/work",
        },
        {
          name: "load",
          engine: "kubernetes",
          image: "alpine:3.23",
          command: ["sh", "-c", "exit 1"],
          triggerRule: "all_done",
          serviceAccountName: "report-runner",
          podAnnotations: { "caesium.dev/workload": "report" },
          automountServiceAccountToken: false,
          kueue: { queueName: "batch" },
          inputSchema: { extract: { type: "object", required: ["rows"] } },
        },
      ],
    });

    const yaml = yamlStringify(manifest);
    expect(yaml).toContain("apiVersion: v1");
    expect(yaml).toContain("kind: Job");
    expect(yaml).not.toContain("latest_run");
    expect(yaml).not.toContain("trigger_id");
    expect(yaml).not.toContain("configuration: '{");
  });
});
