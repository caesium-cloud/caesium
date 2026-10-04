# Go maintainability implementation

Status: **in progress**. One aggregate PR will be published after the complete scope is implemented and validated; worker branches are local integration units.

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

Current item states: assigned=53, pending=200.

| Item | Candidates | Work | State | Evidence |
| --- | --- | --- | --- | --- |
| W01 | C061, C058 | Pin CLI HTTP policies and separate output streams | pending | pending |
| W02 | C105 | Characterize local execution state and completion handoff | pending | pending |
| W249 | C032 | Pin predecessor projections and execution ownership | assigned | pending |
| W250 | C003 | Prove native foreign keys on both pools and new connections | assigned | pending |
| W03 | C002 | Pin caller-specific retry policies | assigned | pending |
| W04 | C002 | Pin database retry policy before extraction | assigned | pending |
| W05 | C054 | Characterize the read-only SQL guard | pending | pending |
| W06 | C097 | Use current standard-library idioms in api/rest/controller/jobdef | assigned | pending |
| W07 | C096 | Use current standard-library idioms in api/rest/controller/replay | assigned | pending |
| W08 | C096 | Use current standard-library idioms in api/rest/controller/system | assigned | pending |
| W09 | C097 | Use current standard-library idioms in api/rest/service/contract | assigned | pending |
| W10 | C097 | Use current standard-library idioms in api/rest/service/dataset | assigned | pending |
| W11 | C005 | Use current standard-library idioms in api/rest/service/replay | assigned | pending |
| W12 | C097 | Use current standard-library idioms in api/rest/service/system | assigned | pending |
| W13 | C005 | Use current standard-library idioms in cmd/contract | assigned | pending |
| W14 | C096 | Use current standard-library idioms in cmd/dataset | assigned | pending |
| W15 | C005, C097 | Use current standard-library idioms in cmd/job | assigned | pending |
| W16 | C005, C096 | Use current standard-library idioms in cmd/reproduce | assigned | pending |
| W17 | C005 | Use current standard-library idioms in cmd/why | assigned | pending |
| W18 | C096 | Use current standard-library idioms in internal/atom/kubernetes | assigned | pending |
| W19 | C005 | Use current standard-library idioms in internal/cache | assigned | pending |
| W20 | C005 | Use current standard-library idioms in internal/connector | assigned | pending |
| W21 | C005 | Use current standard-library idioms in internal/contract | assigned | pending |
| W22 | C005 | Use current standard-library idioms in internal/freshness | assigned | pending |
| W23 | C005 | Use current standard-library idioms in internal/harness | assigned | pending |
| W24 | C005 | Use current standard-library idioms in internal/incident | assigned | pending |
| W25 | C005, C096 | Use current standard-library idioms in internal/jobdef | assigned | pending |
| W26 | C005 | Use current standard-library idioms in internal/jobdef/runtime | assigned | pending |
| W27 | C005 | Use current standard-library idioms in internal/lineage | assigned | pending |
| W28 | C005 | Use current standard-library idioms in internal/reproduce | assigned | pending |
| W29 | C005, C096 | Use current standard-library idioms in internal/run | pending | pending |
| W30 | C005 | Use current standard-library idioms in internal/testfault | assigned | pending |
| W31 | C005 | Use current standard-library idioms in internal/worker | assigned | pending |
| W32 | C096 | Use current standard-library idioms in pkg/dqlite | assigned | pending |
| W33 | C096 | Use current standard-library idioms in pkg/env | assigned | pending |
| W34 | C005 | Use current standard-library idioms in pkg/jobdef/schemacompat | assigned | pending |
| W35 | C005 | Use current standard-library idioms in pkg/task | assigned | pending |
| W36 | C097 | Use current standard-library idioms in reagents/cmd/tf-discover | assigned | pending |
| W37 | C005 | Use current standard-library idioms in reagents/cmd/tf-runner | assigned | pending |
| W38 | C005 | Use current standard-library idioms in reagents/cmd/tf-warm | assigned | pending |
| W39 | C097 | Use current standard-library idioms in reagents/internal/fingerprint | assigned | pending |
| W40 | C005, C097 | Use current standard-library idioms in reagents/internal/protocol | assigned | pending |
| W41 | C005, C097 | Use current standard-library idioms in reagents/internal/tf | assigned | pending |
| W42 | C005, C096, C097 | Use current standard-library idioms in test | assigned | pending |
| W43 | C005, C096, C097 | Use current standard-library idioms in test/lifecycle | assigned | pending |
| W44 | C005, C097 | Use current standard-library idioms in test/model | assigned | pending |
| W45 | C096 | Use current standard-library idioms in test/performance | assigned | pending |
| W46 | C097 | Use current standard-library idioms in test/robustness | assigned | pending |
| W47 | C005 | Use current standard-library idioms in test/robustness/faults | assigned | pending |
| W48 | C005, C097 | Use current standard-library idioms in test/robustness/history | assigned | pending |
| W251 | C003 | Remove redundant FK setup and clarify native PRAGMA policy | assigned | pending |
| W49 | C001 | Reject byte-size suffix multiplication overflow | assigned | pending |
| W50 | C044 | Reject malformed partition edges during recovery | pending | pending |
| W51 | C077 | Reject incomplete SSE event backlogs | pending | pending |
| W52 | C079 | Reject malformed boolean environment values before load qualification | pending | pending |
| W53 | C083 | Require durable snapshots before claiming rejected requests are state-inert | pending | pending |
| W54 | C086 | Treat malformed accepted start outcomes as uncertain | pending | pending |
| W55 | C098 | Stop the distributed atom before returning partition marker errors | assigned | pending |
| W56 | C099 | Register local whole-run retries before starting execution | pending | pending |
| W57 | C014, C015, C016 | Keep cron recurrence alive and errors local | assigned | pending |
| W252 | C032 | Read and project predecessor inputs from one strict snapshot | assigned | pending |
| W253 | C032 | Fail before launch and reuse predecessor inputs across retries | assigned | pending |
| W58 | C008 | Chunk wide lineage frontiers before building SQL predicates | pending | pending |
| W59 | C009 | Share the repeated task lifecycle event mapping | pending | pending |
| W60 | C010 | Reclaim expired job-cache entries | pending | pending |
| W61 | C012 | Batch consumed-dataset state reads | pending | pending |
| W62 | C018 | Classify Docker image absence with the typed error | pending | pending |
| W63 | C019 | Share predecessor-output name reconstruction | pending | pending |
| W64 | C024 | Normalize job aliases in one auth helper | pending | pending |
| W65 | C026 | Share the SQL and in-memory dispatch batching loop | pending | pending |
| W66 | C028 | Keep deterministic policy rules from dispatching approval-tier actions | pending | pending |
| W67 | C033 | Use cache.HashInput directly for local hash inputs | pending | pending |
| W68 | C040 | Share fan-out TaskRun materialization between SQL and owner paths | pending | pending |
| W69 | C043 | Share schema-violation policy across catalog and instance validation | pending | pending |
| W70 | C045 | Use one SQL set for terminal task statuses | pending | pending |
| W71 | C046 | Propagate execution context through data-assertion evaluation | pending | pending |
| W72 | C049 | Share run and partition page-bound parsing | pending | pending |
| W73 | C053 | Propagate statistics query failures | pending | pending |
| W74 | C054 | Share the SQL quote and comment scanner | pending | pending |
| W75 | C063 | Centralize tf-runner's plan-to-apply test wiring | pending | pending |
| W76 | C073 | Reap developer-journey CLI children on early failures | pending | pending |
| W77 | C080 | Reject unknown names in mixed performance workload selections | pending | pending |
| W78 | C082 | Use one percentile convention for end-to-end latency fields | pending | pending |
| W79 | C084 | Fail soak drain when owned task resources remain after grace | pending | pending |
| W80 | C085 | Join the retention checkpoint poller on every exit | pending | pending |
| W81 | C089 | Restrict the reserved actor check to the root action schema | pending | pending |
| W82 | C095 | Scan worker aggregates into a typed result | pending | pending |
| W83 | C103 | Correlate SSE expectations with the triggered run | pending | pending |
| W84 | C104 | Replace the fixed stagger in the concurrent warm test | pending | pending |
| W85 | C038, C039 | Share completion field encoding and failure messages | pending | pending |
| W86 | C068, C075 | Wait on retry state instead of sleeps | pending | pending |
| W87 | C048 | Reject malformed atom UUIDs before service calls | pending | pending |
| W88 | C048 | Reject malformed trigger UUIDs before service calls | pending | pending |
| W89 | C006 | Share the duplicated cache-hash test setup | pending | pending |
| W90 | C013 | Share dataset_advanced event construction | pending | pending |
| W91 | C020 | Centralize repeated reproduction descriptor test defaults | pending | pending |
| W92 | C022 | Remove the no-op Kubernetes Atom constructor | pending | pending |
| W93 | C030 | Share the failed-first task-run attribution selector | pending | pending |
| W94 | C037 | Trigger-rule success and failure scenarios duplicate their DAG setup | pending | pending |
| W95 | C047 | Table-drive OIDC and SAML callback persistence coverage | pending | pending |
| W96 | C067 | Use the shared raw CLI runner for expected errors | pending | pending |
| W97 | C071 | Share the task-ID-to-name index across integration helpers | pending | pending |
| W98 | C088 | Give the host-request encoder a concrete request type | pending | pending |
| W99 | C106 | Remove the unused bus dispatcher tuning options | pending | pending |
| W100 | C069, C070, C074 | Consolidate typed integration manifest fixtures | pending | pending |
| W101 | C076, C078 | Share lifecycle matrix and membership reads | pending | pending |
| W102 | C057, C058, C060 | Consolidate helpers and error handling in cmd/auth | pending | pending |
| W103 | C057, C058 | Consolidate helpers and error handling in cmd/backfill | pending | pending |
| W104 | C057, C058 | Consolidate helpers and error handling in cmd/cache | pending | pending |
| W105 | C057, C058 | Consolidate helpers and error handling in cmd/receipt | pending | pending |
| W106 | C058 | Consolidate helpers and error handling in cmd/reproduce | pending | pending |
| W107 | C058 | Consolidate helpers and error handling in cmd/verify | pending | pending |
| W108 | C058, C060 | Consolidate helpers and error handling in cmd/why | pending | pending |
| W109 | C087 | Introduce strict internal SQL evidence decoding | pending | pending |
| W110 | C087 | Migrate cluster strict query to the C087 helper | pending | pending |
| W111 | C087 | Migrate robustness persisted evidence to the C087 helper | pending | pending |
| W112 | C087 | Migrate robustness scalar and checkpoint evidence to the C087 helper | pending | pending |
| W113 | C057 | Consolidate helpers and error handling in cmd/agentprofile | pending | pending |
| W114 | C057 | Consolidate helpers and error handling in cmd/contract | pending | pending |
| W115 | C102 | Consolidate helpers and error handling in cmd/dataset | pending | pending |
| W116 | C057 | Consolidate helpers and error handling in cmd/dev | pending | pending |
| W117 | C057 | Consolidate helpers and error handling in cmd/event | pending | pending |
| W118 | C057 | Consolidate helpers and error handling in cmd/job | pending | pending |
| W119 | C057, C060, C062 | Consolidate helpers and error handling in cmd/run | pending | pending |
| W120 | C057, C102 | Consolidate helpers and error handling in cmd/start | pending | pending |
| W121 | C057 | Consolidate helpers and error handling in cmd/test | pending | pending |
| W122 | C102 | Consolidate helpers and error handling in reagents/cmd/tf-warm | pending | pending |
| W123 | C102 | Consolidate helpers and error handling in test/lifecycle | pending | pending |
| W124 | C002 | Introduce caller-configured database retry mechanics | assigned | pending |
| W125 | C002 | Migrate auth to the C002 helper | pending | pending |
| W126 | C002 | Migrate backfill to the C002 helper | pending | pending |
| W127 | C002 | Migrate callback to the C002 helper | pending | pending |
| W128 | C002 | Migrate freshness to the C002 helper | pending | pending |
| W129 | C002 | Migrate jobdef importer to the C002 helper | pending | pending |
| W130 | C002 | Migrate run store to the C002 helper | pending | pending |
| W131 | C002 | Migrate worker to the C002 helper | pending | pending |
| W132 | C017 | Consolidate event matching and scalar formatting | pending | pending |
| W133 | C017 | Migrate HTTP trigger to the C017 helper | pending | pending |
| W134 | C017 | Migrate contract matcher to the C017 helper | pending | pending |
| W135 | C017 | Migrate jobdef matcher to the C017 helper | pending | pending |
| W136 | C021 | Share container state and exit-code mappings | pending | pending |
| W137 | C021 | Migrate Kubernetes to the C021 helper | pending | pending |
| W138 | C021 | Migrate Podman to the C021 helper | pending | pending |
| W139 | C023 | Use the SQL uniqueness classifier in user insertion | pending | pending |
| W140 | C023 | Migrate SAML assertions to the C023 helper | pending | pending |
| W141 | C025 | Share SSO return-target and random-state primitives | pending | pending |
| W142 | C025 | Migrate SAML state to the C025 helper | pending | pending |
| W143 | C029 | Share the active-authentication predicate | pending | pending |
| W144 | C029 | Migrate incident auth gate to the C029 helper | pending | pending |
| W145 | C031 | Share event subscription lifecycle | pending | pending |
| W146 | C031 | Migrate incident subscriber to the C031 helper | pending | pending |
| W147 | C031 | Migrate lineage subscriber to the C031 helper | pending | pending |
| W148 | C031 | Migrate notification subscriber to the C031 helper | pending | pending |
| W149 | C035 | Share execution retry-delay calculation | pending | pending |
| W150 | C035 | Migrate worker retry delay to the C035 helper | pending | pending |
| W151 | C042 | Share fan-out group detection | pending | pending |
| W152 | C042 | Migrate run fan-out groups to the C042 helper | pending | pending |
| W153 | C050 | Introduce the bounded body reader | pending | pending |
| W154 | C081 | Require complete HTTP evidence in test/lifecycle | pending | pending |
| W155 | C081 | Require complete HTTP evidence in test/load | pending | pending |
| W156 | C081 | Require complete HTTP evidence in test/performance | pending | pending |
| W157 | C081 | Require complete HTTP evidence in test/robustness/cluster | pending | pending |
| W158 | C081 | Require complete HTTP evidence in test/robustness/faults | pending | pending |
| W159 | C081 | Require complete HTTP evidence in test/robustness | pending | pending |
| W160 | C050 | Migrate webhook body limit to the C050 helper | pending | pending |
| W161 | C051 | Share allowlisted order parsing | pending | pending |
| W162 | C051 | Migrate notification ordering to the C051 helper | pending | pending |
| W163 | C055 | Share exact engine membership | pending | pending |
| W164 | C055 | Migrate agentprofile engines to the C055 helper | pending | pending |
| W165 | C061 | Share CLI HTTP transport | pending | pending |
| W166 | C061 | Migrate auth HTTP to the C061 helper | pending | pending |
| W167 | C061 | Migrate contract HTTP to the C061 helper | pending | pending |
| W168 | C061 | Migrate dataset HTTP to the C061 helper | pending | pending |
| W169 | C061 | Migrate incident HTTP to the C061 helper | pending | pending |
| W170 | C065 | Share Terraform child-output routing | pending | pending |
| W171 | C065 | Migrate tf-discover output to the C065 helper | pending | pending |
| W172 | C065 | Migrate tf-warm output to the C065 helper | pending | pending |
| W173 | C090 | Share trigger-pattern configuration parsing | pending | pending |
| W174 | C090 | Migrate contract pattern parser to the C090 helper | pending | pending |
| W175 | C091 | Expose the existing internal contract alias query | pending | pending |
| W176 | C091 | Migrate jobdef alias reads to the C091 helper | pending | pending |
| W177 | C092 | Share Reagents environment-name normalization | pending | pending |
| W178 | C092 | Migrate tf-runner names to the C092 helper | pending | pending |
| W179 | C094 | Share Prometheus sample selection | pending | pending |
| W180 | C094 | Migrate robustness metrics to the C094 helper | pending | pending |
| W181 | C060, C062 | Consolidate helpers and error handling in cmd/blame | pending | pending |
| W182 | C007 | Introduce a dependency-free YAML path leaf | pending | pending |
| W183 | C007 | Migrate jobdef Git to the C007 helper | pending | pending |
| W184 | C007 | Migrate jobdef diff to the C007 helper | pending | pending |
| W185 | C011 | Reuse OpenTestDB in the first lineage suite | pending | pending |
| W186 | C011 | Migrate agentprofile tests to the C011 helper | pending | pending |
| W187 | C011 | Migrate atom tests to the C011 helper | pending | pending |
| W188 | C011 | Migrate job tests to the C011 helper | pending | pending |
| W189 | C011 | Migrate notification tests to the C011 helper | pending | pending |
| W190 | C011 | Migrate receipt tests to the C011 helper | pending | pending |
| W191 | C011 | Migrate stats tests to the C011 helper | pending | pending |
| W192 | C011 | Migrate task tests to the C011 helper | pending | pending |
| W193 | C011 | Migrate taskedge tests to the C011 helper | pending | pending |
| W194 | C011 | Migrate trigger tests to the C011 helper | pending | pending |
| W195 | C011 | Migrate worker tests to the C011 helper | pending | pending |
| W196 | C027 | Reuse metric value helpers in dispatch tests | pending | pending |
| W197 | C027 | Migrate metrics tests to the C027 helper | pending | pending |
| W198 | C027 | Migrate run metrics tests to the C027 helper | pending | pending |
| W199 | C034 | Share run-parameter environment construction | pending | pending |
| W200 | C034 | Migrate worker params to the C034 helper | pending | pending |
| W201 | C036 | Share task-failure policy normalization | pending | pending |
| W202 | C036 | Migrate worker failure policy to the C036 helper | pending | pending |
| W203 | C052 | Share API audit-failure logging | pending | pending |
| W204 | C052 | Migrate auth audit warnings to the C052 helper | pending | pending |
| W205 | C052 | Migrate notification audit warnings to the C052 helper | pending | pending |
| W206 | C056 | Share test JSON fixture encoding | pending | pending |
| W207 | C056 | Migrate API replay JSON fixtures to the C056 helper | pending | pending |
| W208 | C056 | Migrate incident JSON fixtures to the C056 helper | pending | pending |
| W209 | C056 | Migrate jobdef diff JSON fixtures to the C056 helper | pending | pending |
| W210 | C056 | Migrate notification JSON fixtures to the C056 helper | pending | pending |
| W211 | C056 | Migrate replay JSON fixtures to the C056 helper | pending | pending |
| W212 | C059 | Share first-nonblank string selection | pending | pending |
| W213 | C059 | Migrate job lint to the C059 helper | pending | pending |
| W214 | C059 | Migrate reproduce to the C059 helper | pending | pending |
| W215 | C059 | Migrate reproduce CLI to the C059 helper | pending | pending |
| W216 | C059 | Migrate run strings to the C059 helper | pending | pending |
| W217 | C064 | Share Terraform environment copying | pending | pending |
| W218 | C064 | Migrate tf-warm environment to the C064 helper | pending | pending |
| W219 | C066 | Share deterministic Git fixture commands | pending | pending |
| W220 | C066 | Migrate tf Git fixtures to the C066 helper | pending | pending |
| W221 | C066 | Migrate tf-discover Git fixtures to the C066 helper | pending | pending |
| W222 | C072 | Share the loopback-address fixture | pending | pending |
| W223 | C072 | Migrate Linux nodelay fixture to the C072 helper | pending | pending |
| W224 | C107 | Share the scoped GORM fault fixture | pending | pending |
| W225 | C107 | Migrate freshness failure fixtures to the C107 helper | pending | pending |
| W226 | C041 | Replace typed union-key copies with one generic helper | pending | pending |
| W227 | C105 | Extract local queue and trigger bookkeeping | pending | pending |
| W228 | C105 | Move local cache identity and rate-limit methods | pending | pending |
| W229 | C105 | Move the atom execution closure | pending | pending |
| W230 | C105 | Move fan-out execution and its shared state | pending | pending |
| W231 | C105 | Move task dispatch onto localRun | pending | pending |
| W232 | C105 | Finish local scheduler execution handoff | pending | pending |
| W233 | C100 | Introduce the server work supervisor | pending | pending |
| W234 | C100 | Wire the supervisor into API and startup shutdown | pending | pending |
| W235 | C100 | Migrate backfill launches to the C100 helper | pending | pending |
| W236 | C100 | Migrate manual launches to the C100 helper | pending | pending |
| W237 | C100 | Migrate partition retry launches to the C100 helper | pending | pending |
| W238 | C100 | Migrate replay launches to the C100 helper | pending | pending |
| W239 | C100 | Migrate webhook launches and receipts to the C100 helper | pending | pending |
| W240 | C100 | Migrate whole-run retry launches to the C100 helper | pending | pending |
| W241 | C101 | Add the signal-context protocol entry point | pending | pending |
| W242 | C101 | Migrate tf-discover signals to the C101 helper | pending | pending |
| W243 | C101 | Migrate tf-runner signals to the C101 helper | pending | pending |
| W244 | C101 | Migrate tf-warm signals to the C101 helper | pending | pending |
| W245 | C004 | Use the declared backfill enum types in the model | pending | pending |
| W246 | C093 | Introduce the shared workload catalogue schema | pending | pending |
| W247 | C093 | Migrate load catalogue to the C093 helper | pending | pending |
| W248 | C093 | Migrate performance catalogue to the C093 helper | pending | pending |

## Verification status

No execution acceptance gate has yet passed. Shared gates are scheduled by the coordinator; native builders and images have task-specific tags.

Required baseline commands: `just lint`, `just unit-test`, `just reagents-lint`, `just reagents-test`, `just integration-test`, plus affected auth, distributed/owner-memory, Podman, lifecycle and infrastructure lanes named by the work items and current CI workflow. Commands run from the aggregate worktree and use repository containers.

The original audit ledger remains local under `.audit/`; its immutable target is evidence, not the implementation checkout. Worker receipts and command logs are recovery evidence under `.codex/runs/go-maintenance/all/`. This tracked dashboard is the durable implementation status.
