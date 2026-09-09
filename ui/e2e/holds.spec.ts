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
  for (let i = 0; i < 5; i++) await runHoldJob(request, producer.id);
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
  await expect(
    page.getByTestId(`lineage-impact-node::${output}:${consumer.id}`),
  ).toHaveAttribute("data-hold-affected", "true");
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
  await page.goto(holdURL(hold));
  await expect(page.getByTestId("hold-panel")).toHaveAttribute(
    "data-hold-id",
    hold.id,
  );
});
