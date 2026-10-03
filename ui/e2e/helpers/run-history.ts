import type { Page } from "@playwright/test";
import { runIdFromHref, statusFromRowText, type ConsoleRunRow } from "./cluster";

/** Read each run's status from its own history row, independently of layout. */
export async function readRunRows(page: Page, jobId: string): Promise<ConsoleRunRow[]> {
  const history = page.getByTestId("job-runs-list").getByTestId("run-history-row");
  const rows: ConsoleRunRow[] = [];
  for (let index = 0; index < await history.count(); index += 1) {
    const row = history.nth(index);
    const links = row.locator("a");
    for (let linkIndex = 0; linkIndex < await links.count(); linkIndex += 1) {
      const href = (await links.nth(linkIndex).getAttribute("href")) ?? "";
      const id = runIdFromHref(href, jobId);
      if (!id) continue;
      // The open link is a sibling of the status cell. Unknown, missing, or
      // ambiguous badges remain unreadable evidence for the recovery gate.
      const badge = row.locator('[data-history-cell="status"] [data-status]');
      const value = await badge.count() === 1 ? await badge.getAttribute("data-status") : null;
      const status = value === null ? null : statusFromRowText(value);
      rows.push({ id, status: status === value ? status : null });
    }
  }
  return rows;
}
