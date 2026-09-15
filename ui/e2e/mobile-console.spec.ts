import { expect, test, type Locator, type Page } from "@playwright/test";
import { applyAndRun } from "./helpers/fixtures";

test.use({ hasTouch: true });

async function expectWithinViewport(page: Page, locator: Locator, viewportWidth: number) {
  await expect.poll(async () => {
    const box = await locator.boundingBox();
    return box ? box.x >= 0 && box.x + box.width <= viewportWidth + 1 : false;
  }).toBe(true);
}

async function expectDialogContentFitsViewport(page: Page, dialog: Locator) {
  await expect.poll(async () => {
    const box = await dialog.boundingBox();
    const viewport = page.viewportSize();
    return box && viewport
      ? box.x >= 0 && box.y >= 0 && box.x + box.width <= viewport.width + 1 && box.y + box.height <= viewport.height + 1
      : false;
  }).toBe(true);
}

async function expectNoPageHorizontalOverflow(page: Page) {
  await expect.poll(() => page.evaluate(() =>
    document.documentElement.scrollWidth <= window.innerWidth &&
    document.body.scrollWidth <= window.innerWidth &&
    document.querySelector("main")?.scrollWidth === document.querySelector("main")?.clientWidth,
  )).toBe(true);
}

async function finishMainEntranceAnimation(page: Page) {
  const main = page.locator("main");
  await main.evaluate((element) => {
    for (const animation of element.getAnimations()) {
      animation.finish();
    }
  });
  await expect(main).toHaveCSS("opacity", "1");
}

async function expectHeaderActionsDoNotOverlap(page: Page) {
  await expect.poll(() => page.locator("header button").evaluateAll((buttons) => {
    const visibleBoxes = buttons
      .filter((button) => {
        const style = getComputedStyle(button);
        return style.display !== "none" && style.visibility !== "hidden";
      })
      .map((button) => button.getBoundingClientRect());

    return visibleBoxes.every((box, index) =>
      box.width > 0 &&
      box.height > 0 &&
      box.left >= 0 &&
      box.right <= window.innerWidth &&
      visibleBoxes.slice(index + 1).every((other) =>
        box.right <= other.left || other.right <= box.left || box.bottom <= other.top || other.bottom <= box.top,
      ),
    );
  })).toBe(true);
}

