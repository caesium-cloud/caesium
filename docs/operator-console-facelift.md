# Operator console facelift

Implements the Standard concept from `Caesium Operator Console Facelift.zip`, starting with its handoff README, design specification, route guide, and functional invariants.

The console uses self-hosted Sometype Mono, warm text on cool instrument surfaces, flat lists, word tabs, and status glyphs. The shell has a 44 px header, 208 px sidebar, and 40 px command footer. Cmd-K and `:` open the inline desktop palette; mobile keeps its dialog and navigation drawer.

The shared UTC clock drives the fleet history strip, trigger countdowns, and phase alignment. Animation phases resynchronize after a hidden tab returns. Reduced motion freezes live indicators and replaces the oscillator with a flat line. Identifiers use one copy-chip component with eight visible characters, the full title and clipboard payload, and a one-second confirmation.

Run pages show alias and start time before the diagnostic ID. Task nodes are 260 × 112 px with status-specific electrons. Timeline rows are 36 px, graph spacing is updated, and contract and lineage nodes are 260 × 80 px. Lineage metadata remains in the node tooltip; its links, freshness and hold overlays remain functional. Job YAML uses the same CodeMirror theme as the authoring editor.

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

### Local qualification, 2026-10-01

- ESLint and all 414 UI tests passed across 57 test files.
- The containerized production image built successfully. Its embedded UI passed 83 browser scenarios: 62 default, 16 light and five network-recovery checks. All six regenerated Linux visual baselines were compared without snapshot updates. Accessibility, mobile-console and performance gates passed in both themes.
- API-key authentication passed eight dark and eight light scenarios, including viewer/runner login, scoped access, replay permissions, incidents and hold release. Every browser lane used zero retries and reported zero skipped, flaky or failed tests.
- Containerized `go test -race ./...`, `go vet ./...` and the real run-history integration scenario passed.
- The production asset check passed with unchanged budgets: largest JS chunk 1,228.26 KiB raw / 358.42 KiB gzip; all 15 route assets, including fonts, 2,590.04 KiB raw / 782.41 KiB gzip.
- A live uncached task confirmed that the running electron travels around the node perimeter; dark/light jobs and system views and a live run were rendered for inspection.

Browser JSON reports are retained in the ignored `.tmp/facelift-e2e-results.json`, `.tmp/facelift-auth-results.json` and `.tmp/facelift-auth-light-results.json`. Render previews and the electron probe are in `.tmp/facelift-previews/`.
