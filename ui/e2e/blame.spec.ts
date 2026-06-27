import { expect, test, type APIRequestContext } from "@playwright/test";

type E2EJob = {
  id: string;
  alias: string;
};

type ApplyProvenance = {
  source_id: string;
  repo: string;
  ref: string;
  commit: string;
  path: string;
};

type BlameFixture = {
  apiVersion: string;
  kind: string;
  metadata: {
    alias: string;
  };
  trigger: {
    type: string;
    configuration: {
      cron: string;
      timezone: string;
    };
  };
  steps: Array<{
    name: string;
    engine: string;
    image: string;
    command: string[];
    dependsOn?: string[];
    next?: string[];
  }>;
};

const neutralShellImage = "debian:12-slim";
const firstCommit = "1111111111111111111111111111111111111111";
const secondCommit = "2222222222222222222222222222222222222222";

test("operator can inspect blame attribution for provenance-backed applies", async ({ page, request }) => {
  test.slow();

  const alias = `blame-e2e-${Date.now().toString(36)}`;
  const sourceId = `ui-e2e/${alias}`;

  await applyDefinitionWithProvenance(
    request,
    buildBlameDefinition(alias, "v1"),
    buildProvenance(sourceId, firstCommit, alias),
  );
  const job = await findJobByAlias(request, alias);

  // Force a clearly-distinct CreatedAt between the two snapshot applies. Blame
  // orders snapshots by CreatedAt with an unstable tiebreak on an exact tick
  // collision (internal/blame/query.go), so a too-small gap could flip the
  // introducing-commit attribution under CI clock granularity. 1.1s is safely
  // above any storage-timestamp resolution.
  await new Promise((resolve) => setTimeout(resolve, 1100));

  await applyDefinitionWithProvenance(
    request,
    buildBlameDefinition(alias, "v2"),
    buildProvenance(sourceId, secondCommit, alias),
  );

  await page.goto(`/jobs/${job.id}/blame`);

  await expect(page.getByTestId("blame-container")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("blame-coverage")).toContainText("Topology, image, and command");
  await expect(page.getByTestId("blame-coverage-caveat")).toContainText(
    "env/spec/retries/cache/schema/sla/triggerRules are intentionally untracked",
  );

  const changedTask = page.getByTestId("blame-task-row").filter({ hasText: "transform" });
  await expect(changedTask.getByTestId("blame-task-introducing-commit")).toContainText(secondCommit);
  await expect(changedTask.getByTestId("blame-task-command")).toContainText("transform-v2");

  const stableTask = page.getByTestId("blame-task-row").filter({ hasText: "extract" });
  await expect(stableTask.getByTestId("blame-task-introducing-commit")).toContainText(firstCommit);
});

function buildBlameDefinition(alias: string, variant: string): BlameFixture {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
    },
    trigger: {
      type: "cron",
      configuration: {
        cron: "0 2 * * *",
        timezone: "UTC",
      },
    },
    steps: [
      {
        name: "extract",
        engine: "docker",
        image: neutralShellImage,
        command: ["sh", "-c", "echo extract"],
      },
      {
        name: "transform",
        engine: "docker",
        image: neutralShellImage,
        command: ["sh", "-c", `echo transform-${variant}`],
        dependsOn: ["extract"],
      },
      {
        name: "load",
        engine: "docker",
        image: neutralShellImage,
        command: ["sh", "-c", "echo load"],
        dependsOn: ["transform"],
      },
    ],
  };
}

function buildProvenance(sourceId: string, commit: string, alias: string): ApplyProvenance {
  return {
    source_id: sourceId,
    repo: "https://example.test/acme/caesium-e2e.git",
    ref: "refs/heads/main",
    commit,
    path: `jobs/${alias}.job.yaml`,
  };
}

async function applyDefinitionWithProvenance(
  request: APIRequestContext,
  definition: BlameFixture,
  provenance: ApplyProvenance,
): Promise<void> {
  const response = await request.post("/v1/jobdefs/apply", {
    data: {
      definitions: [definition],
      provenance,
    },
  });
  if (!response.ok()) {
    throw new Error(`failed to apply fixture: ${response.status()} ${await response.text()}`);
  }
}

async function findJobByAlias(request: APIRequestContext, alias: string): Promise<E2EJob> {
  let foundJob: E2EJob | undefined;
  await expect
    .poll(
      async () => {
        const response = await request.get("/v1/jobs");
        if (!response.ok()) {
          throw new Error(`failed to list jobs: ${response.status()} ${await response.text()}`);
        }

        const jobs = (await response.json()) as E2EJob[];
        foundJob = jobs.find((candidate) => candidate.alias === alias);
        return foundJob?.alias ?? "";
      },
      {
        timeout: 10_000,
        intervals: [250, 500, 1_000],
      },
    )
    .toBe(alias);

  return foundJob!;
}
