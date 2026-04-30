import { expect, test } from "@playwright/test";

const PRIMARY_NAV: { label: string; heading: RegExp; urlMatch: RegExp }[] = [
  { label: "Jobs", heading: /^Jobs$/, urlMatch: /\/jobs$/ },
  { label: "Triggers", heading: /^Triggers$/, urlMatch: /\/triggers$/ },
  { label: "Atoms", heading: /^Atoms$/, urlMatch: /\/atoms$/ },
  { label: "Stats", heading: /^Operator Statistics$/, urlMatch: /\/stats$/ },
  { label: "System", heading: /^System$/, urlMatch: /\/system$/ },
  { label: "JobDefs", heading: /^Job Definitions$/, urlMatch: /\/jobdefs$/ },
];

test("sidebar navigates between every primary control-plane page", async ({ page }) => {
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();

  // Walk every sidebar link and verify it routes to the expected page and
  // renders without throwing — a fast smoke that catches broken routes,
  // crashed page components, or missing page headings after a refactor.
  // Scoped to <aside> so we don't collide with the breadcrumb links in the
  // header, and the link is matched by regex so a count badge appended to
  // the accessible name (e.g. "Jobs 3") still resolves.
  const sidebar = page.locator("aside");
  for (const item of PRIMARY_NAV) {
    await sidebar.getByRole("link", { name: new RegExp(`^${item.label}\\b`) }).click();
    await page.waitForURL(item.urlMatch);
    await expect(page.getByRole("heading", { name: item.heading })).toBeVisible();
  }
});
