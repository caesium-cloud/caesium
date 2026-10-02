# Operator console polish — PR #617

This pass keeps the console's mono typography, warm ink, cyan/gold status language, flat navigation, existing permissions, and prior QA/review fixes. It improves how the existing data is arranged and explained.

The findings were reproduced on `4d5b91f3dbaac22f111346c2af04557118d1147c` using `caesiumcloud/caesium:pr617-review-20261001` at localhost:8083 before source edits. The navigation probe recorded Jobs and then Triggers both at `scrollTop=500`.

## Finding to fix

| Finding | Change |
| --- | --- |
| 1. Sidebar alignment | Fixed indicator, label, count, and shortcut columns. Missing shortcuts and counts retain their slots; counts are right aligned. |
| 2. Historical history | An ordinal “Older” group and consistently anchored age caption replace archived marks in time coordinates. Only recent events use the 15-minute plot. |
| 3. Jobs hierarchy | Names receive more width, history receives less, header/rows share one grid, and empty history says “No runs yet.” Remove repeated cyan now-dots. Phone rows reflow with visible actions. |
| 4. Run header | Separate title, time/status/identity, metadata, task counts, and grouped navigation/execution actions. Keep one All runs link. Replay configures a baseline replay; Re-run starts a new run with the same parameters. |
| 5. Execution timeline | Choose ticks from the actual duration, including milliseconds. Keep endpoints inside the plot, place durations with task labels, and reveal full skip/error reasons through expandable details. Actual bar widths remain proportional; zero-duration events are explicitly described as markers. |
| 6. Mobile details | Wrap graph metadata and job navigation; retain the latest-run link. Initially frame the first task at readable zoom on narrow canvases; Fit view still shows the whole graph. A scroll hint and sticky task labels preserve timeline context. |
| 7. Section scrolling | Register the main scroller with router restoration. New destinations start at the top; Back restores the history entry. Preserve router history state when updating filters or task selection, and preserve scroll for job dialogs. |
| 8. Page consistency | Shared PageHeader and FilterChip establish title/count/description/action hierarchy and consistent filters. Remove Contracts' extra breadcrumb, eyebrow, and duplicated filter heading. |
| 9. Empty states | Datasets/Holds show a single relevant empty state with an existing next step. Selection guidance appears when records can be selected. Hide empty pagination and distinguish no inventory from no filter matches. |
| 10. Atoms | Combine atom/image identity, decode serialized arguments with explicit quoting, retain exact raw command disclosure, and make expansion keyboard accessible. Rename the abbreviated “Full ID” to “Atom ID”; copying still uses the full value. |
| 11. Stats | Failing-atom bars show task, job identity, and failure count without hover. Separate current/24h KPIs from the range-controlled analysis and label UTC/run-count units. |
| 12. Details/dialogs | Remove duplicate Server default text, use a trigger ID chip, and distinguish Delete/Invalidate actions with the danger treatment. Keep the existing confirmation behavior. |
| 13. Status wording | Distinguish succeeded, skipped, blocked, cached, and cancelled counts. “Completed” is the terminal aggregate, including failed/skipped tasks; its explanation and the separate status counts remain available. |

## Representative before / after

These are unedited browser screenshots with real API data, not mockups. The gallery covers desktop and phone layouts in both themes. The gallery pairs use the same seeded backend inventory: the original 4d5b91f3 frontend and the final production frontend. Wall-clock timestamps and elapsed ages can differ between captures. The original pre-edit matrix is also retained locally.

| Surface | Before | After |
| --- | --- | --- |
| Jobs and sidebar · dark · 1440×900 | [Before](ui-polish-617/jobs-dark-1440-before.png) | [After](ui-polish-617/jobs-dark-1440-after.png) |
| Run header and timeline · light · 1280×800 | [Before](ui-polish-617/run-light-1280-before.png) | [After](ui-polish-617/run-light-1280-after.png) |
| Job graph/navigation · dark · 390×844 | [Before](ui-polish-617/job-dark-390-before.png) | [After](ui-polish-617/job-dark-390-after.png) |
| Empty datasets · light · 1280×800 | [Before](ui-polish-617/datasets-light-1280-before.png) | [After](ui-polish-617/datasets-light-1280-after.png) |
| Atoms · dark · 1440×900 | [Before](ui-polish-617/atoms-dark-1440-before.png) | [After](ui-polish-617/atoms-dark-1440-after.png) |
| Stats · light · 1440×900 | [Before](ui-polish-617/stats-light-1440-before.png) | [After](ui-polish-617/stats-light-1440-after.png) |
| JobDefs · light · 390×844 | [Before](ui-polish-617/jobdefs-light-390-before.png) | [After](ui-polish-617/jobdefs-light-390-after.png) |

## Qualification

The UI production bundle is built and tested inside `mcr.microsoft.com/playwright:v1.59.1-noble`, matching the lockfile's Playwright version. It is served through a local preview proxy against the unchanged `pr617-review-20261001` release backend. This is production-frontend / real-backend qualification, not a newly compiled Go release image. No Go source changed in this follow-up.

