# Operator console facelift (October 2026)

> Status: Historical record. Evidence and dispositions for the Standard operator
> console facelift shipped in PR #617 and its motion follow-up in PR #619.
> Current console behavior is the code under `ui/`; this file preserves the
> design decisions, the data-availability limits, and the qualification runs.

## Facelift (PR #617)

Implements the Standard concept from `Caesium Operator Console Facelift.zip`, starting with its handoff README, design specification, route guide, and functional invariants.

The console uses self-hosted Sometype Mono, warm text on cool instrument surfaces, flat lists, word tabs, and status glyphs. The shell has a 44 px header, 208 px sidebar, and 44 px search footer. Clicking anywhere in the footer, Cmd/Ctrl-K, or `:` opens navigation search in a bounded dialog on desktop and mobile. Search finds pages and resources by name or ID; it does not execute shell commands. Escape, Close, or clicking outside dismisses search and restores focus to its button. Mobile also keeps its navigation drawer.

The shared UTC clock drives recent fleet history and trigger countdowns. Historical strips and terminal timelines stop subscribing to ticks. Each CSS instrument aligns its own animation to UTC when inserted and resynchronizes after a hidden tab returns. Reduced motion freezes live indicators and replaces the oscillator with a flat line. Identifiers retain their original case and use one copy-chip component with eight visible characters, the full title and clipboard payload, and a one-second confirmation. Plain HTTP deployments use native copy when the Clipboard API is unavailable; if both paths are denied, the chip exposes the full selectable value.

Run pages show alias and start time before the diagnostic ID. Task nodes are 260 × 112 px with status-specific electrons. Timeline rows have a 48 px minimum to accommodate task annotations, graph spacing is updated, and contract and lineage nodes are 260 × 80 px. Lineage metadata remains in the node tooltip; its links, freshness and hold overlays remain functional. Job YAML uses the same CodeMirror theme as the authoring editor.

Re-run uses Alt-R; unmodified `r` does not launch work. Global `g` navigation chords consume their second key before route shortcuts run. UTC time formatting accepts malformed or absent timestamps and displays a fallback. Both YAML surfaces follow the resolved light/dark theme, including selections and tooltips.

The fleet strip defaults to the last 15 minutes, with URL-persisted 1-hour and 24-hour choices and a separate, labeled ordinal fallback for older or undated history. One shared clock marker and aligned grid frame recent history; mark height shows duration up to 10 seconds. Live events update both status and the strip immediately, with coalesced reads to reconcile bounded history. `GET /v1/jobs` includes `last_runs[].started_at`, which the existing SQL projection already collected. The integration history test checks this field against a real run.

### Data availability

The facelift does not invent fields missing from the current server:

- Build SHA, raft index/term, last-contact timestamps and leader CPU/memory are unavailable; the console says so. Existing membership, liveness probes, worker counts and latency remain visible. Standalone or stale health does not draw fabricated healthy voters.
- Receipt metadata says pending, available or unavailable based on the receipt query. The receipt API supplies a digest and verification results, not a signature, so the console does not label it signed.
- Atom used-by counts, per-task historical cache hit ratios and manifest digests are not returned by these APIs. Existing creation time, cache inventory/policy/run statistics and reconstructed YAML remain available.
- Blame has snapshot/commit identities and causation evidence, without a complete timestamped commit history or first-bad-run designation. That evidence remains visible without invented times.

Dark `--text-3` lightness is 50%, slightly above the handoff's 48%, because the supplied value measured 4.41:1 on the obsidian surface. No accessibility allowlist was expanded.

### Validation

UI checks:

```sh
cd ui
npm run lint
npm test
npm run build:ci
npx playwright test --project=network-recovery --retries=0
```

The `light` project runs accessibility, visual, mobile-console, performance and facelift checks. `network-recovery` depends on both default and light, so the ordinary CI entry point includes both themes. Visual baselines are generated and compared on Linux with the Playwright version installed by `package-lock.json`. Regenerate only intentionally:

```sh
npx playwright test visual.spec.ts --project=default --project=light --update-snapshots
```

Run the browser suite against the containerized production release, with the environment used by `just ui-e2e`. The API-key lane additionally runs `--project=auth` using the `just ui-e2e-auth` bootstrap and feature configuration. Live external LDAP/OIDC providers and the separate Kubernetes crash-recovery lane are outside this rendering change's local qualification.

Go validation uses the repository builder image (`go test -race ./...` inside the container) and the real `TestJobsListIncludesLastRuns` integration scenario. Never build the Go application on the host.

#### Initial facelift qualification, 2026-10-01

- ESLint and all 414 UI tests passed across 57 test files.
- The containerized production image built successfully. Its embedded UI passed 83 browser scenarios: 62 default, 16 light and five network-recovery checks. All six regenerated Linux visual baselines were compared without snapshot updates. Accessibility, mobile-console and performance gates passed in both themes.
- API-key authentication passed eight dark and eight light scenarios, including viewer/runner login, scoped access, replay permissions, incidents and hold release. Every browser lane used zero retries and reported zero skipped, flaky or failed tests.
- Containerized `go test -race ./...`, `go vet ./...` and the real run-history integration scenario passed.
- The production asset check passed with unchanged budgets: largest JS chunk 1,228.26 KiB raw / 358.42 KiB gzip; all 15 route assets, including fonts, 2,590.04 KiB raw / 782.41 KiB gzip.
- A live uncached task confirmed that the running electron travels around the node perimeter; dark/light jobs and system views and a live run were rendered for inspection.

Browser JSON reports are retained in the ignored `.tmp/facelift-e2e-results.json`, `.tmp/facelift-auth-results.json` and `.tmp/facelift-auth-light-results.json`. Render previews and the electron probe are in `.tmp/facelift-previews/`.

#### Exploratory QA follow-up

- F1: Graph height uses unscrolled layout coordinates and changes only with loading/layout measurement or window resize. Ordinary main-panel scrolling can reach and open the receipt without growing the graph.
- F2/F8: Structured logs use the theme foreground; both task error banners use the failed glyph and danger color. The terminal keeps its readable dark instrument surface in either theme.
- F3/F4: JobDefs constrains its grid/editor widths, wraps its header and contains long lines in CodeMirror's own scroller. Its tabs use the shared underline style. JSON step parsing accepts the same scalar/list `next` and `dependsOn` forms as YAML; invalid forms name the step and field. The three reported repository examples are covered through live lint, diff, apply and persisted graph checks.
- F5: All seven webhook controls have stable IDs and associated labels. Browser coverage drives label-based entry and keyboard submission.
- F6/F7: The footer and palette describe navigation search and its supported destinations. Datasets uses exact active matching so Holds is the sole active destination on `/datasets/holds`.
- Dialogs have accessible descriptions, and a completed live-log source badge describes where the logs were collected. The documentation index includes this page.

