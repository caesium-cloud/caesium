import { expect, test } from "@playwright/test";
import {
  activeHold,
  applyHoldJobs,
  consumerDefinition,
  holdDefinition,
  holdURL,
  runHoldJob,
} from "./helpers/holds";

test("live held lineage, downstream skip and recorded baseline remain visible without release authority", async ({
  page,
  request,
}) => {
  test.slow();
  const features = await request.get("/v1/system/features");
  expect((await features.json()).data_assertions_enabled).toBe(true);
  const suffix = crypto.randomUUID().slice(0, 8);
  const name = `warehouse/hold-ui-${suffix}`;
  const output = `reports/hold-ui-${suffix}`;
  const alias = `hold-ui-${suffix}`;
  const consumerAlias = `hold-ui-consumer-${suffix}`;
  const jobs = await applyHoldJobs(request, [
    holdDefinition(alias, name, 100),
    consumerDefinition(consumerAlias, name, output),
  ]);
  const producer = jobs.find((job) => job.alias === alias)!;
  const consumer = jobs.find((job) => job.alias === consumerAlias)!;
  const seedRuns = [];
  for (let i = 0; i < 5; i++)
    seedRuns.push(await runHoldJob(request, producer.id));
  // Observed lineage must exist before the hold freezes its impact evidence.
  await runHoldJob(request, consumer.id);
  await expect
    .poll(async () => {
      const response = await request.get(
        `/v1/lineage/impact?${new URLSearchParams({ namespace: "", name })}`,
      );
      expect(response.ok(), await response.text()).toBeTruthy();
      return (await response.json()).downstream.map(
        (node: { dataset_name: string }) => node.dataset_name,
      );
    })
    .toContain(output);
  await applyHoldJobs(request, [holdDefinition(alias, name, 1000)]);
  const badRun = await runHoldJob(request, producer.id);
  await runHoldJob(request, producer.id);
  const hold = await activeHold(request, name);
  expect(hold.occurrence_count).toBe(2);
  expect(
    hold.violations?.some((v) => v.assertion === "deltaFromBaseline"),
  ).toBe(true);
  const skipped = await runHoldJob(request, consumer.id, "skipped");

  await page.goto(`/lineage?${new URLSearchParams({ namespace: "", name })}`);
  await expect(page.getByTestId("lineage-root-node")).toHaveAttribute(
    "data-hold-status",
    "active",
  );
  const downstreamNodes = page.getByTestId(
    `lineage-impact-node::${output}:${consumer.id}`,
  );
  // Impact rows are recorded per task run. The earlier successful consumer and
  // the later skipped consumer can both appear for this dataset/job identity;
  // the graph renders every returned row, and every occurrence must be shaded.
  await expect.poll(() => downstreamNodes.count()).toBeGreaterThan(0);
  await expect
    .poll(
      () =>
        downstreamNodes.evaluateAll(
          (nodes) =>
            nodes.length > 0 &&
            nodes.every(
              (node) => node.getAttribute("data-hold-affected") === "true",
            ),
        ),
      {
        message:
          "Every matching downstream occurrence must show the active hold overlay",
      },
    )
    .toBe(true);
  await page.getByTestId("lineage-hold-badge").click();
  await expect(page.getByTestId("hold-panel")).toHaveAttribute(
    "data-hold-id",
    hold.id,
  );
  await expect(page.getByTestId("hold-occurrences")).toHaveText("2");
  await expect(page.getByTestId("hold-panel")).toContainText("Observed: 1000");
  await expect(page.getByTestId("baseline-sparkline")).toBeVisible();
  await expect(page.getByTestId("baseline-band")).toBeAttached();
  await expect(page.getByTestId("baseline-observation")).toBeAttached();
  await expect(
    page.getByRole("button", { name: "Release hold", exact: true }),
  ).toBeDisabled();
  await expect(page.getByTestId("hold-release-gate")).toBeVisible();
  const holdsResponse = await request.get(
    "/v1/datasets/holds?status=active&limit=1",
  );
  const total = (await holdsResponse.json()).total;
  await expect(
    page.locator("aside").getByRole("link", { name: /^Holds\b/ }),
  ).toContainText(String(total));

  await page.goto(`/jobs/${consumer.id}/runs/${skipped.id}`);
  const skipLink = page.getByTestId("hold-skip-reason").getByRole("link");
  await expect(skipLink).toHaveText(name);
  await skipLink.click();
  await expect(page.getByTestId("hold-panel")).toHaveAttribute(
    "data-hold-id",
    hold.id,
  );

  await page.goto(`/jobs/${producer.id}/runs/${badRun.id}`);
  await expect(page.getByTestId("data-assertions-panel")).toContainText(
    "deltaFromBaseline",
  );
  await expect(page.getByTestId("baseline-sparkline")).toBeVisible();
  // The breach is listed beside the check that held, each with its outcome.
  const results = page.getByTestId("assertion-result");
  await expect(results).toHaveCount(2);
  await expect(
    results.filter({ hasText: "rowCount.min: Passed" }),
  ).toHaveAttribute("data-outcome", "passed");
  const breach = results.filter({ hasText: "rowCount.deltaFromBaseline" });
  await expect(breach).toHaveAttribute("data-outcome", "violated");
  await expect(breach).toHaveAttribute("data-disposition", "hold");

  // The first run had no history to compare against, so the delta check
  // decided nothing; the fifth passed against four samples of five. Neither
  // reads as a clean pass.
  await page.goto(`/jobs/${producer.id}/runs/${seedRuns[0].id}`);
  await expect(page.getByTestId("data-assertions-panel")).toContainText(
    "Not decided: no clean baseline yet",
  );
  await page.goto(`/jobs/${producer.id}/runs/${seedRuns[4].id}`);
  const coldStart = page
    .getByTestId("assertion-result")
    .filter({ hasText: "rowCount.deltaFromBaseline" });
  await expect(coldStart).toHaveAttribute("data-outcome", "passed");
  await expect(coldStart).toHaveAttribute("data-seeding", "true");
  await expect(coldStart).toContainText("Passed during cold start");

  await page.goto(holdURL(hold));
  await expect(page.getByTestId("hold-panel")).toHaveAttribute(
    "data-hold-id",
    hold.id,
  );
});