| Check | Result |
| --- | --- |
| Containerized ESLint | Passed |
| Containerized Vitest | 433 tests / 60 files passed (`npm test -- --maxWorkers=2`) |
| Production build and unchanged asset budgets | Passed; largest JS 1,233.51 KiB raw / 361.50 KiB gzip; all assets including fonts 2,594.97 KiB raw / 786.36 KiB gzip |
| Broad browser pass, one worker, zero retries | 58 passed / 2 failed. All ten new polish checks, both mobile journeys, ten accessibility checks, twelve original QA checks, and six unchanged Linux screenshot baselines passed. |
| Documentation guardrail | Containerized `go test ./internal/guardrails/... -count=1` passed |
| Read-only browser measurements | Both themes: aligned count/shortcut edges, section reset and Back restoration, preserved dialog scroll, sticky timeline labels, full trigger-ID copying, empty/filter clearing, and text token contrast passed |

The two broad-pass failures were existing light-theme review journeys: the identifier/shortcut test could not find the run heading after navigation, and the malformed-timestamp test could not find Run History after navigation. The latter trace includes an incomplete `route.fetch` at teardown. They are retained as failures in `regression-results.json`; this pass does not claim to fix intermittent route-loading stalls. An unchanged, isolated recheck of those two journeys passed in both themes (4/4, zero retries). That recheck does not erase the broader failures.

Earlier polish attempts caught stale test expectations and a real mobile framing race between React Flow's initial fit and the resize handler. The framing race was fixed by giving the measured-viewport handler sole ownership of automatic fitting. Direct live screenshots then exposed a non-shrinking counter wrapper; the final browser pass covers that correction with a real running job. An unrestricted unit-test run hit a process-spawn timeout while enumerating Playwright projects; the final two-worker run passed without changing timeouts or assertions.

Rendered pages are captured at 1440×900, 1280×800, and 390×844 in both themes. The full local evidence, including scripts, original reproduction, screenshots, browser traces, and JSON reports, is retained under `.tmp/pr617-polish/`. Representative screenshots are committed above. A separate real 45-second log-streaming run with a long alias was captured while running on Jobs, Job detail, and Run detail at every viewport/theme combination (18 screenshots); the API then reported successful completion. Missing run values and historical-only rows were inspected, as were filtered-empty pages and Cache/Configuration dialogs. No API responses were invented for these visual captures; malformed timestamps remain an explicitly synthetic regression scenario.

Measured `text-3` contrast on page/panel backgrounds is 7.02–7.57:1 in dark mode and 5.87–6.99:1 in light mode. The checks also measured `text-2`, cyan, and danger against those three backgrounds; the minimum was 4.93:1. Disabled controls retain reduced opacity and dashed borders. These measurements do not establish whole-application accessibility compliance.

Populated Datasets/Holds/Contracts layouts and feature-gated incident screens still require a dedicated visual qualification pass. The local visual matrix covers their empty states. Multi-node/degraded operation, external SSO providers, and the full concurrent browser suite are outside this polish pass.

## Search follow-up

The terminal-like footer obscured its navigation-search action: only the narrow left prompt opened it, while desktop results stretched across the content area. The entire 44 px footer is now a labeled search button with a search icon and platform shortcut. Cmd/Ctrl-K and `:` remain supported. Both desktop and phone use the same bounded dialog, with a reserved close-button area, quieter selected rows, consistent metadata, short IDs to distinguish duplicate names, and visible keyboard hints. Search accepts full IDs as well as names. Escape, Close, and outside click restore focus to the footer; resizing retains the query and input focus.

These captures compare the production frontend at `eca55d81` with the search follow-up against the same real backend. The broader qualification limits above still apply.

| Surface | Before | After |
| --- | --- | --- |
| Search button · dark · 1440×900 | [Before](ui-polish-617/search-closed-dark-1440-before.png) | [After](ui-polish-617/search-closed-dark-1440-after.png) |
| Search dialog · dark · 1440×900 | [Before](ui-polish-617/search-open-dark-1440-before.png) | [After](ui-polish-617/search-open-dark-1440-after.png) |
| Search dialog · light · 390×844 | [Before](ui-polish-617/search-open-light-390-before.png) | [After](ui-polish-617/search-open-light-390-after.png) |

The search follow-up passed containerized ESLint, all 433 unit tests, the production build and unchanged bundle budgets, and the documentation guardrail. Twelve focused browser checks passed with one worker and zero retries across both themes, covering full-bar clicks, both keyboard shortcuts, focus containment/restoration, dismissal, viewport changes, full-ID navigation, the existing mobile journey, copying, and reduced motion. Direct screenshots were inspected at all three target sizes in both themes, including long results and no matches; six scoped axe scans of the open search dialog reported zero WCAG 2.0/2.1 A/AA violations. These scoped scans do not establish whole-application compliance. Local receipts are under `.tmp/pr617-search/`.

A second regression selection passed 8/8 with zero retries: navigation-search destination/active-route checks in both themes and all six unchanged Linux visual baselines. Total focused browser coverage for this follow-up is 20/20 passing; it does not supersede the broader failures recorded above.
