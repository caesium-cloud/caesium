# Operator console facelift

Implements the Standard concept from `Caesium Operator Console Facelift.zip`, starting with its handoff README, design specification, route guide, and functional invariants.

The console uses self-hosted Sometype Mono, warm text on cool instrument surfaces, flat lists, word tabs, and status glyphs. The shell has a 44 px header, 208 px sidebar, and 40 px command footer. Cmd-K and `:` open inline desktop navigation search; mobile keeps its dialog and navigation drawer.

The shared UTC clock drives recent fleet history and trigger countdowns. Historical strips and terminal timelines stop subscribing to ticks. Each CSS instrument aligns its own animation to UTC when inserted and resynchronizes after a hidden tab returns. Reduced motion freezes live indicators and replaces the oscillator with a flat line. Identifiers retain their original case and use one copy-chip component with eight visible characters, the full title and clipboard payload, and a one-second confirmation. Plain HTTP deployments use native copy when the Clipboard API is unavailable; if both paths are denied, the chip exposes the full selectable value.

Run pages show alias and start time before the diagnostic ID. Task nodes are 260 × 112 px with status-specific electrons. Timeline rows are 36 px, graph spacing is updated, and contract and lineage nodes are 260 × 80 px. Lineage metadata remains in the node tooltip; its links, freshness and hold overlays remain functional. Job YAML uses the same CodeMirror theme as the authoring editor.

Re-run uses Alt-R; unmodified `r` does not launch work. Global `g` navigation chords consume their second key before route shortcuts run. UTC time formatting accepts malformed or absent timestamps and displays a fallback. Both YAML surfaces follow the resolved light/dark theme, including selections and tooltips.

The fleet strip uses the last 15 minutes, with a labeled last-ten fallback for older or undated history. `GET /v1/jobs` now includes `last_runs[].started_at`, which the existing SQL projection already collected. The integration history test checks this field against a real run.

## Data availability

The facelift does not invent fields missing from the current server:

- Build SHA, raft index/term, last-contact timestamps and leader CPU/memory are unavailable; the console says so. Existing membership, liveness probes, worker counts and latency remain visible. Standalone or stale health does not draw fabricated healthy voters.
- Receipt metadata says pending, available or unavailable based on the receipt query. The receipt API supplies a digest and verification results, not a signature, so the console does not label it signed.
- Atom used-by counts, per-task historical cache hit ratios and manifest digests are not returned by these APIs. Existing creation time, cache inventory/policy/run statistics and reconstructed YAML remain available.
- Blame has snapshot/commit identities and causation evidence, without a complete timestamped commit history or first-bad-run designation. That evidence remains visible without invented times.

Dark `--text-3` lightness is 50%, slightly above the handoff's 48%, because the supplied value measured 4.41:1 on the obsidian surface. No accessibility allowlist was expanded.

## Validation

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

### Initial facelift qualification, 2026-10-01

- ESLint and all 414 UI tests passed across 57 test files.
- The containerized production image built successfully. Its embedded UI passed 83 browser scenarios: 62 default, 16 light and five network-recovery checks. All six regenerated Linux visual baselines were compared without snapshot updates. Accessibility, mobile-console and performance gates passed in both themes.
- API-key authentication passed eight dark and eight light scenarios, including viewer/runner login, scoped access, replay permissions, incidents and hold release. Every browser lane used zero retries and reported zero skipped, flaky or failed tests.
- Containerized `go test -race ./...`, `go vet ./...` and the real run-history integration scenario passed.
- The production asset check passed with unchanged budgets: largest JS chunk 1,228.26 KiB raw / 358.42 KiB gzip; all 15 route assets, including fonts, 2,590.04 KiB raw / 782.41 KiB gzip.
- A live uncached task confirmed that the running electron travels around the node perimeter; dark/light jobs and system views and a live run were rendered for inspection.

Browser JSON reports are retained in the ignored `.tmp/facelift-e2e-results.json`, `.tmp/facelift-auth-results.json` and `.tmp/facelift-auth-light-results.json`. Render previews and the electron probe are in `.tmp/facelift-previews/`.

### Exploratory QA follow-up

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

### PR review follow-up

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

### Visual polish follow-up

See [Operator console polish](operator-console-polish.md) for the thirteen visual findings, behavior changes, before/after screenshots, and the current qualification boundary. Earlier test results above remain evidence for their recorded revisions.
