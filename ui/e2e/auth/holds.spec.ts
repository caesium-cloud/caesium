import { expect, test } from "@playwright/test";
import { authHeaders, loginAtUrl, obtainAuthKeys } from "../helpers/auth";
import {
  activeHold,
  applyHoldJobs,
  consumerDefinition,
  holdDefinition,
  holdURL,
  runHoldJob,
} from "../helpers/holds";

test("authenticated operator releases the reviewed hold and a viewer cannot release its replacement", async ({
  page,
  request,
}) => {
  test.slow();
  const admin = process.env.CAESIUM_E2E_AUTH_ADMIN_KEY;
  expect(
    admin,
    "auth hold scenario needs the lane admin setup key",
  ).toBeTruthy();
  const headers = authHeaders(admin!);
  const keys = await obtainAuthKeys(request);
  const operatorResponse = await request.post("/v1/auth/keys", {
    headers,
    data: { role: "operator", description: "UI hold release regression" },
  });
  expect(operatorResponse.ok(), await operatorResponse.text()).toBeTruthy();
  const operator = (await operatorResponse.json()).key as string;
  const suffix = crypto.randomUUID().slice(0, 8);
  const name = `warehouse/auth-hold-${suffix}`;
  const alias = `auth-hold-${suffix}`;
  const consumerAlias = `auth-hold-consumer-${suffix}`;
  const jobs = await applyHoldJobs(
    request,
    [
      holdDefinition(alias, name, 1, false),
      consumerDefinition(consumerAlias, name, `reports/auth-hold-${suffix}`),
    ],
    headers,
  );
  const producer = jobs.find((job) => job.alias === alias)!;
  const consumer = jobs.find((job) => job.alias === consumerAlias)!;
  await runHoldJob(request, producer.id, "succeeded", headers);
  const hold = await activeHold(request, name, headers);
  await runHoldJob(request, consumer.id, "skipped", headers);
  await loginAtUrl(page, holdURL(hold), operator);
  const releaseButton = page.getByRole("button", {
    name: "Release hold",
    exact: true,
  });
  await expect(releaseButton).toBeDisabled();
  await page
    .getByLabel("Release reason")
    .fill("Validated seasonal feed in live UI");
  await page.getByLabel("Tolerance for min").selectOption("24h");
  await expect(releaseButton).toBeEnabled();
  const releaseResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname ===
        `/v1/datasets/holds/${hold.id}/release`,
  );
  await releaseButton.click();
  const response = await releaseResponse;
  expect(response.status()).toBe(200);
  const released = (await response.json()).hold;
  expect(released.id).toBe(hold.id);
  expect(released.status).toBe("released");
  expect(released.release_note).toBe("Validated seasonal feed in live UI");
  expect(released.tolerances).toEqual({ min: "24h" });
  expect(released.released_by).toBeTruthy();
  await expect(page.getByTestId("hold-panel")).toContainText(
    "Dataset hold · released",
  );
  await runHoldJob(request, consumer.id, "succeeded", headers);

  // A tolerance is advisory: the next bad producer opens a different hold.
  await runHoldJob(request, producer.id, "succeeded", headers);
  const replacement = await activeHold(request, name, headers);
  expect(replacement.id).not.toBe(hold.id);
  // Historical deep links stay pinned even when a new active hold exists.
  await loginAtUrl(page, holdURL(hold), keys.viewer);
  await expect(page.getByTestId("hold-panel")).toHaveAttribute(
    "data-hold-id",
    hold.id,
  );
  await expect(page.getByTestId("hold-panel")).toContainText(
    "Dataset hold · released",
  );
  await page.getByTestId("hold-list-row").filter({ hasText: name }).click();
  await expect(page.getByTestId("hold-panel")).toHaveAttribute(
    "data-hold-id",
    replacement.id,
  );
  await expect(
    page.getByRole("button", { name: "Release hold", exact: true }),
  ).toBeDisabled();
  await expect(page.getByTestId("hold-release-gate")).toContainText("operator");
  expect((await activeHold(request, name, headers)).id).toBe(replacement.id);
});
