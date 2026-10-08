# Isolated native Git source fixture

This integration-only, standard-library executable delegates refs and pack bytes
to the full builder's real Git. It does not change Caesium's transport, import
the watcher/importer, fabricate refs/packs, or contribute eligible coverage.
The production watcher uses its ordinary `git://` client. No Git package is
required in the Caesium runtime image.

Build inside the already-loaded, immutable full builder, from the final candidate:

```sh
GOTOOLCHAIN=local GOPROXY=off go build -tags=integration \
  -o /fixture-bin/git-source ./test/fixtures/git-source
```

The collector owns a fresh directory mounted as `/fixture`. Before starting the
main server, run this executable in an owned builder container:

```sh
/fixture-bin/git-source init --repo /fixture/coverage.git \
  --alias "coverage-git-$ID" --source-id "coverage-git-$ID" \
  --image alpine:3.23 --url "git://$GIT_ALIAS:9418/coverage.git"
```

The image reference must already exist in the parent-validated task inventory.
The parent directory must exist and be a real directory. Both repository and
`/fixture/state.json` must be free. Initialization uses native Git to create
branch `main`, a committed `jobs/imported.job.yaml`, and an ignored text file.
The job uses an impossible-date cron, disables caching and emits `initial`.
Initialization never resets/removes an existing repository. Failure leaves the
owned allocation for the collector's checked cleanup.

Serve using the same uninstrumented builder image and executable:

```sh
/fixture-bin/git-source serve --repo /fixture/coverage.git \
  --url "git://$GIT_ALIAS:9418/coverage.git" --listen 0.0.0.0:9418
```

Give this container a collector-owned private network alias `$GIT_ALIAS`, no
published ports, and a read-only `/fixture` mount. The collector must track its
name before allocation and actual immutable ID afterward, with owner/run/lane
labels and checked stop/remove/literal-absence handling. The main server shares
that private network but needs **no repository mount**. The test runner receives
the same fixture directory read/write, allowing native local commits only.

Serve verifies the receipt's initial HEAD and branch, binds the listener, and
emits `{"phase":"git-fixture-ready"}`. Readiness must additionally verify an
actual native Git query over the private network, before Caesium starts:

```sh
git -c protocol.version=0 ls-remote \
  "git://$GIT_ALIAS:9418/coverage.git" refs/heads/main
```

Require the exact initial commit and `refs/heads/main`. Protocol v0 is the exact
request made by production go-git v5.19.2. The wrapper permits only upload-pack
for `/coverage.git` and the configured host (with/without default port). It
refuses receive-pack, alternate paths/hosts, oversized/truncated requests and
extra protocol capabilities. It forwards native Git's response unchanged.
Requests have a five-second handshake deadline, a sixty-second overall bound,
and a maximum of eight concurrent native children. SIGTERM closes the listener
and connections, cancels child commands, and joins them; it never removes the
repository or another container.

Use a separate `git-sync` collector server with ordinary local/no-auth Docker
settings plus these **same server and runner** variables:

```text
CAESIUM_JOBDEF_GIT_ENABLED=true
CAESIUM_JOBDEF_GIT_ONCE=false
CAESIUM_JOBDEF_GIT_INTERVAL=500ms
CAESIUM_JOBDEF_GIT_SOURCES=[{"url":"git://<owned-alias>:9418/coverage.git","ref":"main","path":"jobs","globs":["**/*.job.yaml"],"source_id":"coverage-git-<run-id>","interval":"500ms","once":false}]
```

Runner-only variables:

```text
CAESIUM_JOBDEF_GIT_SYNC_LANE=true
CAESIUM_JOBDEF_GIT_FIXTURE_ROOT=/fixture
CAESIUM_JOBDEF_GIT_RECEIPT=/coverage-evidence/git-sync.json
```

Mount a fresh, collector-owned evidence directory as `/coverage-evidence` on the
runner and require `TestIntegrationTestSuite/TestJobdefGitSyncLocalRepositoryUpdatesAndPrunes`
to PASS without skip. The runner keeps the candidate CLI path/GOCOVERDIR and
Docker socket from the ordinary public-surface lanes. Only the candidate CLI and
main server supply eligible counters. Fixture/helper/test-binary counters cannot
replace either original process contribution.

`state.json` has schema version 1 and fields `alias`, `source_id`, `url`, `ref`,
`path`, `image`, `initial_commit`, and `git_version`. The test requires the exact
single Git source, validates real fixture paths/initial native HEAD, and observes:

1. Actual configured initial import through job provenance and atom HTTP reads.
2. A CLI-applied sentinel independent of the Git source.
3. A real native Git update commit, unchanged durable JobID with new provenance
   and atom command, then a real candidate CLI start and succeeded completed
   task with output marker `updated`.
4. A real native deletion commit, exact imported JobID 404/alias absence, and
   unchanged sentinel ID surviving source-scoped prune.

The fresh receipt records schema version 1, `source` (the complete initial state),
`initial_commit`, `updated_commit`, `deleted_commit`, `job_id`, `sentinel_id`,
`run_id`, `run_status`, `run_completed_at`, `task_completed_at`, `output_marker`,
`imported_job_absent`, and `sentinel_survived`. All three distinct commits must be
full lowercase 40-hex IDs; job/sentinel/run IDs must be distinct canonical nonzero
UUIDs; status is `succeeded`, output is `updated`, timestamps are present/nonzero
and no later than the main server's finished time, and both prune guards are true.
No credential, Git payload, or raw server log is included.

Run the focused fixture tests in the builder:

```sh
go test -tags=integration ./test/fixtures/git-source -count=1
```

Those tests cover refusal guards, native committed content/advertisement, and
native upload-pack over in-memory `net.Pipe` with cancellation/join. They allocate
no TCP listener or external network resource. A passing fixture test is not live
GitSync or coverage proof. The parent must run the actual private TCP readiness
query and complete public journey, retain candidate-image/source identities,
require clean main-server flush/exit and owned-resource cleanup, then run the
unchanged strict cohort and coverage ratchet checks.