`ui/e2e/facelift-qa.spec.ts` runs six regression journeys in each theme. It uses real runs and REST endpoints, checks computed log contrast for both renderers, and drives scrolling, editing, focus and keyboard submission through the browser. `test/jobdef_scalar_edges_test.go` independently qualifies the JSON REST edge forms and named invalid-field diagnostics.


Follow-up validation uses `caesiumcloud/caesium:facelift-qa-fixes-20261001`, built with `just tag=facelift-qa-fixes-20261001 build-release`. ESLint, 414 UI tests, the production asset budget, containerized Go race tests and vet passed. All six Linux screenshot baselines matched without updates. The two new REST integration scenarios passed, including all three documented examples and both invalid-field diagnostics.

A subsequent clean-server run passed all 12 QA journeys with zero retries, skips, flaky or failed tests. The broader zero-retry browser attempt reported 80 passes, 10 failures and five unrun dependency-blocked recovery tests. All 12 QA regression journeys passed within that attempt. Failures included unexpected live-event stream closures, incomplete run/lineage rendering and a mobile empty-table overflow assertion; server logs also recorded database deadline errors. These results do not qualify the complete suite, and their relationship to the changes is unresolved. No error allowlist, timeout or assertion was weakened. The original 83-test qualification above predates these QA corrections.

Follow-up reports and failure traces are retained under ignored `.tmp/facelift-qa-fixes-*` artifacts. The original exploratory QA server and its evidence remain untouched.

#### PR review follow-up

- Keep empty-fleet authoring navigation inside the router so API-key sessions survive. Omit the unsupported job-origin subtitle rather than labeling every pipeline manual.
- Preserve identifier case in buttons, badges and job headings. Mute zero DAG counters and use the failed glyph in replay errors as well as the already-corrected task and log banners.
- Align late-mounted CSS animations through their actual Web Animations start times. Clamp future run starts to the current strip edge, move marks with transforms, and unsubscribe archived strips and terminal timelines from clock updates. Unstarted terminal tasks use recorded lifecycle times and terminal captions instead of live ghosts.
- Include fonts in the performance harness asset directory. Remove duplicate run/time labels in the latest overlay and compare picker. Select light/dark CodeMirror themes from the resolved console theme.
- Unposted add-on 1: Add safe `formatUTCTime` at all five reported sites, plus the older millisecond log timestamp and UTC clock. Unit tests cover invalid input; browser coverage injects malformed timestamps into all affected route views.
- Unposted add-on 2: Delete the unused sparkline and its tests; the design handoff replaces it with the run strip.
- Unposted add-on 3: Honor `EmptyState.icon`, remove the obsolete `UTCClock.hideDot` prop and its vacuous test, and assert custom empty-state artwork is rendered.
- Unposted add-on 4: Choose metadata chips with explicit `idChip` props across receipt, task and diff cells. A shared renderer keeps empty and `None` values as plain text, independent of display-label spelling.

`ui/e2e/facelift-review.spec.ts` adds six journeys in each theme. Payload, clipboard-capability and animation probes are labeled synthetic; their supporting runs use real REST execution. API-key coverage separately checks authoring navigation without a document reload and performs lint against the real authenticated endpoint.

Review validation uses `caesiumcloud/caesium:pr617-review-20261001`, built with `just tag=pr617-review-20261001 build-release` from an isolated copy of tracked sources and the new review files. ESLint, all 429 UI tests across 59 files, asset budgets, containerized documentation guardrails, Go vet and golangci-lint passed. The performance harness and comparator's 133 Python tests passed. All 28 focused browser journeys passed with one worker and zero retries, skips or flaky tests. The previous push's CI lint simplification and ambiguous wide-DAG node selector are corrected without changing their behavioral assertions.

The expanded four-worker pass finished with 41 passes and six light-theme failures. Both mobile journeys, the real 18-node wide DAG, both adversarial contrast probes and all six existing Linux screenshot baselines passed without updates. Failures affected light-theme run/navigation/copy journeys, a task-panel accessibility journey, JobDefs and a clock check; observed diagnostics included absent route content and incomplete event streams. The successful one-worker focused run does not qualify the full concurrent suite. A separate 107-test attempt against the preceding review image was stopped after the drawer shortcut regression was discovered, then the image was rebuilt with its correction. No assertion, error allowlist or timeout was weakened.

The production budget reports largest JS 1,231.33 KiB raw / 359.36 KiB gzip and total route assets 2,591.93 KiB raw / 783.21 KiB gzip, including fonts. The final release manifest is `sha256:ec21c634ff0b9d210897b50afdfa5ee5fb0536bd009e3b1fd0bce3a5f34e4db8`. Build-source hashes are retained in `.tmp/pr617-review-build-manifest.json`; product sources match the files qualified by the final release.

The final image also passed all 18 API-key journeys: nine dark and nine light, including the empty-fleet authoring action and its authenticated lint request. Both lanes used zero retries and reported no skips or flaky tests. Final focused, expanded and authentication reports are retained under `.tmp/pr617-review-*`.

The shared Docker filesystem exhausted its free space during a separate Go check; the successful rerun stored compiler temporary files and caches in the workspace. A subsequent visual/scale attempt reported four passes and three failures caused by missing run-page content and browser socket errors. Those failures are retained under `.tmp/pr617-review-*`; they are not counted as passes. Owned browser dependencies were moved into the workspace to reduce Docker disk use. No foreign images, volumes or containers were pruned.

#### Visual polish follow-up