test("operator surfaces remain usable at phone, tablet, and desktop widths", async ({ page, request }, testInfo) => {
  const { job, run } = await applyAndRun(request, "branching.job.yaml");

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await expectWithinViewport(page, page.locator("main"), 390);
  await expectNoPageHorizontalOverflow(page);
  await expectHeaderActionsDoNotOverlap(page);
  await page.getByRole("button", { name: "Open search" }).click();
  const commandInput = page.getByPlaceholder("Type a command or search...");
  await expect(commandInput).toBeVisible();
  const commandDialog = page.getByRole("dialog").filter({ has: commandInput });
  await expectDialogContentFitsViewport(page, commandDialog);
  await expect.poll(async () => {
    const [dialogBox, inputBox] = await Promise.all([commandDialog.boundingBox(), commandInput.locator("..").boundingBox()]);
    return Boolean(dialogBox && inputBox && Math.abs(dialogBox.x - inputBox.x) <= 1);
  }).toBe(true);
  await page.keyboard.press("Escape");

  // The job table retains its operational columns, but its own scroll viewport
  // contains the intentional horizontal overflow instead of widening the page.
  const jobsTable = page.getByTestId("jobs-table-scroll");
  await expect.poll(() => jobsTable.evaluate((table) => ({
    clientWidth: table.clientWidth,
    overflowX: getComputedStyle(table).overflowX,
    scrollWidth: table.scrollWidth,
  }))).toEqual(expect.objectContaining({ overflowX: "auto" }));
  await expect.poll(() => jobsTable.evaluate((table) => table.scrollWidth > table.clientWidth)).toBe(true);
  const searchInput = page.getByPlaceholder("Filter pipelines…");
  await searchInput.fill("no-mobile-match");
  const emptyState = page.getByRole("status");
  await expect(emptyState).toContainText("No pipelines match");
  await expectWithinViewport(page, emptyState, 390);
  await expect.poll(() => jobsTable.evaluate((table) => table.scrollWidth === table.clientWidth)).toBe(true);
  await searchInput.fill("");
  await expect(emptyState).toBeHidden();
  await jobsTable.evaluate((table) => { table.scrollLeft = table.scrollWidth; });
  const rowTrigger = page.getByTestId("job-row").filter({ hasText: job.alias }).getByRole("button", { name: "Trigger run" });
  await expect(rowTrigger).toBeVisible();
  await expect.poll(() => rowTrigger.evaluate((button) => {
    const buttonBox = button.getBoundingClientRect();
    const tableBox = button.closest('[data-testid="jobs-table-scroll"]')?.getBoundingClientRect();
    return {
      hoverNone: window.matchMedia("(hover: none)").matches,
      parentOpacity: getComputedStyle(button.parentElement!).opacity,
      withinScrollViewport: Boolean(tableBox && buttonBox.left >= tableBox.left && buttonBox.right <= tableBox.right),
    };
  })).toEqual({ hoverNone: true, parentOpacity: "1", withinScrollViewport: true });
  await rowTrigger.click();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}/runs/`));
  await page.goto("/jobs");

  const menu = page.getByRole("button", { name: "Open navigation" });
  await expect(menu).toBeVisible();
  await menu.click();
  const drawer = page.getByRole("dialog", { name: "Navigation" });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole("link", { name: /^System\b/ })).toBeVisible();
  await drawer.getByRole("link", { name: /^System\b/ }).click();
  await expect(drawer).toBeHidden();
  await expect(page).toHaveURL(/\/system$/);
  await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
  await expectWithinViewport(page, page.locator("main"), 390);
  await expectNoPageHorizontalOverflow(page);

  const kpiBoxes = await page.getByTestId("system-kpi").evaluateAll((cards) =>
    cards.map((card) => {
      const box = card.getBoundingClientRect();
      return { bottom: box.bottom, top: box.top };
    }),
  );
  expect(kpiBoxes).toHaveLength(4);
  for (let index = 1; index < kpiBoxes.length; index += 1) {
    expect(kpiBoxes[index]?.top).toBeGreaterThanOrEqual(kpiBoxes[index - 1]?.bottom ?? 0);
  }
  await page.screenshot({ path: testInfo.outputPath("phone-system.png") });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByText("Run detail", { exact: true })).toBeVisible();
  await expectWithinViewport(page, page.locator("main"), 390);
  await expect(page.getByRole("button", { name: "Re-run" })).toBeVisible();
  await expect(page.getByText("Interactive DAG + task logs", { exact: true })).toBeVisible();
  const taskLabel = page.getByTestId("task-node-label").first();
  await taskLabel.scrollIntoViewIfNeeded();
  await taskLabel.click();
  const taskPanel = page.getByTestId("task-detail-panel");
  await expect(taskPanel).toBeVisible();
  await expect(taskPanel.getByRole("button", { name: "Logs", exact: true })).toBeVisible();
  await expectNoPageHorizontalOverflow(page);

  await page.goto(`/jobs/${job.id}`);
  const trigger = page.getByRole("button", { name: "Trigger job" });
  await expect(trigger).toBeVisible();
  await trigger.click();
  const triggerDialog = page.getByRole("dialog", { name: "Trigger Job" });
  await expect(triggerDialog).toBeVisible();
  await expectDialogContentFitsViewport(page, triggerDialog);
  const runParameters = triggerDialog.getByLabel("Run parameters");
  const closeDialog = triggerDialog.getByRole("button", { name: "Close" });
  await runParameters.focus();
  await page.keyboard.press("Shift+Tab");
  await expect(closeDialog).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(triggerDialog.locator(":focus")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(triggerDialog).toBeHidden();
  await expect(trigger).toBeFocused();

  await page.setViewportSize({ width: 768, height: 1024 });
  await page.goto("/jobs");
  await expect(menu).toBeVisible();
  await expectWithinViewport(page, page.locator("main"), 768);
  await menu.click();
  await expect(drawer).toBeVisible();
  await expect.poll(async () => {
    const box = await drawer.boundingBox();
    return box ? box.x === 0 && box.y === 0 && box.width <= 320 && Math.abs(box.height - 1024) <= 1 : false;
  }).toBe(true);
  await expect(drawer).toHaveCSS("border-top-left-radius", "0px");
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();
  await expect(menu).toBeFocused();
  await menu.click();
  await expect(drawer).toBeVisible();
  await page.keyboard.press("g");
  await page.keyboard.press("y");
  await expect(page).toHaveURL(/\/system$/);
  await expect(drawer).toBeHidden();
  await menu.click();
  await expect(drawer).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/jobs$/);
  await expect(drawer).toBeHidden();
  await page.getByRole("button", { name: "Open search" }).click();
  await expect(commandInput).toBeVisible();
  await expectDialogContentFitsViewport(page, commandDialog);
  await expect.poll(async () => {
    const [dialogBox, inputBox] = await Promise.all([commandDialog.boundingBox(), commandInput.locator("..").boundingBox()]);
    return Boolean(dialogBox && inputBox && Math.abs(dialogBox.x - inputBox.x) <= 1);
  }).toBe(true);
  await page.keyboard.press("Escape");
  await page.goto(`/jobs/${job.id}`);
  await page.getByRole("link", { name: "Runs" }).click();
  const secondaryDialog = page.getByRole("dialog", { name: "Run History" });
  await expectDialogContentFitsViewport(page, secondaryDialog);
  await expect.poll(async () => {
    const [dialogBox, titleBox] = await Promise.all([secondaryDialog.boundingBox(), secondaryDialog.getByText("Run History", { exact: true }).boundingBox()]);
    return Boolean(dialogBox && titleBox && Math.abs(titleBox.x - dialogBox.x - 24) <= 1);
  }).toBe(true);
  await page.keyboard.press("Escape");
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await expect(jobsTable).toBeVisible();
  await finishMainEntranceAnimation(page);
  await page.screenshot({ path: testInfo.outputPath("tablet-jobs.png") });

  await page.setViewportSize({ width: 390, height: 390 });
  await menu.click();
  await expect(drawer).toBeVisible();
  const finalNavItem = drawer.getByRole("link", { name: /^JobDefs\b/ });
  await finalNavItem.scrollIntoViewIfNeeded();
  await expect(finalNavItem).toBeVisible();
  await expect(drawer.getByRole("link", { name: /^Contracts\b/ })).toBeVisible();
  await expect(drawer.getByText("Cluster", { exact: true })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();
  await expect(menu).toBeFocused();

  await menu.click();
  await expect(drawer).toBeVisible();
  await page.setViewportSize({ width: 1280, height: 960 });
  await expect(drawer).toBeHidden();
  await expect.poll(() => page.evaluate(() => ({
    overflow: document.body.style.overflow,
    pointerEvents: document.body.style.pointerEvents,
  }))).toEqual({ overflow: "", pointerEvents: "" });
  await expect(page.locator("aside").getByRole("link", { name: /^Jobs\b/ }).first()).toBeFocused();
  await page.goto("/jobs");
  await expect(page.locator("aside")).toBeVisible();
  await expect(menu).toBeHidden();
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await finishMainEntranceAnimation(page);
  await page.screenshot({ path: testInfo.outputPath("desktop-jobs.png") });
});
