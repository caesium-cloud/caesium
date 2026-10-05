# Go maintainability implementation

Status: **implemented; required hosted qualification failed**. [Aggregate draft PR #620](https://github.com/caesium-cloud/caesium/pull/620) contains all 253 planned source items. The published `296c4e8a` run completed with 38 successful, three failed and three skipped jobs. Both architecture stress checks and the normal test matrices passed; coverage stopped at backend readiness, and Helm initial cluster startup failed before pod replacement. Independent/root review retains both failures with their underlying causes unproven. All 253 items remain implemented; zero are finally accepted.

Base / verified audit target: `0b4cc4c4677f9cb28924366043ccb79cc4543e54` (#618 included). Both Go modules declare Go 1.27.1. The audit raised 142 findings, merged them into 107 independently verified candidates, and produced 253 work items. All confirmed items remain in scope, including low-value cleanup; item-sized PR suggestions are now local commit batches for one final PR.

## Accepted design decisions

- C003: native dqlite v1.18.7 already enables foreign keys per connection. Native orphan rejection and replacement-connection characterization support removing the redundant Go setter. No unsupported-FK fallback. [Native source](https://github.com/canonical/dqlite/blob/v1.18.7/src/vfs.c#L2461-L2588).
- C032: fail closed on predecessor input acquisition errors and reuse one successful snapshot for identity, descriptor and execution across retries. Preserve nil-success, legacy trigger/status policy and deliberate image uncertainty cache bypass.
- C024/W64: reuse the existing canonical `normalizeAllowlist` implementation, with a nil-to-empty adapter for scope decoding. This preserves agent-key nil/empty restrictions and scope behavior, removes 18 production lines against the audit base, and avoids introducing an agent launch path solely for coverage. Auth/middleware native race, tagged vet and lint passed at c93a5daa; public mint wiring remains unqualified.
- No exported identifier/signature changes outside internal packages are authorized by this plan.

## Acceptance

- Every confirmed candidate and every work item below must have an explicit current-code disposition and acceptance evidence. Source edits alone do not complete an item.
- Both modules pass their containerized lint/unit checks; tagged integration packages compile and affected real CLI/HTTP/runtime scenarios execute without hollow skips.
- Ownership/concurrency/retry/parser changes pass focused race and failure tests; changes preserve the verified semantic differences.
- Required CI checks are bound to the final candidate; missing/native/environment proof remains incomplete. The requested endpoint is one PR, not a merge.
- Main checkout/unrelated work and foreign Docker resources remain untouched.

## Progress

Current item states: implemented=253.

| Item | Candidates | Work | State | Evidence |
| --- | --- | --- | --- | --- |
| W01 | C061, C058 | Pin CLI HTTP policies and separate output streams | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W02 | C105 | Characterize local execution state and completion handoff | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W249 | C032 | Pin predecessor projections and execution ownership | implemented | 7255aa8f..5b50c51d; PG snapshot77aaf098; complete-scope acceptance pending |
| W250 | C003 | Prove native foreign keys on both pools and new connections | implemented | bb70101f..63d3dbae; complete-scope acceptance pending |
| W03 | C002 | Pin caller-specific retry policies | implemented | bb70101f..63d3dbae; complete-scope acceptance pending |
| W04 | C002 | Pin database retry policy before extraction | implemented | bb70101f..63d3dbae; complete-scope acceptance pending |
| W05 | C054 | Characterize the read-only SQL guard | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W06 | C097 | Use current standard-library idioms in api/rest/controller/jobdef | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W07 | C096 | Use current standard-library idioms in api/rest/controller/replay | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W08 | C096 | Use current standard-library idioms in api/rest/controller/system | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W09 | C097 | Use current standard-library idioms in api/rest/service/contract | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W10 | C097 | Use current standard-library idioms in api/rest/service/dataset | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W11 | C005 | Use current standard-library idioms in api/rest/service/replay | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W12 | C097 | Use current standard-library idioms in api/rest/service/system | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W13 | C005 | Use current standard-library idioms in cmd/contract | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W14 | C096 | Use current standard-library idioms in cmd/dataset | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W15 | C005, C097 | Use current standard-library idioms in cmd/job | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W16 | C005, C096 | Use current standard-library idioms in cmd/reproduce | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W17 | C005 | Use current standard-library idioms in cmd/why | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W18 | C096 | Use current standard-library idioms in internal/atom/kubernetes | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W19 | C005 | Use current standard-library idioms in internal/cache | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W20 | C005 | Use current standard-library idioms in internal/connector | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W21 | C005 | Use current standard-library idioms in internal/contract | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W22 | C005 | Use current standard-library idioms in internal/freshness | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W23 | C005 | Use current standard-library idioms in internal/harness | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W24 | C005 | Use current standard-library idioms in internal/incident | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W25 | C005, C096 | Use current standard-library idioms in internal/jobdef | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W26 | C005 | Use current standard-library idioms in internal/jobdef/runtime | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W27 | C005 | Use current standard-library idioms in internal/lineage | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W28 | C005 | Use current standard-library idioms in internal/reproduce | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W29 | C005, C096 | Use current standard-library idioms in internal/run | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W30 | C005 | Use current standard-library idioms in internal/testfault | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W31 | C005 | Use current standard-library idioms in internal/worker | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W32 | C096 | Use current standard-library idioms in pkg/dqlite | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W33 | C096 | Use current standard-library idioms in pkg/env | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W34 | C005 | Use current standard-library idioms in pkg/jobdef/schemacompat | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W35 | C005 | Use current standard-library idioms in pkg/task | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W36 | C097 | Use current standard-library idioms in reagents/cmd/tf-discover | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W37 | C005 | Use current standard-library idioms in reagents/cmd/tf-runner | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W38 | C005 | Use current standard-library idioms in reagents/cmd/tf-warm | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W39 | C097 | Use current standard-library idioms in reagents/internal/fingerprint | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W40 | C005, C097 | Use current standard-library idioms in reagents/internal/protocol | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W41 | C005, C097 | Use current standard-library idioms in reagents/internal/tf | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W42 | C005, C096, C097 | Use current standard-library idioms in test | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W43 | C005, C096, C097 | Use current standard-library idioms in test/lifecycle | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W44 | C005, C097 | Use current standard-library idioms in test/model | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W45 | C096 | Use current standard-library idioms in test/performance | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W46 | C097 | Use current standard-library idioms in test/robustness | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W47 | C005 | Use current standard-library idioms in test/robustness/faults | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W48 | C005, C097 | Use current standard-library idioms in test/robustness/history | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W251 | C003 | Remove redundant FK setup and clarify native PRAGMA policy | implemented | bb70101f..63d3dbae; complete-scope acceptance pending |
| W49 | C001 | Reject byte-size suffix multiplication overflow | implemented | f6415b2a/f73a5901; complete-scope acceptance pending |
| W50 | C044 | Reject malformed partition edges during recovery | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W51 | C077 | Reject incomplete SSE event backlogs | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W52 | C079 | Reject malformed boolean environment values before load qualification | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W53 | C083 | Require durable snapshots before claiming rejected requests are state-inert | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W54 | C086 | Treat malformed accepted start outcomes as uncertain | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W55 | C098 | Stop the distributed atom before returning partition marker errors | implemented | 7255aa8f..5b50c51d; PG snapshot77aaf098; complete-scope acceptance pending |
| W56 | C099 | Register local whole-run retries before starting execution | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending |
| W57 | C014, C015, C016 | Keep cron recurrence alive and errors local | implemented | bb70101f..63d3dbae; complete-scope acceptance pending |
| W252 | C032 | Read and project predecessor inputs from one strict snapshot | implemented | 7255aa8f..5b50c51d; PG snapshot77aaf098; complete-scope acceptance pending |
| W253 | C032 | Fail before launch and reuse predecessor inputs across retries | implemented | 7255aa8f..5b50c51d; PG snapshot77aaf098; complete-scope acceptance pending |
| W58 | C008 | Chunk wide lineage frontiers before building SQL predicates | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W59 | C009 | Share the repeated task lifecycle event mapping | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W60 | C010 | Reclaim expired job-cache entries | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W61 | C012 | Batch consumed-dataset state reads | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W62 | C018 | Classify Docker image absence with the typed error | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W63 | C019 | Share predecessor-output name reconstruction | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W64 | C024 | Normalize job aliases in one auth helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending; C024 accepted direction amendment852376→c93: reuse exact existing normalizeAllowlist, nil-to-empty scope adapter; no new agent launch behavior; independent ACCEPT + native auth/middleware race/taggedvet/lint0 PASS. |
| W65 | C026 | Share the SQL and in-memory dispatch batching loop | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W66 | C028 | Keep deterministic policy rules from dispatching approval-tier actions | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W67 | C033 | Use cache.HashInput directly for local hash inputs | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W68 | C040 | Share fan-out TaskRun materialization between SQL and owner paths | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W69 | C043 | Share schema-violation policy across catalog and instance validation | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W70 | C045 | Use one SQL set for terminal task statuses | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W71 | C046 | Propagate execution context through data-assertion evaluation | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W72 | C049 | Share run and partition page-bound parsing | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W73 | C053 | Propagate statistics query failures | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending; 2783514e nativecontroller: threefreshchild racePASS, realGet/Summary querycancel=>wrapped500/exactredactedJSON/positivebeforeafter; package lint0; rootreceipt run/stats-controller-root-adjudication.json |
| W74 | C054 | Share the SQL quote and comment scanner | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W75 | C063 | Centralize tf-runner's plan-to-apply test wiring | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W76 | C073 | Reap developer-journey CLI children on early failures | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W77 | C080 | Reject unknown names in mixed performance workload selections | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W78 | C082 | Use one percentile convention for end-to-end latency fields | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W79 | C084 | Fail soak drain when owned task resources remain after grace | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W80 | C085 | Join the retention checkpoint poller on every exit | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W81 | C089 | Restrict the reserved actor check to the root action schema | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W82 | C095 | Scan worker aggregates into a typed result | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W83 | C103 | Correlate SSE expectations with the triggered run | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W84 | C104 | Replace the fixed stagger in the concurrent warm test | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W85 | C038, C039 | Share completion field encoding and failure messages | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W86 | C068, C075 | Wait on retry state instead of sleeps | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W87 | C048 | Reject malformed atom UUIDs before service calls | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W88 | C048 | Reject malformed trigger UUIDs before service calls | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W89 | C006 | Share the duplicated cache-hash test setup | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W90 | C013 | Share dataset_advanced event construction | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W91 | C020 | Centralize repeated reproduction descriptor test defaults | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W92 | C022 | Remove the no-op Kubernetes Atom constructor | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W93 | C030 | Share the failed-first task-run attribution selector | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W94 | C037 | Trigger-rule success and failure scenarios duplicate their DAG setup | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W95 | C047 | Table-drive OIDC and SAML callback persistence coverage | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W96 | C067 | Use the shared raw CLI runner for expected errors | implemented | Luna test hygiene integrated through 1dc686e6; host subset passed; native/integration acceptance pending |
| W97 | C071 | Share the task-ID-to-name index across integration helpers | implemented | a63fc5fe task name fixtures; real-surface acceptance pending |
| W98 | C088 | Give the host-request encoder a concrete request type | implemented | 610b7cea typed host request encoder; real-surface acceptance pending |
| W99 | C106 | Remove the unused bus dispatcher tuning options | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending |
| W100 | C069, C070, C074 | Consolidate typed integration manifest fixtures | implemented | 97506bfe integration manifest fixtures; real-surface acceptance pending |
| W101 | C076, C078 | Share lifecycle matrix and membership reads | implemented | 36447dc5 matrix/directViews; final acceptance pending |
| W102 | C057, C058, C060 | Consolidate helpers and error handling in cmd/auth | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W103 | C057, C058 | Consolidate helpers and error handling in cmd/backfill | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W104 | C057, C058 | Consolidate helpers and error handling in cmd/cache | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W105 | C057, C058 | Consolidate helpers and error handling in cmd/receipt | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W106 | C058 | Consolidate helpers and error handling in cmd/reproduce | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W107 | C058 | Consolidate helpers and error handling in cmd/verify | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W108 | C058, C060 | Consolidate helpers and error handling in cmd/why | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W109 | C087 | Introduce strict internal SQL evidence decoding | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending |
| W110 | C087 | Migrate cluster strict query to the C087 helper | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending |
| W111 | C087 | Migrate robustness persisted evidence to the C087 helper | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending |
| W112 | C087 | Migrate robustness scalar and checkpoint evidence to the C087 helper | implemented | 824c82b8 exact same-request scalar/checkpoint cells; final acceptance pending |
| W113 | C057 | Consolidate helpers and error handling in cmd/agentprofile | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W114 | C057 | Consolidate helpers and error handling in cmd/contract | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W115 | C102 | Consolidate helpers and error handling in cmd/dataset | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W116 | C057 | Consolidate helpers and error handling in cmd/dev | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W117 | C057 | Consolidate helpers and error handling in cmd/event | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W118 | C057 | Consolidate helpers and error handling in cmd/job | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W119 | C057, C060, C062 | Consolidate helpers and error handling in cmd/run | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W120 | C057, C102 | Consolidate helpers and error handling in cmd/start | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending |
| W121 | C057 | Consolidate helpers and error handling in cmd/test | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W122 | C102 | Consolidate helpers and error handling in reagents/cmd/tf-warm | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W123 | C102 | Consolidate helpers and error handling in test/lifecycle | implemented | e944b197 dual-cause lifecycle error wrapping; final acceptance pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W124 | C002 | Introduce caller-configured database retry mechanics | implemented | bb70101f..63d3dbae; complete-scope acceptance pending |
| W125 | C002 | Migrate auth to the C002 helper | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W126 | C002 | Migrate backfill to the C002 helper | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W127 | C002 | Migrate callback to the C002 helper | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W128 | C002 | Migrate freshness to the C002 helper | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W129 | C002 | Migrate jobdef importer to the C002 helper | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W130 | C002 | Migrate run store to the C002 helper | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W131 | C002 | Migrate worker to the C002 helper | implemented | 97751615..b66a4c66; complete-scope acceptance pending |
| W132 | C017 | Consolidate event matching and scalar formatting | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W133 | C017 | Migrate HTTP trigger to the C017 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W134 | C017 | Migrate contract matcher to the C017 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W135 | C017 | Migrate jobdef matcher to the C017 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W136 | C021 | Share container state and exit-code mappings | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W137 | C021 | Migrate Kubernetes to the C021 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W138 | C021 | Migrate Podman to the C021 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W139 | C023 | Use the SQL uniqueness classifier in user insertion | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W140 | C023 | Migrate SAML assertions to the C023 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W141 | C025 | Share SSO return-target and random-state primitives | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W142 | C025 | Migrate SAML state to the C025 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W143 | C029 | Share the active-authentication predicate | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W144 | C029 | Migrate incident auth gate to the C029 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W145 | C031 | Share event subscription lifecycle | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W146 | C031 | Migrate incident subscriber to the C031 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W147 | C031 | Migrate lineage subscriber to the C031 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W148 | C031 | Migrate notification subscriber to the C031 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W149 | C035 | Share execution retry-delay calculation | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W150 | C035 | Migrate worker retry delay to the C035 helper | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W151 | C042 | Share fan-out group detection | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W152 | C042 | Migrate run fan-out groups to the C042 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W153 | C050 | Introduce the bounded body reader | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending |
| W154 | C081 | Require complete HTTP evidence in test/lifecycle | implemented | 09614dcc complete mixed-protocol probe; final-qualification-receipt.md; live gates pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W155 | C081 | Require complete HTTP evidence in test/load | implemented | d8f47a02 complete load HTTP evidence; real-surface acceptance pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W156 | C081 | Require complete HTTP evidence in test/performance | implemented | 18828553 complete env probe; final acceptance pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W157 | C081 | Require complete HTTP evidence in test/robustness/cluster | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W158 | C081 | Require complete HTTP evidence in test/robustness/faults | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W159 | C081 | Require complete HTTP evidence in test/robustness | implemented | 5dec0494 incomplete mutation proof; ledger follow-up in progress; final acceptance pending; C081 exactb42 selected25tests/race3/tagvet/lint PASS; original6612 read-seam controls attributed; new consumer outcomes separately proved; final hosted acceptance pending. |
| W160 | C050 | Migrate webhook body limit to the C050 helper | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending |
| W161 | C051 | Share allowlisted order parsing | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W162 | C051 | Migrate notification ordering to the C051 helper | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W163 | C055 | Share exact engine membership | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W164 | C055 | Migrate agentprofile engines to the C055 helper | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W165 | C061 | Share CLI HTTP transport | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W166 | C061 | Migrate auth HTTP to the C061 helper | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W167 | C061 | Migrate contract HTTP to the C061 helper | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W168 | C061 | Migrate dataset HTTP to the C061 helper | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W169 | C061 | Migrate incident HTTP to the C061 helper | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W170 | C065 | Share Terraform child-output routing | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W171 | C065 | Migrate tf-discover output to the C065 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W172 | C065 | Migrate tf-warm output to the C065 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W173 | C090 | Share trigger-pattern configuration parsing | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W174 | C090 | Migrate contract pattern parser to the C090 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W175 | C091 | Expose the existing internal contract alias query | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W176 | C091 | Migrate jobdef alias reads to the C091 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W177 | C092 | Share Reagents environment-name normalization | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W178 | C092 | Migrate tf-runner names to the C092 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W179 | C094 | Share Prometheus sample selection | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W180 | C094 | Migrate robustness metrics to the C094 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W181 | C060, C062 | Consolidate helpers and error handling in cmd/blame | implemented | f9396f3c..4169b1ff; complete-scope acceptance pending |
| W182 | C007 | Introduce a dependency-free YAML path leaf | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W183 | C007 | Migrate jobdef Git to the C007 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W184 | C007 | Migrate jobdef diff to the C007 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W185 | C011 | Reuse OpenTestDB in the first lineage suite | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W186 | C011 | Migrate agentprofile tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W187 | C011 | Migrate atom tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W188 | C011 | Migrate job tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W189 | C011 | Migrate notification tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W190 | C011 | Migrate receipt tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W191 | C011 | Migrate stats tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W192 | C011 | Migrate task tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W193 | C011 | Migrate taskedge tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W194 | C011 | Migrate trigger tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W195 | C011 | Migrate worker tests to the C011 helper | implemented | 9f4ceb51/59b6a1e8, db-fixture-receipt.md; hostlineage/receipt passed, native service gates pending |
| W196 | C027 | Reuse metric value helpers in dispatch tests | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W197 | C027 | Migrate metrics tests to the C027 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W198 | C027 | Migrate run metrics tests to the C027 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W199 | C034 | Share run-parameter environment construction | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W200 | C034 | Migrate worker params to the C034 helper | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W201 | C036 | Share task-failure policy normalization | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W202 | C036 | Migrate worker failure policy to the C036 helper | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W203 | C052 | Share API audit-failure logging | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W204 | C052 | Migrate auth audit warnings to the C052 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W205 | C052 | Migrate notification audit warnings to the C052 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W206 | C056 | Share test JSON fixture encoding | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending |
| W207 | C056 | Migrate API replay JSON fixtures to the C056 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending |
| W208 | C056 | Migrate incident JSON fixtures to the C056 helper | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W209 | C056 | Migrate jobdef diff JSON fixtures to the C056 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W210 | C056 | Migrate notification JSON fixtures to the C056 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W211 | C056 | Migrate replay JSON fixtures to the C056 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W212 | C059 | Share first-nonblank string selection | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W213 | C059 | Migrate job lint to the C059 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W214 | C059 | Migrate reproduce to the C059 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W215 | C059 | Migrate reproduce CLI to the C059 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W216 | C059 | Migrate run strings to the C059 helper | implemented | 475dbf1a integrated; native regression pending |
| W217 | C064 | Share Terraform environment copying | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W218 | C064 | Migrate tf-warm environment to the C064 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W219 | C066 | Share deterministic Git fixture commands | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W220 | C066 | Migrate tf Git fixtures to the C066 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W221 | C066 | Migrate tf-discover Git fixtures to the C066 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W222 | C072 | Share the loopback-address fixture | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W223 | C072 | Migrate Linux nodelay fixture to the C072 helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W224 | C107 | Share the scoped GORM fault fixture | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W225 | C107 | Migrate freshness failure fixtures to the C107 helper | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W226 | C041 | Replace typed union-key copies with one generic helper | implemented | 5b2b7db6..58883905; complete-scope acceptance pending |
| W227 | C105 | Extract local queue and trigger bookkeeping | implemented | 7029e41a..93ba93ce; compile/race underway; complete-scope acceptance pending |
| W228 | C105 | Move local cache identity and rate-limit methods | implemented | 7029e41a..93ba93ce; compile/race underway; complete-scope acceptance pending |
| W229 | C105 | Move the atom execution closure | implemented | 7029e41a..93ba93ce; compile/race underway; complete-scope acceptance pending |
| W230 | C105 | Move fan-out execution and its shared state | implemented | 7029e41a..93ba93ce; compile/race underway; complete-scope acceptance pending |
| W231 | C105 | Move task dispatch onto localRun | implemented | 7029e41a..93ba93ce; compile/race underway; complete-scope acceptance pending |
| W232 | C105 | Finish local scheduler execution handoff | implemented | 7029e41a..93ba93ce; compile/race underway; complete-scope acceptance pending |
| W233 | C100 | Introduce the server work supervisor | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W234 | C100 | Wire the supervisor into API and startup shutdown | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W235 | C100 | Migrate backfill launches to the C100 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W236 | C100 | Migrate manual launches to the C100 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W237 | C100 | Migrate partition retry launches to the C100 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W238 | C100 | Migrate replay launches to the C100 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W239 | C100 | Migrate webhook launches and receipts to the C100 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W240 | C100 | Migrate whole-run retry launches to the C100 helper | implemented | 219bead1..12fc7e0e; metrics ec3a40c8; fixture cca78cea; aggregate/real-surface acceptance pending; native df7c69c1 + fresh all8 C100realshutdown PASS (lanes-df7c69c1-shutdown/summary.json); finalhostedCIpending |
| W241 | C101 | Add the signal-context protocol entry point | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W242 | C101 | Migrate tf-discover signals to the C101 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W243 | C101 | Migrate tf-runner signals to the C101 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W244 | C101 | Migrate tf-warm signals to the C101 helper | implemented | 592c0a14..9eb5734c; host focused race; real Terraform check underway; aggregate/real-surface acceptance pending |
| W245 | C004 | Use the declared backfill enum types in the model | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W246 | C093 | Introduce the shared workload catalogue schema | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W247 | C093 | Migrate load catalogue to the C093 helper | implemented | f84bceff shared typed catalog, driver override tests; real-surface acceptance pending |
| W248 | C093 | Migrate performance catalogue to the C093 helper | implemented | 2fcdbacd typed20 expectation presence; final acceptance pending |

## Verification status

Verification evidence (each result keeps its recorded source identity):

- Whole local coverage at `6612e775` passed independent/root review: collector and all three evidence gates exited zero; all 77 unchanged package floors passed; 482 changed Go paths, 177 eligible production statement files, zero uncovered. Integration coverage was 52.2%; integration plus browser coverage was 52.8%. Profiles retain their original source identity.
- Actual `6612e775` journeys passed: 69 local, 38 auth, 54 distributed and 37 owner-memory scenarios; continuous GitSync update/output/prune; 32 SSO checks with persistent replay and restart; 14 Kubernetes/Podman processes; five original no-server retry processes; three browser cases. Independent evidence and exact owned-resource/process cleanup checks passed.
- Whole performance at `6612e775` passed independent/root review on attempt 1: four families, 42 series with ten samples per side, 84 unchanged target/base and fixed-baseline comparisons, and 15 applicable closed-workload SLOs. Source images, builders, harness, calibration and original samples are bound; no pooling. Both servers, network and launcher groups were absent; port/lock were free; 12 preflight foreign containers were preserved.
- C003 native FK characterization covers all three dqlite databases, both pools and replacement connections. C032 PostgreSQL repeatable-read snapshot proof includes a failing READ COMMITTED negative control. Input acquisition failure, snapshot reuse and caller retry policies have focused native race evidence.
- SQL scanning has 50,000-input differential race evidence. API Start/Shutdown owner barriers, private CLI natural drain/replacement handoff, freshness compensation and matching committed REST retry cases passed focused races, tagged vet and lint at recorded source commits. Current production blobs preserve those identities; no atomic context-versus-commit guarantee is claimed.
- Public API declaration screen passed: 234 Git-inventoried files, 107 packages, 866 declarations per side and 11 controls. Only planned Cobra NoArgs initializer differences remain; no exported identifier/signature changes. This syntax screen is not an ABI/runtime compatibility proof.
- Reagents is checked separately: native races and lint/vet, both architecture unit/race jobs, and actual Terraform 1.15.9 infrastructure scenarios have retained source-equivalent evidence. The 14 supporting Reagents blobs and module/infra inputs match their qualified source. Unit/Reagents coverage profiles were not collected.
- All 724 CI-tooling controls passed locally at `6612e775`. The local-retry correction has independent/root native five-process proof; the SSO permission correction has independent/root Linux UID refusal and positive execution proof. Fresh `6612e775` whole coverage subsequently passed both corrected helpers.
- Hosted `6612e775` run 37366120047 failed twice. Attempt 1: two successful, two failed, two canceled and 26 skipped jobs. Attempt 2: zero successful, two failed, four canceled and 26 skipped jobs. Authenticated annotations show hosted runner acquisition refusal with runner ID zero; merge guards reject the canceled changes job. Required product/runtime/coverage jobs did not execute. No third manual retry was performed.
- Final C081 test-only follow-up at `b42e1c6b` passed independent Luna/root contract review and native `-tags=integration -race -count=3`: 25 selected tests, 75 top-level and 96 subtest passes, no skips; six package commands, tagged vet and lint exited zero. The owned builder exited zero without OOM/restarts, then was removed by exact ID with literal absence checks. Existing seam/overflow/offer/performance controls were already present; added tests exercise consumer recovery, unavailable observations and complete-page retention. Four test files changed (+310/-18); production, measurement, build and workflow inputs match `6612e775`.
- Published `348338f0` hosted run 37375672260 completed with 18 successful, five failed and 12 skipped jobs. Both architectures passed unit and lint; ARM64 integrations passed. The AMD64 image stress smoke failed after its healthy control with no retained failed assertion/native state. Coverage later failed before its first Kubernetes case after provisioning/import and main startup; the precise cause is unrecorded. Separate host Git fixture cleanup failed with permission errors. No strict coverage/floor pass or full required matrix is claimed.
- Stress helper correction at `bfacd92e`, integrated as `2885a423`, passed independent Luna/root review, all 739 tooling controls, Bash syntax and ShellCheck. Real ARM64 kernel OOM at exact 64 MiB caps passed; deliberate SIGKILL exit 137 with OOMKilled=false was refused. Six owned container IDs were absent and preexisting containers preserved; the owned lane lock was released. The healthy fixture now requires actual limit/allocation/completion markers; eight original false-green controls are rejected. The original AMD64 failed assertion/cause remains unknown; local proof does not establish hosted qualification.
- Final bounded backend/Git helper sources `36f54c7c` and `3d59a952`, integrated at `64dd45d4`, passed independent Luna/root review. Backend diagnostics captured an actual exited owned process and refused wrong ownership/missing objects, with three exact IDs removed. Real Linux UID1001 cleanup reproduced EACCES on root-written Git directories, then deleted the restored tree; symlink/hardlink/substituted-marker controls refused and preserved sentinels. Thirteen pinned-builder process IDs and four exact owned volumes were removed; the owned lane PID/lock are absent. Readiness/scenario/coverage/performance budgets are unchanged. These are bounded helper controls; the original Kubernetes failure cause remains unknown and no whole new coverage cohort is claimed.
- The final combined qualification-helper tree at `64dd45d4` passed all 752 CI-tooling tests in 230.214 seconds. All 515 existing work-item file bindings and eight original alternate-path null bindings match the previous attributed source. The delta from published `348338f0` is seven helper/test files plus this dashboard; Go sources, module files, runtime producer Dockerfiles and fixed performance harness/budget inputs are unchanged; coverage qualification collectors changed as described above. Original runtime profiles/performance samples retain their actual source; no failed cohort is promoted.
- Published `296c4e8a` hosted run [37385246512](https://github.com/caesium-cloud/caesium/actions/runs/37385246512), attempt 1, completed with 38 successful, three failed and three skipped jobs. Both architecture stress smokes, unit/race/lint/Reagents checks, all Docker/Podman/ARM64/Kubernetes integration shards, Terraform infrastructure, auth/SSE, UI E2E, standalone lifecycle and the separate owner-crash/rejoin lane passed. The two primary failures are coverage-ratchets and helm-pod-replacement-test; ci-ok reflects those failures. No manual rerun was performed.
- Original `296c4e8a` coverage was independently/root reviewed against its retained original log and 417-member artifact ZIP. Local/auth/distributed/owner-memory/Git required scenarios passed 69/38/54/37/1; all 32 SSO checks passed. Both Git fixture directories emitted successful ownership-restoration records (64 and three entries), with source-checked host deletion and no prior permission errors. No independent remote-daemon absence check is claimed. Coverage then stopped before its first Kubernetes case: 396 transport errors, no HTTP status, exact owned server Running=true/OOMKilled=false/restarts=0 and verified loopback port. The original report remains incomplete; no final backend/browser/local-retry/floor gate or accepted profile exists. Specific transport/startup cause is unknown.
- Original `296c4e8a` Helm replacement qualification failed during the initial 480-second Helm install, before any replacement action. Pods zero and one were Ready; pod two repeatedly received native join refusal "server ID already in use (1)". The suffix is a protocol error code, not the attempted node ID. Attempted identity and committed membership were not retained. Bootstrap/Helm setup matches base, but no base reproduction, flake cause or maintenance regression is established. The separate successful owner-crash/rejoin scenarios do not qualify this failed replacement lane.
- A bounded read-only startup trace found that the backend collector attaches the kind network before its first health poll; neither startup nor health constructs the Kubernetes engine or contacts its API. Engine creation is deferred to a task attempt. This rules out the proposed pre-health Kubernetes API dependency; saved inputs do not establish the cause of the transport deadline.

Current qualification limits:

- Published `296c4e8a` hosted run 37385246512 failed (38 successful / three failed / three skipped). All 253 items remain implemented with zero final acceptances. This documentation-only status update preserves the unchanged qualified helper/source bindings; it does not fix either startup failure. Original `6612e775` local runtime/performance evidence and C081 error-path evidence retain their identities; failed/partial hosted profiles are not promoted.
- Historical failed cohorts remain retained at their actual sources. The `348338f0` AMD64 stress assertion and backend startup cause were unlocalized; its Git cleanup permission failure was definite. Stress and Git corrections have independent native proof and now passed their corresponding `296c4e8a` hosted execution. The new Kubernetes health failure has safe native-state evidence but no application startup log or specific transport cause. Both old and new coverage cohorts remain incomplete.
- Issue #598 orphan reaping remains separate by user decision: enforcing soak passed five fault families but failed final drain after a killed worker retained a completed task pod. No fresh base reproduction or passing full enforcing soak is claimed.
- Nightly-performance registration remains separate by user decision: measurements passed, but the original wrapper failed with no registered scenarios. Fifteen open-workload SLOs were not evaluated. Results show bounded noninferiority, not equivalence or a speedup; sample SIGKILL teardown does not prove graceful coverage flushing.
- DT-CANCEL-01/DT-EVENT-01 labels, the excluded nonmandatory saturated-pool skip and public agent-mint wiring remain unqualified. Skips do not count as named passes. Selected coverage demonstrates at least one statement in each eligible changed production file, not every changed branch.
- Decision pending: keep initial cluster startup/recovery work separate and document the failed qualification (recommended), or expand PR #620 to diagnose and fix it. No recovery implementation, retry-budget increase or coverage-policy relaxation has been made. The failed backend readiness and pod-replacement gates remain acceptance blockers until resolved or explicitly scoped by the user.

All 253 source items have explicit attribution and remain implemented, with zero final acceptances while required hosted qualification is failed. Source `296c4e8a` has terminal results; this dashboard update changes documentation only and does not remedy either startup failure. Prior failed qualifications and partial profiles remain bound to their original sources. Startup-recovery scope is awaiting the user decision described above. The requested endpoint is this single aggregate draft PR; no merge has been performed.

Required baseline commands: `just lint`, `just unit-test`, `just reagents-lint`, `just reagents-test`, `just integration-test`, plus affected auth, distributed/owner-memory, Podman, lifecycle and infrastructure lanes named by the work items and current CI workflow. Commands run from the aggregate worktree and use repository containers.

The original audit ledger remains local under `.audit/`; its immutable target is evidence, not the implementation checkout. Worker receipts and command logs are recovery evidence under `.codex/runs/go-maintenance/all/`. This tracked dashboard is the durable implementation status.