See [Operator console polish](#polish-pass-pr-617) for the thirteen visual findings, behavior changes, before/after screenshots, and the current qualification boundary. Earlier test results above remain evidence for their recorded revisions.

See [Handoff and motion audit](#handoff-and-motion-audit-pr-619) for the comparison with the original bundle, restored timeline instruments, live-state correction, representative screenshots, and a real-run motion recording.

## Polish pass (PR #617)

This pass keeps the console's mono typography, warm ink, cyan/gold status language, flat navigation, existing permissions, and prior QA/review fixes. It improves how the existing data is arranged and explained.

The findings were reproduced on `4d5b91f3dbaac22f111346c2af04557118d1147c` using `caesiumcloud/caesium:pr617-review-20261001` at localhost:8083 before source edits. The navigation probe recorded Jobs and then Triggers both at `scrollTop=500`.

### Finding to fix

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

### Representative before / after

These are unedited browser screenshots with real API data, not mockups. The gallery covers desktop and phone layouts in both themes. The gallery pairs use the same seeded backend inventory: the original 4d5b91f3 frontend and the final production frontend. Wall-clock timestamps and elapsed ages can differ between captures. The original pre-edit matrix is also retained locally.

| Surface | Before | After |
| --- | --- | --- |
| Jobs and sidebar · dark · 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/jobs-dark-1440-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/jobs-dark-1440-after.png) |
| Run header and timeline · light · 1280×800 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/run-light-1280-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/run-light-1280-after.png) |
| Job graph/navigation · dark · 390×844 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/job-dark-390-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/job-dark-390-after.png) |
| Empty datasets · light · 1280×800 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/datasets-light-1280-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/datasets-light-1280-after.png) |
| Atoms · dark · 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/atoms-dark-1440-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/atoms-dark-1440-after.png) |
| Stats · light · 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/stats-light-1440-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/stats-light-1440-after.png) |
| JobDefs · light · 390×844 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/jobdefs-light-390-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/jobdefs-light-390-after.png) |

### Qualification

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

### Search follow-up

The terminal-like footer obscured its navigation-search action: only the narrow left prompt opened it, while desktop results stretched across the content area. The entire 44 px footer is now a labeled search button with a search icon and platform shortcut. Cmd/Ctrl-K and `:` remain supported. Both desktop and phone use the same bounded dialog, with a reserved close-button area, quieter selected rows, consistent metadata, short IDs to distinguish duplicate names, and visible keyboard hints. Search accepts full IDs as well as names. Escape, Close, and outside click restore focus to the footer; resizing retains the query and input focus.

These captures compare the production frontend at `eca55d81` with the search follow-up against the same real backend. The broader qualification limits above still apply.

| Surface | Before | After |
| --- | --- | --- |
| Search button · dark · 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/search-closed-dark-1440-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/search-closed-dark-1440-after.png) |
| Search dialog · dark · 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/search-open-dark-1440-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/search-open-dark-1440-after.png) |
| Search dialog · light · 390×844 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/search-open-light-390-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-polish-617/search-open-light-390-after.png) |

The search follow-up passed containerized ESLint, all 433 unit tests, the production build and unchanged bundle budgets, and the documentation guardrail. Twelve focused browser checks passed with one worker and zero retries across both themes, covering full-bar clicks, both keyboard shortcuts, focus containment/restoration, dismissal, viewport changes, full-ID navigation, the existing mobile journey, copying, and reduced motion. Direct screenshots were inspected at all three target sizes in both themes, including long results and no matches; six scoped axe scans of the open search dialog reported zero WCAG 2.0/2.1 A/AA violations. These scoped scans do not establish whole-application compliance. Local receipts are under `.tmp/pr617-search/`.

A second regression selection passed 8/8 with zero retries: navigation-search destination/active-route checks in both themes and all six unchanged Linux visual baselines. Total focused browser coverage for this follow-up is 20/20 passing; it does not supersede the broader failures recorded above.

## Handoff and motion audit (PR #619)

This follow-up restores the Standard bundle's time instruments and visible motion while keeping the QA, review, responsive-layout, status-language, and search corrections in PR #617.

### Comparison basis

The audit read the supplied `Caesium Operator Console Facelift.zip` handoff README, design specification, route guide, and functional invariants, and rendered the chosen Standard references in both themes. Retired concepts were excluded. Source before this follow-up was `705ad583d9d2fb6cde8ba545ddff35c14a0ca683`.

Three local surfaces were distinguished: port 8080 still serves the original `facelift-qa-20261001` embedded frontend; port 8083 serves the `pr617-review-20261001` backend and its older embedded frontend; port 5175 serves the current production frontend against that real backend. The before/after gallery below uses the 705ad583 frontend and this follow-up against the same backend. Counts and wall-clock ages can change as real qualification runs execute.

The bundle's sample fleet has frequent recent runs and a multi-voter cluster. The local fleet includes many historical-only jobs and a standalone server. Those differences were preserved rather than replaced with invented activity or membership.

### Findings and dispositions

| Area | Audit result and action |
| --- | --- |
| Fleet timeline | Restored aligned four-tick gridlines, the instrument surface, one shared beating clock marker, 10 px glowing running bars, and duration-height marks. Added labeled 15-minute, 1-hour, and 24-hour windows persisted in the URL. Header and row coordinates align exactly. |
| Historical-only and empty rows | The prior polish correctly separated old runs from recent time coordinates, but reduced their marks to uniform squares. Restored duration heights within an explicitly ordinal Older strip, with a consistently anchored caption. Empty rows stay quiet. Up to ten actual runs remain available; missing dates/durations stay unknown. |
| Live history wiring | Reproduced a real run-start event changing status while the strip stayed archived and retained the previous completion time. Merge run fields only for the same run identity; reconcile the latest run into history immediately and coalesce authoritative list reads across replayed event bursts. No reload is required to see a live bar or its terminal replacement. |
| Execution timeline | Restored a continuous grid, top-axis labels, one live cursor, stronger bars, UTC-aligned glow, and smooth width changes. Keep actual duration geometry, short-run ticks, endpoint labels inside the chart, sticky mobile labels, readable duration/reason annotations, and terminal stability. |
| Atom artwork | The small header mark had inherited thin large-logo strokes. Apply the bundle's compact optical proportions at sizes up to 32 px and stronger large-logo strokes/nucleus. Real voter count, liveness, quorum, and leader treatments remain intact. |
| Oscillators | Strengthened the header sine to the reference stroke weight. The login sine previously collapsed into a tiny central viewBox; it now repeats across the full viewport at a stable wavelength. Reduced motion paints a flat line. |
| Motion contract | Retained the existing UTC phase system, 1-second clock beat, 2-second gold/glow beat, 6-second task electron, and 22/30/38-second atom orbits. New live bars join the same clock. No route entrance animation was added. Reduced motion disables the new glow and duration transitions as well as existing instruments. |

### Handoff coverage and deliberate differences

