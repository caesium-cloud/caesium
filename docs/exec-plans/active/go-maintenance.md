# Go maintainability implementation

Status: **in progress**. [Aggregate draft PR #620](https://github.com/caesium-cloud/caesium/pull/620) contains all planned source edits. Acceptance remains pending while final runtime and hosted checks run; worker branches are local integration units.

Base / verified audit target: `0b4cc4c4677f9cb28924366043ccb79cc4543e54` (#618 included). Both Go modules declare Go 1.27.1. The audit raised 142 findings, merged them into 107 independently verified candidates, and produced 253 work items. All confirmed items remain in scope, including low-value cleanup; item-sized PR suggestions are now local commit batches for one final PR.

## Accepted design decisions

- C003: native dqlite v1.18.7 already enables foreign keys per connection. Characterize native orphan rejection/replacement before removing the redundant Go setter. No unsupported-FK fallback. [Native source](https://github.com/canonical/dqlite/blob/v1.18.7/src/vfs.c#L2461-L2588).
- C032: fail closed on predecessor input acquisition errors and reuse one successful snapshot for identity, descriptor and execution across retries. Preserve nil-success, legacy trigger/status policy and deliberate image uncertainty cache bypass.
- No exported identifier/signature changes outside internal packages are authorized by this plan.

## Acceptance

- Every confirmed candidate and every work item below has an explicit current-code disposition and acceptance evidence. Source edits alone do not complete an item.
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
| W64 | C024 | Normalize job aliases in one auth helper | implemented | 29 items integrated through 25bafffe/6c254bac/b32aed16; auth-runtime-helpers-receipt.md, native/real surface acceptance pending |
| W65 | C026 | Share the SQL and in-memory dispatch batching loop | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W66 | C028 | Keep deterministic policy rules from dispatching approval-tier actions | implemented | 26 items integrated through d7f1426c; domain-events-receipt.md, native/real surface acceptance pending |
| W67 | C033 | Use cache.HashInput directly for local hash inputs | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W68 | C040 | Share fan-out TaskRun materialization between SQL and owner paths | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W69 | C043 | Share schema-violation policy across catalog and instance validation | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W70 | C045 | Use one SQL set for terminal task statuses | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W71 | C046 | Propagate execution context through data-assertion evaluation | implemented | b4518a47..8ce50178; fixturef8cf3ac0; complete-scope acceptance pending |
| W72 | C049 | Share run and partition page-bound parsing | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
| W73 | C053 | Propagate statistics query failures | implemented | 2ab9b7cd/b6e456f9; stats correctiona52a7be0; complete-scope acceptance pending |
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
| W123 | C102 | Consolidate helpers and error handling in test/lifecycle | implemented | e944b197 dual-cause lifecycle error wrapping; final acceptance pending |
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
| W154 | C081 | Require complete HTTP evidence in test/lifecycle | implemented | 09614dcc complete mixed-protocol probe; final-qualification-receipt.md; live gates pending |
| W155 | C081 | Require complete HTTP evidence in test/load | implemented | d8f47a02 complete load HTTP evidence; real-surface acceptance pending |
| W156 | C081 | Require complete HTTP evidence in test/performance | implemented | 18828553 complete env probe; final acceptance pending |
| W157 | C081 | Require complete HTTP evidence in test/robustness/cluster | implemented | 8948da9a/16586b50/1cb2cee7/7283391e/f7215821/008feae2; complete-scope acceptance pending |
| W158 | C081 | Require complete HTTP evidence in test/robustness/faults | implemented | c04b6577/298dd8ee/3a14063e/bf18324a/3a4070dd/f5423c3f/03530cc2/a542984e; aggregate/real-surface acceptance pending |
| W159 | C081 | Require complete HTTP evidence in test/robustness | implemented | 5dec0494 incomplete mutation proof; ledger follow-up in progress; final acceptance pending |
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

Completed local gates (each applies only to the recorded candidate):

- builder image built: pinned Go/native headers/linter
- native FK regression at acdbb43b: all3DBs/bothpools/replacementconnections
- foundation dbretry/db/auth/freshness/cron/sqlcell race pass at e4addaa6
- run/job/backfill race pass at0031eb3e
- worker race pass at5b50c51d
- PG repeatable-read predecessor snapshot at77aaf098 passed; READ COMMITTED negative control fails as expected (postgres-inputs-negative.log)
- root full race suites at8ce50178 passed except unique-index fixture subsequently fixedf8cf3ac0; focused third native/run/cluster checks atf8cf3ac0 passed
- all cmd packages +clihttp +pre-extraction internal/job race at4169b1ff passed (cli-baseline.log)
- private transport ambiguity tagged cluster host race passed (cluster-transport-host.log)
- Reagents toolchain built with Go1.27.1/Terraform1.15.9 (reagents-toolchain.log)
- Full native local executor/stats/event/worker race atf314d320 passed (local-extract-rerun.log)
- SQL 50000-input differential old/new scanner+guard comparison race passed at3a14063e (sql-differential.log)
- Standalone/shared-owner/unexpected-exit API +start/runlife native race at e430e416 passed; launcher gate pending finalfixture (differential-lifetime.log)
- Interposer valid-JSON+error/cap+1/exactcap host race passed (interposer-evidence-host.log)
- Strict workload schema20keys actualcatalog/false/zero/null/nested/unselected tests race passed atf5423c3f (catalog-foundation-host.log)
- Missing lease/recipe SQL durable snapshot subprocess characterization host race passed (durable-snapshot-host.log)
- Full Reagents race with actual Terraform at167743bb passed; two redundant conversions fixed d15c97a4; final lint pending
- Root module full native race/coverage + tagged vet + lint and TestJoinedCommand hermetic regression at7b2a8f0f passed (root-unit-fourth.log); later source needs refreshed proof
- Reagents native-container lint/vet at7b2a8f0f passed0issues (reagents-lint-second.log)
- Actual real CLI/REST/local-runtime integration at36447dc5 passed305 scenario PASS lines, 703s; auth/distributed/infra guards expected and not counted as proof (local-integration-go-maintenance-36447dc5.log)
- Root full native module race/coverage, vet -tags=integration, lint and tagged load/cluster/faults/sqlcell/catalog races at7d0ecc69 PASS; excluded live performance failure caused by missing server is not acceptance.
- Final source09b7f6c9 native tagged vet, lint0issues, fullroot race/coverage PASS (final-native-fourth.log), changedGo gofmt-l empty. Reagents source identical to its previously qualified7b2a8f0f tree.
- Focused source d7822d66 job/API/CLI + selected hermetic tagged lifecycle/robustness/performance/root fixtures race PASS (focused-final-followup.log); finalCLIcompensation subsequently covered fullroot09b7f6c9.
- F4 standalone previous-release upgrade/readdress/rollback/shards/isolation at09b7f6c9 PASS; qualification.json resultpass, all phase exits0 and strict test-evidence passed (lifecycle-standalone-09b7f6c9.log). Image built by that clean invocation.
- Final source09b7f6c9 fresh actual CLI/REST/local-runtime integration PASS305 scenarios/632.780s (local-integration-go-maintenance-09b7f6c9.log); runtime guards excluded auth/distributed/infra are separately hosted.
- Pinned performance fixture test relocation at1c448427: original owner_state_test.go equalsbasebyte-for-byte; baselineharness4005c04frestored; internal/run race PASS. Production source identical09b7; live evidence unaffected by test relocation.
- Run-service authoritative-store reuse atf3edd43c native race + tagged vet PASS; lint flagged seven nil-context test calls, narrow test correction pending (run-service-native-f3edd43c.log).
- Authoritative run-service lazy binding/manual reuse ata66eefed: native race bothpackages, taggedvet andlint0issues PASS; independent Sol review no blocker. Real coverage/join gate pending.
- Hosted lifecycle-cluster and core-robustness run37173776053 at0d54be67 PASS. Later lazy service/manual adapter change has focused native proof; final affected runtime evidence pending.
- C100 explicit whole-owner cancellation Store/worker native races and tagged vet atb6afaf8f PASS; early canceled-resume job race/SSO fixture/tagged vet/lint0issues atf97b784a PASS. Latest write-boundary follow-up independently accepted; refreshed fulljobrace found one prior admission diagnostic expectation, under assessment.
- Final cancellation write-boundary owner sampling atdf7c69c1: fulljobrace, integration-taggedvet andlint0issues PASS; stale closed-owner admission diagnostic regression corrected to assert exact canceledcause/terminalrows/no-extraCreate/join. No atomic claim for context sample-to-DBcommit interval.
- Fresh df7c69c1 image all-eight real C100shutdown cases PASS: admission/start/retry/partition/backfill/webhook/freshness plus distributedreplay durabletasksettlement/restart and timeoutnegative(exit1); cleanownedcleanup. HeldDB/receipt persistence remains separate nativeproof.
- Fresh original-base performance atff0fbd9a: all20bench samples0, bothUIbundlesPASS, cold/warm workload and livebrowser phasesPASS; attempt1 comparison targetbase/SLOno_significant_difference, fixedbaselinewithin_budget; boundednoninferiority, not equivalence. OldfailedUIinvocation retained. WrapperFAILdisablednightly-performance registry is separatequalificationlimit.

Current qualification limits:

- Standard hosted CI run37172950172 failed only coverage-ratchets and dependent ci-ok after successful one-time ARM image rerun; other standard jobs passed.
- Enforcing short soak at1c448427 FAILED final drain: all five fault families passed, one completed task pod/exited container retained after worker SIGKILL, matches existing issue598; fresh base comparison not run. User confirmed #598 reaper remains separate; retain failed soak evidence.
- Original-base performance ata66eefed FAILED correctness before speed: base UI npm ci ENOENT; samples completed but no comparison/SLO/fixed-baseline verdict. Strict nightly-performance manifest gate disabled. After host reboot and Docker restore, fresh workspace-base npm ci/build preflight passed; a new whole invocation remains pending. Fresh ff0fbd9a wholeinvocation subsequently passed measurement/comparison budgets on attempt1; wrapper stillfails only existing disablednightly-performance registry (scope question pending).
- C100 live shutdown ata66eefed: six local admission/retry/backfill/webhook/freshness cases passed. Distributed replay failed: successful process join/runtime removal left target TaskRun running after restart; timeout case not reached. Atomic task settlement with explicit whole-owner cancellation marker implemented and independently reviewed; final native/live rerun pending. Fresh finalsource df7c69c1 all-eight live rerun subsequently PASS; initial failedattempt preserved.
- Coverage journey expansion and live SSO/backend coverage remain in progress; source SSO fixture is integrated with offline protocol tests passing. Coverage floors/policy unchanged.
- Native Podman prerequisite smoke passed with verified cleanup; no coverage contribution. Kubernetes prerequisite smoke is under diagnosis; actual same-image backend processes/profile hits remain pending.
- Final coverage collector source remains under Sol correction after independent review: retain original process counters through exit, separate aggregate destinations, exact image/cancellation/Running/timestamp evidence, checked flush and cleanup before eligible publication. Existing floors and source policy remain unchanged.

Complete-scope acceptance remains pending. Shared gates are scheduled by the coordinator; native builders and images have task-specific tags.

Required baseline commands: `just lint`, `just unit-test`, `just reagents-lint`, `just reagents-test`, `just integration-test`, plus affected auth, distributed/owner-memory, Podman, lifecycle and infrastructure lanes named by the work items and current CI workflow. Commands run from the aggregate worktree and use repository containers.

The original audit ledger remains local under `.audit/`; its immutable target is evidence, not the implementation checkout. Worker receipts and command logs are recovery evidence under `.codex/runs/go-maintenance/all/`. This tracked dashboard is the durable implementation status.
