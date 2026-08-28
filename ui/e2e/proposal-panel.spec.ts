import { expect, test } from "@playwright/test";
import { applyDefinitions, awaitRun, findJobByAlias, triggerJob, type FixtureDefinition } from "./helpers/fixtures";

/**
 * Spec §5.6: a propose step that emits the three reserved output keys
 * (`proposal_kind`, `proposal_summary`, `proposal_artifact`) is rendered by
 * the Console as a proposal — a convention over ordinary `##caesium::output`
 * values, no new marker or endpoint. This drives that surface end-to-end with
 * a plain `alpine:3.23` step (no Terraform involved) shaped like the
 * `tf-runner tf-plan` propose step (plan C2) would emit.
 */

type ProposalFixtureDefinition = Omit<FixtureDefinition, "trigger"> & {
  apiVersion: string;
  kind: string;
  trigger: { type: string; configuration: Record<string, unknown> };
  steps: Array<{ name: string; engine: string; image: string; command: string[] }>;
};

const shellImage = "alpine:3.23";
const artifactDigest = "sha256:decd9b61c14751809e49c8946aeaad7ed87397b88f7fbbc14278afe971cca296";

test("task detail panel renders a terraform.plan.v1 proposal from a propose step's output", async ({
  page,
  request,
}) => {
  test.slow();

  const alias = `proposal-panel-e2e-${Date.now().toString(36)}`;
  await applyDefinitions(request, buildProposalDefinition(alias));
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "succeeded" });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  const node = page.locator(".react-flow__node").first();
  await expect(node).toBeVisible({ timeout: 30_000 });
  await node.click();

  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();
  await panel.getByRole("button", { name: "Details" }).click();

  const proposal = page.getByTestId("proposal-panel");
  await expect(proposal).toBeVisible();
  await expect(proposal.getByTestId("proposal-kind")).toContainText("terraform.plan.v1");

  // Exactly the three action buckets the fixture's summary carries — never
  // `changes` or `outputs`, which the fixture also carries (mirroring the
  // real `reagents/internal/tf.Summary` wire shape) precisely because they used
  // to leak through as bogus action badges before the explicit skip list in
  // proposal-renderers.ts.
  const counts = proposal.getByTestId("proposal-count");
  await expect(counts).toHaveCount(3);
  await expect(proposal.locator('[data-testid="proposal-count"][data-action="add"]')).toContainText("2");
  await expect(proposal.locator('[data-testid="proposal-count"][data-action="change"]')).toContainText("1");
  await expect(proposal.locator('[data-testid="proposal-count"][data-action="destroy"]')).toContainText("0");
  await expect(proposal.locator('[data-testid="proposal-count"][data-action="changes"]')).toHaveCount(0);
  await expect(proposal.locator('[data-testid="proposal-count"][data-action="outputs"]')).toHaveCount(0);

  const rows = proposal.getByTestId("proposal-resource-row");
  await expect(rows).toHaveCount(3);
  await expect(rows.first()).toHaveAttribute("data-address", "aws_instance.web");
  await expect(rows.first()).toHaveAttribute("data-action", "add");

  const artifact = proposal.getByTestId("proposal-artifact");
  await expect(artifact).toContainText("plan");
  await expect(proposal.getByTestId("proposal-artifact-digest")).toContainText(artifactDigest);
  await expect(proposal.getByTestId("proposal-artifact-size")).toContainText("2.0 KB");
  await expect(proposal.getByTestId("proposal-artifact-path")).toContainText("/workspace/tf.plan");

  // The Console is not in the data path: no download affordance for the artifact.
  await expect(artifact.locator("a")).toHaveCount(0);
  await expect(artifact.locator("button")).toHaveCount(0);
});

function buildProposalDefinition(alias: string): ProposalFixtureDefinition {
  // Shaped like the real `reagents/internal/tf.Summary` wire payload
  // (reagents/internal/tf/summary.go): `changes` and `outputs` are both present
  // on every real proposal (Outputs has no `omitempty`), so they belong in
  // this fixture even though this scenario doesn't exercise Terraform.
  const summary = {
    changes: true,
    add: 2,
    change: 1,
    destroy: 0,
    outputs: 0,
    resources: [
      { address: "aws_instance.web", action: "add" },
      { address: "aws_instance.api", action: "add" },
      { address: "aws_security_group.web", action: "change" },
    ],
  };
  const outputPayload = {
    proposal_kind: "terraform.plan.v1",
    proposal_summary: JSON.stringify(summary),
    proposal_artifact: "plan",
  };
  const outputRefPayload = {
    key: "plan",
    path: "/workspace/tf.plan",
    digest: artifactDigest,
    size: 2048,
  };

  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: {
      type: "cron",
      configuration: { cron: "0 2 * * *", timezone: "UTC" },
    },
    steps: [
      {
        name: "plan",
        engine: "docker",
        image: shellImage,
        command: [
          "sh",
          "-c",
          `echo '##caesium::output ${JSON.stringify(outputPayload)}' && echo '##caesium::output-ref ${JSON.stringify(outputRefPayload)}'`,
        ],
      },
    ],
  };
}