| Handoff surface | Current disposition |
| --- | --- |
| Shared shell, typography, status, IDs | Self-hosted Sometype Mono, warm text/cool panels, flat shell, glyph-plus-word statuses, copy chips, and visible keyboard focus remain. Prior legibility and sidebar-column corrections take precedence over literal sample spacing. |
| Jobs and job detail | Fleet instrument restored; readable name columns, wrapped filters, visible actions, mobile graph framing, latest-run access, and dialog navigation retained. Unsupported source provenance is not labeled manual. |
| Run, tasks, fan-out, logs, receipt | Task electrons, live edges, cached/queued/skipped treatments, output edges, partition density, resizable task panel, themed logs, and receipt actions remain. Timeline annotations require more height than the sample's 36 px rows. Receipt availability is not mislabeled as a signature. |
| History, YAML, cache, compare, blame | Existing shared dialogs, CodeMirror theme, ID chips, status/duration comparisons, and blame evidence remain. Historical cache ratios, manifest digests, first-bad-run attribution, and a complete timestamped commit history cannot be supplied by the current APIs. |
| Triggers, Atoms, JobDefs | Flat lists, real next-fire countdowns, readable command arguments/raw disclosure, shared authoring theme, and accessible creation controls remain. Redundant image/name columns and unsupported used-by counts are not restored from the sample. |
| Stats | Thin theme-aware trends/gridlines remain. Prior fixes expose failure counts and distinguish atom identity, and separate range-controlled analysis from current/24-hour KPIs. |
| System and consoles | The live atom, flat metrics, node inventory, database/log entry points, and degraded-state handling remain. Unknown build SHA, raft term/index, last-contact times, and leader resource usage are not fabricated. |
| Datasets, Holds, Contracts, Lineage | Shared headers, appropriate empty states, graph/node treatments, and feature gates remain. The dataset observation axis keeps its own label when sharing the history primitive. Populated and incident-enforcement flows need further visual qualification. |
| Incidents and approvals | Existing feature-gated activity timeline, status words, and approval permissions remain; no new live qualification is claimed here. |
| Login, 404, search | Atom and oscillator fidelity improved. The clearer full-width Search button and bounded accessible dialog intentionally replace the bundle's terminal-like expanding footer. Existing API-key/SSO forms, 404 action, focus restoration, and shortcuts remain. |

### Rendered evidence

Screenshots and recordings remain available in the immutable PR review snapshot linked below. Binary review evidence is removed from the final source tree so a squash merge does not add it to `master`.

Reference renders contain the bundle's sample data. Application screenshots are unedited browser captures of real API data, except the explicitly synthetic login gate used only to inspect its layout. API authentication behavior is not established by those login images.

| Surface | Reference / before | After |
| --- | --- | --- |
| Fleet instrument reference | [Standard reference](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/reference-dark-jobs.png) | [24-hour real history, light](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/history-light-1440-after.png) |
| Historical fleet, dark, 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/jobs-dark-1440-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/jobs-dark-1440-after.png) |
| Terminal execution, light, 1280×800 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/run-light-1280-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/run-light-1280-after.png) |
| Live execution | [Standard reference](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/reference-dark-run.png) | [Real running job](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/live-execution-after.png) |
| Phone history, dark, 390×844 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/jobs-dark-390-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/jobs-dark-390-after.png) |
| Login, dark, 1440×900 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/login-dark-1440-before.png) · [Standard reference](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/reference-dark-login.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/login-dark-1440-after.png) |
| Login, light, 390×844 | [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/login-light-390-before.png) | [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/login-light-390-after.png) |

[Watch the real-run motion recording](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/live-instruments.webm): a REST-triggered uncached three-step job updates the fleet strip, then shows the execution cursor, running bar, and task electron. The API subsequently reported this recorded run as succeeded. [Live fleet still](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/live-fleet-after.png).

### Qualification and limits

The final production frontend was built inside the existing Playwright Linux image and served against the unchanged release backend. No Go application source, permission model, or API response schema changed. The raw capture matrix and receipts are under `.tmp/pr617-motion/`; representative artifacts are linked above from the immutable PR review snapshot.

- ESLint, 436 unit tests across 60 files, TypeScript production compilation, and unchanged asset budgets passed. Largest JS: 1,237.28 KiB raw / 362.75 KiB gzip; all route assets: 2,599.89 KiB raw / 787.88 KiB gzip.
- Direct screenshots cover 1440×900, 1280×800, and 390×844 in both themes. Checks include real running/terminal jobs, historical-only/empty history, missing values, and the synthetic login layout. The earlier polish's populated/filtered-empty route captures remain supplemental evidence for unchanged surfaces.
- Twelve scoped axe scans of Jobs and run-page main content across the size/theme matrix reported zero WCAG 2.0/2.1 A/AA violations. This is scoped automated evidence, not a whole-console accessibility-compliance claim.
- The final focused browser pass passed 26/26 with one worker, zero retries, and no skips. It covers real live-to-terminal updates, UTC phase, reduced motion, all three viewport widths, window persistence/alignment, section scroll reset/Back restoration, mobile task context, sidebar/filtered-empty behavior, search/copy interactions, and six unchanged Linux visual baselines.
- Containerized `go test ./internal/guardrails/... -count=1` passed, including the documentation index guardrail.

The first four-test motion pass caught the live-history timing defect in light mode (three passed, one failed); the trace exposed a burst of list reads during event replay. After the correction, both real-run journeys and both layout journeys passed. Two login assertions then incorrectly treated a zero-height stroked SVG path as a visible layout box; they were corrected to assert the painted stroke and reduced-motion display state. Failed attempts remain in the local receipts. An attempted pull of a new Node image exhausted Docker's shared disk; validation used the already-installed image without pruning foreign resources.

