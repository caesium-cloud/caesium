# Operator console handoff and motion audit

This follow-up restores the Standard bundle's time instruments and visible motion while keeping the QA, review, responsive-layout, status-language, and search corrections in PR #617.

## Comparison basis

The audit read the supplied `Caesium Operator Console Facelift.zip` handoff README, design specification, route guide, and functional invariants, and rendered the chosen Standard references in both themes. Retired concepts were excluded. Source before this follow-up was `705ad583d9d2fb6cde8ba545ddff35c14a0ca683`.

Three local surfaces were distinguished: port 8080 still serves the original `facelift-qa-20261001` embedded frontend; port 8083 serves the `pr617-review-20261001` backend and its older embedded frontend; port 5175 serves the current production frontend against that real backend. The before/after gallery below uses the 705ad583 frontend and this follow-up against the same backend. Counts and wall-clock ages can change as real qualification runs execute.

The bundle's sample fleet has frequent recent runs and a multi-voter cluster. The local fleet includes many historical-only jobs and a standalone server. Those differences were preserved rather than replaced with invented activity or membership.

## Findings and dispositions

| Area | Audit result and action |
| --- | --- |
| Fleet timeline | Restored aligned four-tick gridlines, the instrument surface, one shared beating clock marker, 10 px glowing running bars, and duration-height marks. Added labeled 15-minute, 1-hour, and 24-hour windows persisted in the URL. Header and row coordinates align exactly. |
| Historical-only and empty rows | The prior polish correctly separated old runs from recent time coordinates, but reduced their marks to uniform squares. Restored duration heights within an explicitly ordinal Older strip, with a consistently anchored caption. Empty rows stay quiet. Up to ten actual runs remain available; missing dates/durations stay unknown. |
| Live history wiring | Reproduced a real run-start event changing status while the strip stayed archived and retained the previous completion time. Merge run fields only for the same run identity; reconcile the latest run into history immediately and coalesce authoritative list reads across replayed event bursts. No reload is required to see a live bar or its terminal replacement. |
| Execution timeline | Restored a continuous grid, top-axis labels, one live cursor, stronger bars, UTC-aligned glow, and smooth width changes. Keep actual duration geometry, short-run ticks, endpoint labels inside the chart, sticky mobile labels, readable duration/reason annotations, and terminal stability. |
| Atom artwork | The small header mark had inherited thin large-logo strokes. Apply the bundle's compact optical proportions at sizes up to 32 px and stronger large-logo strokes/nucleus. Real voter count, liveness, quorum, and leader treatments remain intact. |
| Oscillators | Strengthened the header sine to the reference stroke weight. The login sine previously collapsed into a tiny central viewBox; it now repeats across the full viewport at a stable wavelength. Reduced motion paints a flat line. |
| Motion contract | Retained the existing UTC phase system, 1-second clock beat, 2-second gold/glow beat, 6-second task electron, and 22/30/38-second atom orbits. New live bars join the same clock. No route entrance animation was added. Reduced motion disables the new glow and duration transitions as well as existing instruments. |

## Handoff coverage and deliberate differences

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

## Rendered evidence

Reference renders contain the bundle's sample data. Application screenshots are unedited browser captures of real API data, except the explicitly synthetic login gate used only to inspect its layout. API authentication behavior is not established by those login images.

| Surface | Reference / before | After |
| --- | --- | --- |
| Fleet instrument reference | [Standard reference](ui-motion-617/reference-dark-jobs.png) | [24-hour real history, light](ui-motion-617/history-light-1440-after.png) |
| Historical fleet, dark, 1440×900 | [Before](ui-motion-617/jobs-dark-1440-before.png) | [After](ui-motion-617/jobs-dark-1440-after.png) |
| Terminal execution, light, 1280×800 | [Before](ui-motion-617/run-light-1280-before.png) | [After](ui-motion-617/run-light-1280-after.png) |
| Live execution | [Standard reference](ui-motion-617/reference-dark-run.png) | [Real running job](ui-motion-617/live-execution-after.png) |
| Phone history, dark, 390×844 | [Before](ui-motion-617/jobs-dark-390-before.png) | [After](ui-motion-617/jobs-dark-390-after.png) |
| Login, dark, 1440×900 | [Before](ui-motion-617/login-dark-1440-before.png) · [Standard reference](ui-motion-617/reference-dark-login.png) | [After](ui-motion-617/login-dark-1440-after.png) |
| Login, light, 390×844 | [Before](ui-motion-617/login-light-390-before.png) | [After](ui-motion-617/login-light-390-after.png) |

[Watch the real-run motion recording](ui-motion-617/live-instruments.webm): a REST-triggered uncached three-step job updates the fleet strip, then shows the execution cursor, running bar, and task electron. The API subsequently reported this recorded run as succeeded. [Live fleet still](ui-motion-617/live-fleet-after.png).

## Qualification and limits

The final production frontend was built inside the existing Playwright Linux image and served against the unchanged release backend. No Go application source, permission model, or API response schema changed. The raw capture matrix and receipts are under `.tmp/pr617-motion/`; representative artifacts are committed above.

- ESLint, 436 unit tests across 60 files, TypeScript production compilation, and unchanged asset budgets passed. Largest JS: 1,237.28 KiB raw / 362.75 KiB gzip; all route assets: 2,599.89 KiB raw / 787.88 KiB gzip.
- Direct screenshots cover 1440×900, 1280×800, and 390×844 in both themes. Checks include real running/terminal jobs, historical-only/empty history, missing values, and the synthetic login layout. The earlier polish's populated/filtered-empty route captures remain supplemental evidence for unchanged surfaces.
- Twelve scoped axe scans of Jobs and run-page main content across the size/theme matrix reported zero WCAG 2.0/2.1 A/AA violations. This is scoped automated evidence, not a whole-console accessibility-compliance claim.
- The final focused browser pass passed 26/26 with one worker, zero retries, and no skips. It covers real live-to-terminal updates, UTC phase, reduced motion, all three viewport widths, window persistence/alignment, section scroll reset/Back restoration, mobile task context, sidebar/filtered-empty behavior, search/copy interactions, and six unchanged Linux visual baselines.
- Containerized `go test ./internal/guardrails/... -count=1` passed, including the documentation index guardrail.

The first four-test motion pass caught the live-history timing defect in light mode (three passed, one failed); the trace exposed a burst of list reads during event replay. After the correction, both real-run journeys and both layout journeys passed. Two login assertions then incorrectly treated a zero-height stroked SVG path as a visible layout box; they were corrected to assert the painted stroke and reduced-motion display state. Failed attempts remain in the local receipts. An attempted pull of a new Node image exhausted Docker's shared disk; validation used the already-installed image without pruning foreign resources.

Populated Datasets/Holds/Contracts, feature-gated incident screens, multi-node/degraded cluster operation, external SSO providers, and the full concurrent browser suite remain outside this pass. Existing broad-suite failures documented in [the polish report](operator-console-polish.md) are not erased by focused validation here.