Populated Datasets/Holds/Contracts, feature-gated incident screens, multi-node/degraded cluster operation, external SSO providers, and the full concurrent browser suite remain outside this pass. Existing broad-suite failures documented in [the polish report](#polish-pass-pr-617) are not erased by focused validation here.

### Fleet column spacing follow-up

Give the timeline more of the flexible desktop width. At 1440 px, the name column changes from 436 to 238 px and history from 264 to 462 px; at 1280 px, names change from 336 to 220 px and history from 204 to 320 px. Header/row alignment, full-name tooltips, fixed status/time/action columns, and the phone layout are preserved. [Before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/spacing-dark-1440-before.png) · [After](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/spacing-dark-1440-after.png).

Direct captures cover both themes at 1440×900, 1280×800, and 390×844, including long names. Containerized lint, production build/budget checks, and four existing browser alignment/responsive checks passed with zero retries. No new unit tests were added for this CSS-only adjustment. Local measurements and captures are retained in `.tmp/pr617-spacing/`.

### Quorum placement and local Kubernetes follow-up

The quorum count now sits below the large System atom, with tabular numerals, a stronger foreground, and the label “reachable / total voters.” Orbit paths no longer cross the count. Membership, liveness, leader marking, and reduced-motion behavior are unchanged.

| View | Before | After |
| --- | --- | --- |
| System, dark, 1440×900 | [Prior standalone capture: count inside the orbit](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/system-quorum-before.png) | [Three-voter Kubernetes cluster: count below](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/system-quorum-dark-1440.png) |
| System, light, 390×844 | — | [Phone layout](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/system-quorum-light-390.png) |

These captures use different real deployments: the earlier standalone server has one voter; the new cluster has three. The count is not mocked. [Watch the cluster atom and a real Kubernetes run](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/kubernetes-quorum-live.webm).

The current embedded release is running at **http://localhost:8084/system** (Jobs: **http://localhost:8084/jobs**). Port 8080 remains the original QA build. The new runtime uses the isolated Docker-backed kind cluster `caesium-ui-617`, Helm release/namespace `caesium-ui`, and three persistent Caesium replicas placed on three separate Kubernetes nodes. A loopback NodePort mapping serves port 8084 without a port-forward process. The default kubectl context remains `docker-desktop`; all deployment commands used the explicit kubeconfig below.

```sh
kubectl --kubeconfig .tmp/pr617-k8s/kubeconfig -n caesium-ui get pods -o wide
curl -fsS http://localhost:8084/health
```

Image `caesiumcloud/caesium:pr617-k8s-20261002` was compiled with `just tag=pr617-k8s-20261002 build-release`, from `e146788b` plus the System count placement change. Its local image index is `sha256:d68919c1a95279d8b3bca5bf035a3b65b828b6de7e47235ea80e7f5e7b456330`. Build inputs, Helm values, kind config, health/pod inventories, browser checks, screenshots, and execution receipts are retained in `.tmp/pr617-k8s/`.

Two manually triggered jobs are available: `k8s-branching-demo` (three successful tasks and one skipped branch) and `k8s-live-demo` (a three-step job with 30 seconds of heartbeat output). Both use the real Kubernetes engine and `alpine:3.23`; they were linted using the release CLI and applied through the REST API. No recurring schedules were added.

Validation passed: containerized ESLint, 10 existing System unit tests, production compilation/asset budgets, documentation guardrails, Helm lint/render, the existing live System Playwright test, and a real Kubernetes live-to-terminal execution including the visible terminal status. The 11 served entry assets byte-match the current production build. Directly reviewed captures cover 1440×900, 1280×800, and 390×844 in both themes. Browser checks confirm three reachable voter electrons, one leader marker, motion that stops under reduced motion, count placement below the SVG, and no main-panel horizontal overflow.

The node inventory has a remaining presentation issue: configured DNS seeds are listed separately from their IP-addressed raft members. Depending on the responding replica, the Nodes KPI can show 3/3, 3/4, or 3/5; supplementary rows are explicitly unknown, while the actual quorum remains 3/3. The initial visual script incorrectly assumed that inventory row count equals voter count; the corrected check verifies three current voters and requires extra DNS seed rows to remain unknown. No backend identity logic was changed. Healthy multi-node operation and Kubernetes task execution are qualified here; failover, partition recovery, populated data/incident surfaces, and storage stress remain outside this follow-up.

Docker's shared disk was full. With explicit user approval, unused build cache was cleared; containers, images, volumes, and existing clusters were preserved. Free space remains low (about 630 MiB at the final runtime check), so this is a small demonstration cluster, not a stress-test environment. The new cluster is intentionally left running.

### Shared shortcut keycaps

Run actions, the search footer, and search-dialog guidance now use the shared `Kbd` primitive. Command/dropdown shortcut primitives delegate to it too. Every hint uses the same 20 px height, 11 px mono type, border, background, and foreground. Sidebar shortcuts appear only in a tooltip on mouse hover or keyboard focus, keeping the resting navigation to labels and right-aligned counts. Tooltips show sequences such as `g › j`, expose “Keyboard shortcut: G, then J” to assistive technology, and dismiss with Escape. Modifier combinations retain `⌘ K` / `Ctrl K` and `Alt R`. The phone drawer reserves room above the first navigation row for its close button.

Hints remain separate from interactive controls and do not add tab stops. Decorative hints are excluded from control names; the search dialog's standalone keyboard instructions remain available to assistive technology. Existing key handlers, permissions, focus behavior, and disabled-button opacity are unchanged.

[Before, desktop dark](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/shortcuts-dark-1440-before.png) · [After, desktop dark](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/shortcuts-dark-1440-after.png) · [Shortcut tooltip](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/shortcuts-tooltip-after.png) · [Phone drawer, light](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/shortcuts-light-390-after.png) · [Search guidance](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/shortcuts-search-after.png).

Direct before/after review covers run actions and search at 1440×900, 1280×800, and 390×844 in both themes, plus both phone drawers. Read-only synthetic counts cover four digits, zero, and absent values without writing records. Browser checks verify shared keycap geometry, aligned count columns, absence of inline sidebar keycaps, hover/focus tooltip discovery, Escape dismissal, a clear drawer close button, run navigation/replay-dialog keys, and desktop/mobile `g` navigation sequences. Keycap text/background contrast measures 8.86:1 dark and 6.99:1 light; these measurements do not qualify the entire interface.

ESLint, all 436 unit tests, production build/budgets, documentation guardrails, six existing search browser journeys, and both updated sidebar browser journeys pass. Search ran against the Kubernetes-backed preview; the sidebar fixture ran against the existing Docker-backed review server because its job fixture uses the Docker engine. The first search pass exposed test timing races: it measured separate boxes and clicked the backdrop while the opening animation was still running. The regression now waits for full dialog opacity before those operations; geometry, dismissal, focus, keyboard, and resize assertions are retained. Raw captures and results are under `.tmp/pr617-shortcuts/`.

The local Kubernetes release uses `caesiumcloud/caesium:pr617-shortcut-tooltips-20261002`. To fit the remaining Docker disk space, the unchanged release compile commands ran inside the existing builder image with build/temp/output directories mounted from the host. The exported Alpine release's application layer was replaced with that container-built binary and linked libraries in an OCI archive; base layers, runtime user, and entry point were retained. No host Go compilation or Go source changes were involved. The UI remains embedded in the release binary; port 8084 serves the updated cluster.

The local rollout encountered two operational issues: one replica retained obsolete peer addresses in its discovery cache, and disk exhaustion interrupted an init container. The discovery cache was backed up and refreshed from observed live membership, then the replica restarted; raft/database data were retained. Removing only this cluster's superseded image tags and import aliases recovered space, and the interrupted pod was recreated. These are runtime recovery notes, not backend fixes or failover qualification. Final checks confirm healthy database access and 3/3 reachable voters on each replica, and all 11 served entry assets match the production build. Foreign runtimes and the default kubectl context were preserved.

### Follow-up: fluid execution and terminal snapshots

Reproduced against `0b775140` on the three-replica Kubernetes release at port 8084. The reported `k8s-live-demo` run `657ca32f-aeaa-482f-9d88-f3bc96667809` is persisted as **failed**, with `database is locked`; its stream task remains recorded as running and its final task as pending. The old UI animated the stream forever and extended its duration using the current clock. No database records were rewritten.

| Finding | Correction |
| --- | --- |
| A failed run still looks active | Parent terminal state now stops node orbits, flowing edges, live cursors, and task timers. Incomplete task records become explicitly **unconfirmed / outcome unknown** or **did not start** in counters, nodes, timeline, and task details. The recorded task status remains available in the explanation. Observed duration is bounded by the last task update and parent completion, not inferred completion. |
| Choppy execution timeline | A scoped animation-frame loop updates chart geometry between clock ticks without rendering React or relaying out the DAG every frame. Bars, cursor, ghosts, and grid share an interpolated time scale. Live charts reserve headroom; entering ticks stay within the plotting area. Terminal geometry remains exact, including sub-second runs. |
| Graph motion feels unstable | Layout is computed from topology, independent of task status and selection updates. Removed the duplicate group/path dash animation and slowed the single flowing path. Unstarted timeline rows no longer sort before already-started work by creation time. |
| Missed events leave stale state | Detail queries reconcile authoritative REST state every five seconds even with a connected event stream. Late task events cannot revive a terminal snapshot. Terminal payloads retain their actual outcome and completion metadata. |

[Reported run before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/execution-failed-before.png) · [Same run after](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/execution-failed-after.png) · [Phone, light](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/execution-failed-phone.png) · [Real live execution](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/execution-live-after.png).

Validation: ESLint, 444 unit tests, production build/budgets, and the focused `dag-execution-motion.spec.ts` browser journey. The browser drives real Kubernetes jobs, samples frame-by-frame bar movement (24 distinct widths in 24 samples), suppresses SSE updates to verify REST reconciliation, observes terminal stopping without reload, preserves graph framing, and checks reduced motion. A separately labeled synthetic incomplete snapshot exercises the rare parent/task mismatch without modifying server rows. The original failed run is also inspected directly through its real API and rendered route. Desktop 1440×900, 1280×800, and phone 390×844 captures cover both themes, mobile overflow and axis endpoints; screenshots were reviewed directly. A fresh execution of the user's same job succeeds. Raw API receipts, browser results, before/after screenshots, and an uninterrupted live-to-terminal video are in `.tmp/pr617-dag-motion/`.

This is a UI correction, not a fix for the recorded backend database-lock failure. Populated incident/fan-out failure recovery and large-DAG performance remain unqualified by this focused pass. Existing permission, keyboard/copy, theme, and reduced-motion behavior is retained; no new accessibility-compliance claim is made.

Local delivery: image `caesiumcloud/caesium:pr617-dag-motion-20261002` is embedded and served on port 8084 by all three replicas. Each replica passes database health and reports 3/3 reachable voters; all 14 entry assets match the production build. A final run of the same `k8s-live-demo` job (`a9a48115-face-4a5b-8ef3-a8f62d95025e`) succeeds on the deployed image and visibly stops without reload. Containerized documentation guardrails also pass. The rollout used the existing builder/OCI packaging procedure with host-mounted build caches and removed only superseded images belonging to this local release.

### Follow-up: connect jobs to their executions

The path from Jobs to a timeline previously relied on a small `Latest overlay` timestamp link, and an execution's main navigation offered no explicit route to the job overview. This pass keeps the graph-first job overview and existing execution routes while making their relationship visible.

| Surface | Navigation change |
| --- | --- |
| Jobs list | The Last run value is a direct link to that exact execution, with an arrow and an accessible name. The job name continues to open its overview. Column widths and the history strip are retained. |
| Job overview | A dedicated active/latest-run summary shows its copyable ID, status, full UTC timestamp, elapsed time, and cache summary. **View run / View live run** is the primary inspection action; the caption names timeline, logs, and receipt. **Choose run** opens other executions. The first-run empty state points to the existing Trigger action. |
| Shared context | **Job overview** and **Run history** occupy the same navigation position on job and execution pages. The active **Execution** item distinguishes a specific run from its parent job. The run identity presentation is shared; paused remains a job property, while execution outcome belongs to the selected run. |
| Exact historical run | **Switch run** lists timestamps, IDs, statuses, and durations, with the inspected run pinned independently of recent history. Up to eight alternatives appear, plus a route to complete history. Viewing history, using Back, copying an ID, or switching runs never launches a new execution. History rows now show the full UTC date as well as time. |

[Job overview before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/job-run-before.png) · [Job overview after](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/job-run-after.png) · [Execution navigation](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/run-navigation-after.png) · [Run picker on phone](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/run-picker-phone.png).

Validation covers the real Jobs → overview → exact execution → historical execution → overview flow at 1440×900, 1280×800, and 390×844 in both themes. The browser checks zero/one/multiple-run states, exact URLs and IDs, direct entry/reload, keyboard selection and dismissal, copy behavior, the existing A history shortcut, browser Back, mobile reachability, and absence of accidental run creation. Four existing regression journeys also pass: mobile graph/timeline context, case-sensitive identifiers and Alt-R/navigation chords, malformed timestamps, and job-scoped history after re-run. ESLint, all 444 unit tests, and production build/budgets pass. Direct screenshot review includes a populated picker with the original failed run and subsequent successful runs. Evidence is under `.tmp/pr617-run-navigation/`.

No scheduler, API, permission, run-state, or animation logic changes in this pass. Populated incident screens, external authentication providers, and large-history performance remain outside this focused navigation qualification.

Local delivery: image `caesiumcloud/caesium:pr617-run-navigation-20261002` is served on port 8084 by three healthy replicas with 3/3 reachable voters. All 14 entry assets match the production build. A final real execution of `k8s-live-demo` (`d90f8e48-b992-46b9-8d96-b7bcae2ce6f2`) verifies View live run, exact execution identity, the run picker, natural completion, and return to the job overview with its succeeded outcome. Containerized documentation guardrails pass. The existing Kubernetes release remains running for inspection.

### Follow-up: history alignment and consistent counts

Reproduced on `a5de859f` at port 8084: relative ages shifted the run-history status and duration columns, Datasets had no count beside Holds' zero, and the tallest archived run marks began only 3px below their row boundary.

| Finding | Correction |
| --- | --- |
| History rows shift with text length | Shared grid tracks align timestamps, ages, statuses, durations, parameters, IDs, and links. The dialog has room for those columns on desktop and reflows into consistently ordered rows on phones. Copy and exact-run links remain available. |
| Missing Datasets count | The sidebar reads the existing paginated dataset endpoint's total, fetching one record. A confirmed empty result displays **0**, matching Holds. Loading, forbidden, unavailable, feature-disabled, and scoped states do not invent a zero or request an unauthorized global count. |
| Crowded bars and ambiguous “Older” | Recent and archived marks share a baseline with at least 14px above the tallest terminal mark in a 64px row. Duration scaling is unchanged. Archived history keeps its separate ordinal treatment and now says **Outside 15m** (or the selected window), with an explicit **View history →** link to that job's history. Exact timestamps and the oldest age remain in accessible descriptions/native hover detail. |

[Strip before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/history-strip-before.png) · [Strip after](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/history-strip-after.png) · [Modal before](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/history-modal-before.png) · [Modal after](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/history-modal-after.png) · [Phone, light](https://github.com/caesium-cloud/caesium/blob/0a4f41185cacf86712197ce8d6188531982a56fd/docs/ui-motion-617/history-modal-phone.png).

Screenshots directly reviewed at 1440×900, 1280×800, and 390×844 in both themes. Focused browser coverage opens real executions through the archive link with keyboard navigation and verifies exact identity, aligned columns, mobile bounds, and bar padding in recent/archived modes. Age differences and long durations are explicitly synthetic browser responses over real run IDs; no server timestamps are changed. Separate rendering probes distinguish zero, a multi-digit paginated total, unavailable counts, and the feature gate. Populated dataset lifecycle and external authentication remain outside this focused pass. Raw evidence: `.tmp/pr617-history-polish/`.

Validation: containerized ESLint, all 444 unit tests, production build/bundle budgets, and `go test ./internal/guardrails/...` pass. Both focused browser journeys pass against the container-built UI with zero retries. The initial browser attempt exposed an in-flight route callback at test teardown; waiting for interception callbacks fixes the test cleanup, with no application change required.

Local delivery: image `caesiumcloud/caesium:pr617-history-polish-20261002` is running on port 8084 across three healthy replicas. Fresh membership observations confirm 3/3 reachable voters, and all 14 served entry assets match the build. A final browser check follows the deployed archive link into the history modal and opens its exact real execution and timeline. Only this release's superseded images were retired; foreign resources were preserved.

### Second review pass

This pass addresses the 16 follow-up threads without changing the backend, permissions, animation phases, or duration geometry.

| Finding | Correction |
| --- | --- |
| JobDefs heading checks | Navigation and performance journeys match the actual `JobDefs` heading. |
| Repeated full-history reads | Healthy SSE disables periodic job/full-history reads; run lifecycle events reconcile the summaries. The selected run retains its bounded REST reconciliation. |
| Timeline units | Use seconds, minutes, and hours for long-run ticks while retaining subsecond scaling for short runs. |
| Partition retry remains terminal | Subscribe to the real `run_retried` event, reopen only the matching execution, reject older retry snapshots, and invalidate run/receipt/Why/job history after retry. Late task events still cannot revive a terminal run. |
| Sustained activity starves refresh | Use a fixed 250 ms coalescing window, with teardown cleanup, instead of a trailing debounce. |
| Concurrent completion overwrites latest | Compare start time, falling back to creation time, in both the fleet and individual-job caches. Completion order does not choose the latest run. |
| Identifier capitalization | Preserve mixed-case trigger aliases and database column names. |
| Autofill key event | Guard an absent key before lowercasing it. |
| Incident error symbol | Use the failed glyph and danger foreground consistently. |
| Compare menu nested button | Show a noninteractive abbreviated run ID inside the menu item; keyboard selection and full-ID tooltip remain available. |
| Remaining metadata regexes | Why and Blame declare ID fields explicitly and share empty/None handling. |
| HTTP copy behavior | Webhook URL, logs, and SQL results use the shared copy fallback and report failure without claiming success. |
| Duplicate command formatting | Atoms, job tasks, and Blame share one argument-aware formatter; plain commands and malformed representations remain faithful. |
| Focus and unused CSS | Keep one focus rule and remove the unused duration-bar transition and reduced-motion selector. |
| Binary review evidence | Remove all 61 screenshot/video files from the final tree. Existing galleries link to their immutable PR snapshot; markdown reports remain. |

Focused unit coverage exercises concurrent ordering, explicit retry reopening, bounded refresh under sustained events, native retry-event dispatch, metadata sentinels, argv quoting, and short/minute/hour/day timeline ticks. Browser regression coverage distinguishes real partition execution and copy interactions from explicitly synthetic history/event/capability/error scenarios.

Validation for this review pass:

- Containerized ESLint, 458 unit tests in 63 files, production TypeScript/Vite build, unchanged bundle budgets, and `go test ./internal/guardrails/... -count=1` passed. No Go application source changed.
- Nineteen focused browser scenarios cover both themes, the navigation heading and memory/readiness regressions, real failed-partition retry, native event wiring, clipboard fallback/error feedback, explicit synthetic 8,000-run history and sustained/concurrent events, and synthetic 10-minute/two-hour layouts. Captures were reviewed directly at desktop and phone sizes.
- The earlier complete 17-test pass was green. The expanded pass exposed a test setup race: filling SQL while its initial schema/snippet was still loading could append to the default statement. The copy journey now waits for schema readiness and verifies the editor's full value before executing; both themes and the error-card captures passed the focused rerun. Earlier failure traces remain in `.tmp/pr617-review2/`.
- Browser qualification used a sticky connection to the local cluster leader; the read-only SQL scenario used the existing enabled QA server. Cluster-wide SSE distribution, the full concurrent browser suite, and populated feature-gated incident flows are outside this focused pass. REST reconciliation and existing permission gates remain intact.
- The subsequent CI run exposed missing database-console enablement in the browser test server: both copy scenarios stopped at the disabled schema control. Enable the console only in that server setup so the existing tests exercise the real SQL endpoint. Both themes passed a focused rerun against the current UI and the enabled QA server; application defaults remain unchanged.
- Local captures, test/build receipts, and an archive of the removed review media are under `.tmp/pr617-review2/`. CI collects new browser screenshots in its diagnostics artifact; no new binary evidence is added to the source tree.

### Now-reference alignment

The Jobs clock dot was centered 3 px left of the time-grid boundary. Replace it with a 1 px line below “now” at the same coordinate as the row guides, and give the time reference a synchronized, low-opacity two-second pulse. Reduced motion keeps the line static; archived history stays in its separate ordered lane.

Before/after captures were reviewed at 1440, 1280, and 390 px in both themes. Browser checks measured zero horizontal offset, confirmed the pulse changes opacity without shifting the line, checked reduced motion, and found no main-panel overflow. Lint and the containerized production build/bundle budgets passed. Captures and geometry receipts are retained in `.tmp/pr617-now-line/`; no binary evidence is added to the source tree.

### Final review pass

| Finding | Correction |
| --- | --- |
| Database-console test setup | Keep `just ui-e2e` in parity with the enabled CI browser server. Production defaults are unchanged. |
| Replayed job events and slow fleet reads | Share a 250 ms coalescer, allow an in-flight read to finish, and schedule one trailing refresh for intervening events. Job history walks also consume an AbortSignal when leaving the route. |
| Repeat retry lifecycle events | Prefer the unique event sequence for replay deduplication; successive retries and failures remain separate activity entries. |
| Terminal metadata after retry | Clear omitted completion/error fields for an authoritative same-run snapshot and reject older updates. |
| Compare shortcut | Control the Radix menu's open state so `c` opens the menu and Escape restores focus. |
| Task outcome labels | Preserve cancelled, blocked, and pending labels in the panel, DAG, timeline rows, and legend. Visual status aliases do not rename recorded outcomes. |
| Cross-node discovery | Reconcile the cheap job/latest-run projection every 60 seconds while SSE is healthy. Refresh full history only for a newly discovered run or a changed outcome/completion, rather than every poll. The fleet projection uses the same slow reconciliation interval. |
| Copy inside dialogs | Place the fallback selection inside the active dialog's focus scope and restore focus after copying. |
| Retry activity presentation | Show “Run retried” with the running glyph. |
| Literal metadata values | Only blank values mean absence; preserve literal `none` and use blanks at missing-field call sites. |
| Timestamp ordering | Parse variable-precision timestamps before sorting fleet history and last-run order. |
| Incident links | Use noninteractive abbreviated IDs with full-value titles inside row links; keep copy controls on detail surfaces. |
| DAG keyboard focus | Add an unlayered outline that takes precedence over React Flow's focus reset. |
| Standalone atom state | Distinguish an observed deployment without Raft from unknown health, with no invented voters and a matching accessible description. |
| DAG command display | Reuse the shared argument decoder and formatter, preserving raw malformed/scalar commands and quoted argument boundaries. |

Validation includes 470 unit tests, containerized lint/production build and bundle checks, 151 CI-configuration tests, and the docs guardrails. Focused browser scenarios exercise Compare, dialog copying without Clipboard API, DAG focus/Enter, 900 ms fleet responses during sustained events, successive retries, 500 retained events over paged history, aborting a walk on navigation, and discovery of a real new run with its event deliberately excluded from the stream. Delayed history, event bursts, and browser capability changes are explicitly synthetic; job creation and run execution use the real REST/runtime surface. Screenshots were reviewed directly; captures and receipts stay in `.tmp/pr617-final-review/`.

Cross-node SSE fan-out itself remains a backend concern: a one-minute REST reconciliation interval discovers missed events while preserving the existing event bus and permissions. Populated incident lifecycles remain outside this focused visual qualification.

#### CI follow-up

The full browser suite exposed two runtime issues beyond the focused review pass. SSE event and server-log responses inherited the HTTP server's 30-second write deadline; renew that deadline for each stream chunk, including heartbeats, while retaining the ordinary HTTP limit and a bounded timeout for stalled stream writes. Both real REST streams now have an integration scenario requiring four heartbeats over 45 seconds, plus a real HTTP unit regression through Echo's response wrapper.

A latest-run projection that disagreed with paged history could make each completed history walk schedule another. Bound retries for the same latest-run revision to once per minute; changed revisions and lifecycle events still refresh promptly. Browser coverage verifies both the absence of a feedback loop and a later retry for eventual consistency.

Presentation fixtures now use one fixed browser clock and consistent latest/history snapshots. Sustained-event coverage retains one request handler throughout its gated read; slow-response request bounds use elapsed time rather than assuming workstation timing. The mid-burst update, final update, single in-flight request, concurrent-run identity, and console-error assertions remain enforced. CI follow-up artifacts are retained in `.tmp/pr617-ci-recovery/` and `.tmp/pr617-ci-green/`.

#### Streaming follow-up review

| Finding | Correction |
| --- | --- |
| Task logs still inherit the initial write deadline | Both live paths—the runtime log pipe and scrubbed snapshot tail—use the shared bounded streaming writer. One-shot retained snapshots keep the ordinary response deadline. |
| Server and stream timeout constants can diverge | The API server takes its write timeout from the same constant used by streaming writes. |
| Manual reconnect replays all retained events | Retain the last delivered SSE ID and send it as the reconnect cursor. Reset it for a changed filter or disconnected session; ignore callbacks from replaced sources and cancel superseded reconnect timers. |
| Long integration tests are undercosted | Record 46-second idle-stream and 65-second task-stream costs in the shard timing inventory. Document the parallel idle subtests' reliance on immutable suite configuration without per-test hooks. |

Real integration coverage runs plain and secret-bearing log producers concurrently for 50 seconds, follows both actual live REST responses, checks every output marker and redaction, and requires successful completion. The two idle SSE paths independently deliver heartbeats past 30 seconds. Browser coverage observes native event IDs across an offline interruption and requires cursor resumption without replaying earlier IDs. A loopback relay forwards real backend bytes and severs the established socket, because Chromium's offline flag alone can leave an existing SSE connection open. It also opens the live server-log console through navigation. The coverage collector reads the executed task's retained logs and fails if its actual output is absent; these journeys exercise the changed REST surfaces without lowering coverage thresholds.

Local validation passes: 474 UI unit tests, production build and bundle budgets, Go lint/vet and focused package tests, documentation guardrails, 226 coverage/CI configuration tests, both long-lived integration scenarios, and the two focused browser journeys with zero retries. The three-node local deployment serves the new application build with healthy 3/3 quorum and matching entry assets.

Artifacts and local deployment receipts are retained in `.tmp/pr617-stream-followup/`. This follow-up preserves scheduler behavior, permissions, retained-log responses, and cross-node event distribution.
