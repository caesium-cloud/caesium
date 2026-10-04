#!/usr/bin/env bash
# Additional real integration journeys for integration-coverage.sh.
#
# This file is sourced by the collector, not run as a standalone command. It
# uses the already-built, immutable coverage image for each server and CLI,
# and an already-loaded full builder image only as the integration-test runner.
# It never builds or pulls images. Each lane writes into a separate raw
# GOCOVERDIR; only lanes with passing real-surface tests and clean process
# shutdown are merged into the collector's CLI/server profiles.

COVERAGE_JOURNEY_ACTIVE_IDS=()
COVERAGE_JOURNEY_TEMP_DIRS=()
COVERAGE_JOURNEY_SECRET_FILES=()
COVERAGE_JOURNEY_CLI_DIRS=()
COVERAGE_JOURNEY_SERVER_DIRS=()
COVERAGE_JOURNEY_NAMES=()

coverage_journey_fail() {
  log "ERROR: coverage journey: $*"
  return 1
}

coverage_journey_track_id() {
  COVERAGE_JOURNEY_ACTIVE_IDS+=("$1")
}

coverage_journey_untrack_id() {
  local want="$1"
  local kept=()
  local id
  if ((${#COVERAGE_JOURNEY_ACTIVE_IDS[@]} == 0)); then
    return
  fi
  for id in "${COVERAGE_JOURNEY_ACTIVE_IDS[@]}"; do
    [[ "$id" == "$want" ]] || kept+=("$id")
  done
  COVERAGE_JOURNEY_ACTIVE_IDS=("${kept[@]}")
}

coverage_journey_remove_owned() {
  local id="$1"
  local owner run_id
  owner="$("$CONTAINER_CLI" inspect -f '{{index .Config.Labels "caesium.coverage.owner"}}' "$id" 2>/dev/null || true)"
  run_id="$("$CONTAINER_CLI" inspect -f '{{index .Config.Labels "caesium.coverage.run"}}' "$id" 2>/dev/null || true)"
  if [[ "$owner" == "$CANDIDATE_SHA" && "$run_id" == "$ID" ]]; then
    "$CONTAINER_CLI" rm -f "$id" >/dev/null 2>&1 || true
  elif [[ -n "$owner" || -n "$run_id" ]]; then
    log "refusing to remove container $id with unexpected coverage ownership labels"
  fi
  coverage_journey_untrack_id "$id"
}

cleanup_coverage_journeys() {
  local id path
  if ((${#COVERAGE_JOURNEY_ACTIVE_IDS[@]})); then
    for id in "${COVERAGE_JOURNEY_ACTIVE_IDS[@]}"; do coverage_journey_remove_owned "$id"; done
  fi
  if ((${#COVERAGE_JOURNEY_SECRET_FILES[@]})); then
    for path in "${COVERAGE_JOURNEY_SECRET_FILES[@]}"; do rm -f "$path"; done
  fi
  COVERAGE_JOURNEY_SECRET_FILES=()
  if ((${#COVERAGE_JOURNEY_TEMP_DIRS[@]})); then
    for path in "${COVERAGE_JOURNEY_TEMP_DIRS[@]}"; do rm -rf "$path"; done
  fi
  COVERAGE_JOURNEY_TEMP_DIRS=()
}

coverage_journey_log_redacted() {
  # Bootstrap/API keys must never be persisted in coverage artifacts or echoed
  # into CI logs, including when a test assertion prints a response body.
  sed -E 's/csk_[[:alnum:]_-]+/[REDACTED_API_KEY]/g'
}

coverage_journey_free_tcp_port() {
  python3 - <<'PY'
import socket

with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
    listener.bind(("0.0.0.0", 0))
    print(listener.getsockname()[1])
PY
}

coverage_journey_write_record() {
  local lane="$1" pattern="$2" min_pass="$3" test_rc="$4" passes="$5"
  local server_id="$6" stop_rc="$7" exit_code="$8" oom="$9"
  local complete="${10}" missing="${11}" killed="${12}"
  local lane_dir="$RAW/journeys/$lane"
  JOURNEY_RECORD_LANE="$lane" \
  JOURNEY_RECORD_PATTERN="$pattern" \
  JOURNEY_RECORD_MIN_PASS="$min_pass" \
  JOURNEY_RECORD_TEST_RC="$test_rc" \
  JOURNEY_RECORD_PASSES="$passes" \
  JOURNEY_RECORD_SERVER_ID="$server_id" \
  JOURNEY_RECORD_STOP_RC="$stop_rc" \
  JOURNEY_RECORD_EXIT_CODE="$exit_code" \
  JOURNEY_RECORD_OOM="$oom" \
  JOURNEY_RECORD_COMPLETE="$complete" \
  JOURNEY_RECORD_MISSING="$missing" \
  JOURNEY_RECORD_KILLED="$killed" \
  JOURNEY_RECORD_SHA="$CANDIDATE_SHA" \
  JOURNEY_RECORD_IMAGE_ID="$IMAGE_ID" \
  JOURNEY_RECORD_BUILD_CONTEXT="$BUILD_CONTEXT" \
  JOURNEY_RECORD_IMAGE_PROVENANCE="$IMAGE_PROVENANCE" \
  JOURNEY_RECORD_VERIFIED="$IMAGE_VERIFIED" \
  JOURNEY_RECORD_TEST_LOG="$ARTIFACTS/journeys/$lane.log" \
  python3 - "$lane_dir/provenance.json" <<'PY'
import json
import os
import pathlib
import sys

def boolean(name):
    return os.environ[name] == "true"

record = {
    "schema_version": 1,
    "source": "integration-journey",
    "lane": os.environ["JOURNEY_RECORD_LANE"],
    "kind": "gocoverdir-pair",
    "module": "github.com/caesium-cloud/caesium",
    "candidate_sha": os.environ["JOURNEY_RECORD_SHA"],
    "image_id": os.environ["JOURNEY_RECORD_IMAGE_ID"],
    "build_context": json.loads(os.environ["JOURNEY_RECORD_BUILD_CONTEXT"]),
    "image_provenance": os.environ["JOURNEY_RECORD_IMAGE_PROVENANCE"],
    "verified": boolean("JOURNEY_RECORD_VERIFIED"),
    "complete": boolean("JOURNEY_RECORD_COMPLETE"),
    "missing": boolean("JOURNEY_RECORD_MISSING"),
    "killed": boolean("JOURNEY_RECORD_KILLED"),
    "test": {
        "run_pattern": os.environ["JOURNEY_RECORD_PATTERN"],
        "minimum_passes": int(os.environ["JOURNEY_RECORD_MIN_PASS"]),
        "passed_scenarios": int(os.environ["JOURNEY_RECORD_PASSES"]),
        "exit_code": int(os.environ["JOURNEY_RECORD_TEST_RC"]),
        "log": os.environ["JOURNEY_RECORD_TEST_LOG"],
    },
    "server": {
        "container_id": os.environ["JOURNEY_RECORD_SERVER_ID"],
        "exit_code": int(os.environ["JOURNEY_RECORD_EXIT_CODE"]),
        "stop_rc": int(os.environ["JOURNEY_RECORD_STOP_RC"]),
        "flush": "sigusr2",
        "signal": "SIGTERM",
        "oom_killed": boolean("JOURNEY_RECORD_OOM"),
    },
    "raw": {"cli": "cli", "server": "server"},
}
pathlib.Path(sys.argv[1]).write_text(json.dumps(record, indent=2) + "\n")
PY
}

coverage_journey_server_env() {
  local mode="$1"
  COVERAGE_JOURNEY_SERVER_ENV=(
    CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key
    CAESIUM_EVENT_INGEST_API_KEY="${CAESIUM_EVENT_INGEST_API_KEY:-integration-test-key}"
    CAESIUM_LOG_LEVEL=debug
    CAESIUM_DATABASE_SHARDS=4
    CAESIUM_OPEN_LINEAGE_ENABLED=true
    CAESIUM_OPEN_LINEAGE_TRANSPORT=console
    CAESIUM_FRESHNESS_ENABLED=true
    CAESIUM_DATA_ASSERTIONS_ENABLED=true
    CAESIUM_RESOURCE_STATS_ENABLED=true
    CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL=100ms
    CAESIUM_RIGHT_SIZING_ENABLED=true
    CAESIUM_CONTRACT_ENFORCEMENT=fail
    CAESIUM_CONTRACT_DEPRECATION_WINDOW="${CAESIUM_CONTRACT_DEPRECATION_WINDOW:-5s}"
    CAESIUM_CACHE_PIN_DIGESTS=true
    CAESIUM_REGISTRY_AUTH=127.0.0.1=secret://env/CAESIUM_IT_REGISTRY_CREDS
    CAESIUM_IT_REGISTRY_CREDS=ci-user:ci-pass
    CAESIUM_CACHE_ENABLED=true
    CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s
    CAESIUM_RATE_LIMIT_PRUNER_ENABLED=true
    CAESIUM_RATE_LIMIT_PRUNE_INTERVAL=500ms
    CAESIUM_RUN_QUEUE_ENABLED=true
    CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true
    CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms
    CAESIUM_FANOUT_MAX_PARTITIONS=8
    CAESIUM_CANCEL_RECONCILE_INTERVAL=2s
  )
  case "$mode" in
    local)
      COVERAGE_JOURNEY_SERVER_ENV+=(CAESIUM_AUTH_MODE=none)
      ;;
    auth)
      COVERAGE_JOURNEY_SERVER_ENV+=(
        CAESIUM_AUTH_MODE=api-key
        CAESIUM_AUTH_REQUIRE_TLS=false
        CAESIUM_AUTH_KEY_HASH_SECRET=agent-integration-auth-key-hash-secret-000001
        CAESIUM_CONNECTORS_ENABLED=true
        CAESIUM_CONNECTORS_CONFIG_FILE=/etc/caesium/connectors/connections.yaml
        TEMPORAL_TOKEN=agent-integration-connector-token
        CAESIUM_AGENT_REMEDIATION_ENABLED=true
        CAESIUM_AGENT_DEFAULT_PROFILE=triage-only
        CAESIUM_AGENT_MAX_CONCURRENT_SESSIONS=1
        CAESIUM_AGENT_SESSION_TIMEOUT=45s
        CAESIUM_AGENT_INCIDENT_COOLDOWN=1s
        CAESIUM_AGENT_APPROVAL_REDRIVE_INTERVAL=5s
      )
      ;;
    distributed|owner-memory)
      COVERAGE_JOURNEY_SERVER_ENV+=(
        CAESIUM_AUTH_MODE=none
        CAESIUM_EXECUTION_MODE=distributed
        CAESIUM_NODE_ADDRESS=127.0.0.1:9001
        CAESIUM_INTERNAL_WAKEUP_TOKEN=integration-distributed-internal-token
        CAESIUM_INTERNAL_PORT=8443
        CAESIUM_RUN_OWNER_ENABLED=true
        CAESIUM_RUN_LEASE_TTL=30s
        CAESIUM_RUN_OWNER_DISPATCH_INTERVAL=500ms
        CAESIUM_RUN_OWNER_DISPATCH_DEADLINE=5m
        CAESIUM_WORKER_ENABLED=true
        CAESIUM_WORKER_POOL_SIZE=1
        CAESIUM_WORKER_POLL_INTERVAL=500ms
        CAESIUM_WORKER_RECLAIM_INTERVAL=500ms
        CAESIUM_WORKER_LEASE_TTL=30s
      )
      if [[ "$mode" == "owner-memory" ]]; then
        COVERAGE_JOURNEY_SERVER_ENV+=(
          CAESIUM_RUN_OWNER_IN_MEMORY=true
          CAESIUM_RUN_OWNER_DISPATCH_PROGRESS_DEADLINE=5s
        )
      fi
      ;;
    *)
      coverage_journey_fail "unknown server mode '$mode'"
      return 1
      ;;
  esac
}

coverage_journey_run_lane() {
  local lane="$1" mode="$2" pattern="$3" min_pass="$4" cli_dir="$5"
  local lane_dir="$RAW/journeys/$lane"
  local server_name="${ID}-journey-${lane}"
  local server_id="" ready=0 test_rc=125 passes=0
  local stop_rc=0 exit_code=1 oom=true killed=false complete=false missing=true
  local key="" auth_env="" runner_log="$ARTIFACTS/journeys/$lane.log"
  local agent_port="" CAESIUM_AGENT_API_EXTERNAL_URL=""
  local cli_raw="$lane_dir/cli" server_raw="$lane_dir/server"
  local -a server_args runner_args

  if "$CONTAINER_CLI" inspect "$server_name" >/dev/null 2>&1; then
    coverage_journey_fail "refusing pre-existing container name $server_name"
    return 1
  fi
  if [[ -e "$lane_dir" || -L "$lane_dir" ]]; then
    coverage_journey_fail "refusing pre-existing lane artifact path $lane_dir"
    return 1
  fi
  mkdir -p "$cli_raw" "$server_raw" "$ARTIFACTS/journeys" \
    || { coverage_journey_fail "cannot create lane artifacts for '$lane'"; return 1; }
  chmod 0777 "$cli_raw" "$server_raw" \
    || { coverage_journey_fail "cannot prepare lane GOCOVERDIRs for '$lane'"; return 1; }

  coverage_journey_server_env "$mode" || return 1
  if [[ "$mode" == "auth" ]]; then
    agent_port="$(coverage_journey_free_tcp_port)" \
      || { coverage_journey_fail "cannot choose an isolated auth callback port"; return 1; }
    if [[ ! "$agent_port" =~ ^[0-9]{1,5}$ ]] || ((agent_port <= 0 || agent_port > 65535)); then
      coverage_journey_fail "auth callback port is invalid"
      return 1
    fi
    # Match the normal auth integration recipe's published-port route while
    # choosing a fresh port for this collector run. Do not inherit the
    # operator-facing justfile override into an isolated coverage server.
    CAESIUM_AGENT_API_EXTERNAL_URL="http://172.17.0.1:$agent_port"
    COVERAGE_JOURNEY_SERVER_ENV+=("CAESIUM_API_EXTERNAL_URL=$CAESIUM_AGENT_API_EXTERNAL_URL")
  fi
  server_args=(
    run -d --pull=never
    --name "$server_name"
    --label "caesium.coverage.owner=$CANDIDATE_SHA"
    --label "caesium.coverage.run=$ID"
    --label "caesium.coverage.lane=$lane"
    --platform "$PLATFORM"
    --network "$NETWORK"
    --privileged
    --user 10001:10001
    --group-add "$SOCK_GID"
    -e GOCOVERDIR=/var/lib/caesium/coverage
    -e CAESIUM_DATABASE_PATH=/var/lib/caesium/dqlite
    -e DOCKER_HOST=unix:///var/run/docker.sock
    -v "$SOCK:/var/run/docker.sock"
    -v "$server_raw:/var/lib/caesium/coverage"
  )
  if [[ -n "$agent_port" ]]; then
    server_args+=(-p "$agent_port:8080")
  fi
  if [[ "$mode" == "auth" ]]; then
    server_args+=(
      -v "$ROOT/test/fixtures/connectors/connections.yaml:/etc/caesium/connectors/connections.yaml:ro"
    )
  fi
  local env_value
  for env_value in "${COVERAGE_JOURNEY_SERVER_ENV[@]}"; do
    server_args+=(-e "$env_value")
  done
  server_args+=("$IMAGE_ID" start)

  log "starting real integration coverage lane '$lane' on $IMAGE_ID"
  server_id="$("$CONTAINER_CLI" "${server_args[@]}")" || {
    coverage_journey_fail "could not start server for lane '$lane'"
    return 1
  }
  [[ -n "$server_id" ]] || { coverage_journey_fail "lane '$lane' server has no container id"; return 1; }
  coverage_journey_track_id "$server_id"
  local actual_image
  actual_image="$("$CONTAINER_CLI" inspect -f '{{.Image}}' "$server_id" 2>/dev/null || true)"
  if [[ "$actual_image" != "$IMAGE_ID" ]]; then
    coverage_journey_fail "lane '$lane' server image $actual_image differs from pinned $IMAGE_ID"
    return 1
  fi

  for _ in $(seq 1 120); do
    if "$CONTAINER_CLI" run --pull=never --rm --platform "$PLATFORM" \
        --network "container:$server_id" --user 0:0 --entrypoint wget \
        "$IMAGE_ID" -q -O - http://127.0.0.1:8080/health 2>/dev/null | grep -q healthy; then
      ready=1
      break
    fi
    local running
    running="$("$CONTAINER_CLI" inspect -f '{{.State.Running}}' "$server_id" 2>/dev/null || true)"
    [[ "$running" == "true" ]] || break
    sleep 1
  done
  if [[ "$ready" -ne 1 ]]; then
    log "lane '$lane' server did not become healthy; sanitized logs follow"
    "$CONTAINER_CLI" logs "$server_id" 2>&1 | coverage_journey_log_redacted >&2 || true
    test_rc=125
  else
    if [[ "$lane" == "auth" ]]; then
      local server_logs
      server_logs="$("$CONTAINER_CLI" logs "$server_id" 2>&1 || true)"
      key="$(printf '%s\n' "$server_logs" | awk '/csk_/ { for (i = 1; i <= NF; i++) if ($i ~ /^csk_/) { print $i; exit } }' | tr -d '\r')"
      unset server_logs
      if [[ ! "$key" =~ ^csk_[[:alnum:]_-]+$ ]]; then
        coverage_journey_fail "auth lane did not emit a parseable bootstrap key"
        test_rc=125
      else
        auth_env="$(mktemp "${TMPDIR:-/tmp}/caesium-auth-env-$ID.XXXXXX")"
        chmod 0600 "$auth_env"
        COVERAGE_JOURNEY_SECRET_FILES+=("$auth_env")
        printf 'CAESIUM_AUTH_ADMIN_KEY=%s\nCAESIUM_API_KEY=%s\n' "$key" "$key" > "$auth_env"
      fi
    fi
    if [[ "$test_rc" -ne 125 || "$lane" != "auth" || -n "$auth_env" ]]; then
      runner_args=(
        run --pull=never --rm --platform "$PLATFORM"
        -v "$ROOT:/source"
        -v "$cli_dir:/coverage-cli:ro"
        -v "$SOCK:/var/run/docker.sock"
        -v "$cli_raw:/coverage"
        -e GOCOVERDIR=/coverage
        -e GOTOOLCHAIN=local
        -e GOPROXY=off
        -e GOFLAGS=-buildvcs=false
        -e CAESIUM_CLI_PATH=/coverage-cli/caesium
        -e CAESIUM_RESOURCE_STRESS_IMAGE="${CAESIUM_RESOURCE_STRESS_IMAGE:-caesiumcloud/resource-stress:latest}"
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key
        -e CAESIUM_EVENT_INGEST_API_KEY="${CAESIUM_EVENT_INGEST_API_KEY:-integration-test-key}"
        -e DOCKER_HOST=unix:///var/run/docker.sock
        --network "container:$server_id"
        -w /source
      )
      if [[ "$mode" == "auth" ]]; then
        runner_args+=(
          --env-file "$auth_env"
          -e CAESIUM_AGENT_AUTH_LANE=true
          -e CAESIUM_AUTH_MODE=api-key
          -e CAESIUM_AGENT_REMEDIATION_ENABLED=true
          -e CAESIUM_AUTH_KEY_HASH_SECRET=agent-integration-auth-key-hash-secret-000001
        )
      elif [[ "$mode" == "distributed" || "$mode" == "owner-memory" ]]; then
        runner_args+=(-e CAESIUM_EXECUTION_MODE=distributed)
        if [[ "$mode" == "owner-memory" ]]; then
          runner_args+=(-e CAESIUM_RUN_OWNER_IN_MEMORY=true)
        fi
      fi
      runner_args+=("$BUILDER_RUN_IMAGE" sh scripts/integration-test.sh -test.run "$pattern")
      set +e
      "$CONTAINER_CLI" "${runner_args[@]}" 2>&1 | coverage_journey_log_redacted | tee "$runner_log"
      local -a pipeline_status=("${PIPESTATUS[@]}")
      test_rc="${pipeline_status[0]:-125}"
      set -e
      passes="$(grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/' "$runner_log" 2>/dev/null || true)"
      passes="${passes:-0}"
    fi
  fi

  if [[ -n "$auth_env" ]]; then
    rm -f "$auth_env"
    COVERAGE_JOURNEY_SECRET_FILES=()
  fi
  unset key

  log "flushing '$lane' server coverage via SIGUSR2 and stopping with SIGTERM"
  "$CONTAINER_CLI" kill --signal=SIGUSR2 "$server_id" >/dev/null 2>&1 || true
  sleep 1
  "$CONTAINER_CLI" stop -t 60 "$server_id" >/dev/null || stop_rc=$?
  local inspect_json
  inspect_json="$("$CONTAINER_CLI" inspect "$server_id" 2>/dev/null || true)"
  if [[ -n "$inspect_json" ]]; then
    exit_code="$(printf '%s' "$inspect_json" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[0]["State"].get("ExitCode", 1) if d else 1)' 2>/dev/null || echo 1)"
    oom="$(printf '%s' "$inspect_json" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("true" if d and d[0]["State"].get("OOMKilled") else "false")' 2>/dev/null || echo true)"
  fi
  if [[ "$oom" == "true" || "$exit_code" == "137" ]]; then
    killed=true
  fi
  if [[ "$stop_rc" -eq 0 && "$killed" == false && ( "$exit_code" == "0" || "$exit_code" == "143" ) ]] \
      && gocoverdir_complete "$cli_raw" && gocoverdir_complete "$server_raw" \
      && [[ "$test_rc" -eq 0 && "$passes" -ge "$min_pass" ]]; then
    complete=true
    missing=false
  else
    missing=$(gocoverdir_complete "$cli_raw" && gocoverdir_complete "$server_raw" && echo false || echo true)
  fi
  coverage_journey_write_record "$lane" "$pattern" "$min_pass" "$test_rc" "$passes" \
    "$server_id" "$stop_rc" "$exit_code" "$oom" "$complete" "$missing" "$killed" \
    || { coverage_journey_fail "cannot write provenance for lane '$lane'"; return 1; }

  if [[ "$test_rc" -ne 0 || "$passes" -lt "$min_pass" ]]; then
    log "lane '$lane' test command exit=$test_rc passes=$passes minimum=$min_pass"
  fi
  if [[ "$complete" != "true" ]]; then
    log "lane '$lane' did not produce complete verified test+CLI+server evidence"
    if [[ "$test_rc" -ne 0 ]]; then
      log "lane '$lane' server logs (API keys redacted):"
      "$CONTAINER_CLI" logs "$server_id" 2>&1 | coverage_journey_log_redacted >&2 || true
    fi
    coverage_journey_remove_owned "$server_id"
    return 1
  fi

  COVERAGE_JOURNEY_CLI_DIRS+=("$cli_raw")
  COVERAGE_JOURNEY_SERVER_DIRS+=("$server_raw")
  COVERAGE_JOURNEY_NAMES+=("$lane")
  coverage_journey_remove_owned "$server_id"
}

run_coverage_journeys() {
  if [[ "$CONTAINER_CLI" != "docker" ]]; then
    coverage_journey_fail "selected live integration journeys require the Docker daemon and Docker test engine"
    return 1
  fi
  if [[ "$IMAGE_PROVENANCE" != "built-by-this-run" || "$IMAGE_VERIFIED" != "true" ]]; then
    coverage_journey_fail "real journeys require the coverage image built and verified by this collection"
    return 1
  fi
  mkdir -p "$RAW/journeys" "$ARTIFACTS/journeys"
  chmod 0777 "$RAW/journeys"

  # Extract the candidate CLI from the exact same immutable image as each
  # instrumented server. The test runner itself is not part of the coverage
  # profile; it only invokes this candidate binary and public HTTP surfaces.
  local cli_dir cli_ctr cli_digest lane
  cli_dir="$(mktemp -d "$ARTIFACTS/coverage-cli-$ID.XXXXXX")"
  COVERAGE_JOURNEY_TEMP_DIRS+=("$cli_dir")
  cli_ctr="$("$CONTAINER_CLI" create --pull=never --platform "$PLATFORM" \
    --label "caesium.coverage.owner=$CANDIDATE_SHA" \
    --label "caesium.coverage.run=$ID" \
    --label caesium.coverage.lane=cli-extract \
    --entrypoint true "$IMAGE_ID")" || {
      coverage_journey_fail "could not create candidate CLI extraction container"
      return 1
    }
  coverage_journey_track_id "$cli_ctr"
  if ! "$CONTAINER_CLI" cp "$cli_ctr:/bin/caesium" "$cli_dir/caesium"; then
    coverage_journey_fail "could not extract candidate CLI from $IMAGE_ID"
    return 1
  fi
  coverage_journey_remove_owned "$cli_ctr"
  chmod 0755 "$cli_dir/caesium"
  cli_digest="$(python3 - "$cli_dir/caesium" <<'PY'
import hashlib
import pathlib
import sys
print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
)" || { coverage_journey_fail "cannot fingerprint candidate CLI"; return 1; }
  [[ "$cli_digest" =~ ^[0-9a-f]{64}$ ]] || { coverage_journey_fail "candidate CLI digest is invalid"; return 1; }
  printf '{"schema_version":1,"candidate_sha":"%s","image_id":"%s","cli_sha256":"%s","image_provenance":"%s","verified":%s}\n' \
    "$CANDIDATE_SHA" "$IMAGE_ID" "$cli_digest" "$IMAGE_PROVENANCE" "$IMAGE_VERIFIED" \
    > "$ARTIFACTS/journeys/cli-image.json" || { coverage_journey_fail "cannot record candidate CLI identity"; return 1; }

  # Each regex is suite-qualified. Distributed and owner-memory filters match
  # the checked-in justfile lanes; the local/auth filters add existing live
  # API/CLI journeys that exercise changed caller paths without relying on
  # synthetic handler or unit profiles.
  local local_pattern auth_pattern distributed_pattern owner_pattern
  local_pattern='TestIntegrationTestSuite/(TestEventAndTriggerCLIWithWebhookReceiptLog|TestEventIngestRoutesEventTriggerJob|TestWebhookPathFiresHTTPAndEventTriggers|TestJobApplyThenExportInspectsDeployedPipeline|TestJobDiffCLIPrintsBreakingContractFindings|TestJobApplyReconcilesExistingDefinition|TestCacheHitSkipsExecution|TestCacheHitDAGOutputPropagation|TestCacheChainValuesBreaksUpstreamChurn|TestCacheManagementListAndInvalidate|TestCacheInvalidationForcesReexecution|TestFreshnessEvaluatorStateTransitions|TestFreshnessConsumedSnapshotTakenAtRunStart|TestFreshnessDerivedRunExecutesWithoutConcurrencyPolicy|TestFreshnessDerivedRunExecutesUnderConcurrencyPolicy|TestFreshnessDerivedRunHonoursJobPause|TestFreshnessDerivedRunCoalescesAJobsStaleOutputs|TestArrivalEventAdvancesSourceDatasetWatermark|TestLineageImpactReturnsDownstream|TestBackfillCLILifecycle|TestBackfillBasicHappyPath|TestBackfillListAndGet|TestBackfillReprocessNone|TestBackfillReprocessAll|TestBackfillValidationEndBeforeStart|TestNotificationChannelAndPolicyByID|TestReplayRESTEndpointIdempotencyAndSafety|TestReplayRESTEndpointConcurrentIdempotency|TestReplayRESTEndpointBaselineWithoutTaskRunsIs4xx|TestReplayMatrixCachePrunedFailsClosed|TestReplayMatrixBaselineScopedReplaySafeGate|TestRunReplayCLIJSONStdoutIdempotencyAndDiff|TestRunRetryCLIRoutesOverServer|TestReproduceCLIDryRunAndRunMode|TestReproduceDescriptorEndpointRoundTrip|TestDataAssertionsFeatureFlagIsReportedByLiveServer|TestDataAssertionsDatasetOperatorReads|TestRunListPaginationPagesRealRuns)'
  auth_pattern='TestIntegrationTestSuite/(TestAgentProfileCLIListJSONStdout|TestAgentProfileCRUD|TestAgentProfileCreateRejectsUnsupportedSecretProvider|TestJobdefLintVerifiesRemediationProfileReference|TestAuthKeyLifecycleCLI|TestAuthAuditCLI|TestAuthKeysREST|TestAuthJobApplyCLI|TestNotificationChannelAndPolicyMutationsAreAudited)'
  distributed_pattern='TestIntegrationTestSuite/(TestRunConcurrencyStrategies|TestPriorityRunStartSurfacesAndCronDefault|TestFanOut|TestPlainFailure|TestSecretLogs|TestHaltPolicy|TestReplaceCancel|TestRetryAfterApplyExecutesRegisteredCommand|TestRetryValidatesAgainstTheRegisteredOutputSchema|TestDataAssertionsMetricsPersisted|TestResourceStats)'
  owner_pattern='TestIntegrationTestSuite/(TestFanOut|TestPlainFailure|TestSecretLogs|TestHaltPolicy|TestResourceStats)'

  local local_min_pass=20
  local auth_min_pass=8
  local distributed_min_pass="${CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS:-20}"
  local owner_min_pass="${CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS:-14}"
  local server_raw cli_raw
  for lane in local auth distributed owner-memory; do
    case "$lane" in
      local)
        coverage_journey_run_lane "$lane" local "$local_pattern" "$local_min_pass" "$cli_dir" || return 1
        ;;
      auth)
        coverage_journey_run_lane "$lane" auth "$auth_pattern" "$auth_min_pass" "$cli_dir" || return 1
        ;;
      distributed)
        coverage_journey_run_lane "$lane" distributed "$distributed_pattern" "$distributed_min_pass" "$cli_dir" || return 1
        ;;
      owner-memory)
        coverage_journey_run_lane "$lane" owner-memory "$owner_pattern" "$owner_min_pass" "$cli_dir" || return 1
        ;;
      esac
  done

  local sso_log sso_rc
  sso_log="$(mktemp "$ARTIFACTS/journeys/sso-driver-$ID.XXXXXX.log")" \
    || { coverage_journey_fail "cannot allocate SSO driver log"; return 1; }
  set +e
  python3 "$ROOT/scripts/coverage-journeys.py" sso \
    --root "$ROOT" \
    --artifacts "$ARTIFACTS/journeys" \
    --raw "$RAW" \
    --coverage-image "$IMAGE_ID" \
    --builder-image "$BUILDER_RUN_IMAGE" \
    --platform "$PLATFORM" \
    --candidate-sha "$CANDIDATE_SHA" \
    --run-id "$ID" \
    --build-context "$BUILD_CONTEXT" \
    --image-provenance "$IMAGE_PROVENANCE" \
    --verified "$IMAGE_VERIFIED" >"$sso_log" 2>&1
  sso_rc=$?
  set -e
  if [[ "$sso_rc" -ne 0 ]]; then
    log "persistent SSO journey failed; sanitized driver log follows"
    coverage_journey_log_redacted <"$sso_log" >&2
    return 1
  fi
  coverage_journey_log_redacted <"$sso_log"
  local sso_record="$RAW/journeys/sso/provenance.json"
  if ! SSO_RECORD_SHA="$CANDIDATE_SHA" SSO_RECORD_IMAGE_ID="$IMAGE_ID" \
      python3 - "$sso_record" <<'PY'
import json
import os
import pathlib
import sys

record = json.loads(pathlib.Path(sys.argv[1]).read_text())
if record.get("complete") is not True or record.get("missing") is not False or record.get("killed") is not False:
    raise SystemExit("SSO record is incomplete")
if record.get("candidate_sha") != os.environ["SSO_RECORD_SHA"]:
    raise SystemExit("SSO record belongs to a different candidate")
if record.get("image_id") != os.environ["SSO_RECORD_IMAGE_ID"]:
    raise SystemExit("SSO record differs from the pinned candidate image")
generations = record.get("server_generations")
if not isinstance(generations, list) or len(generations) != 2:
    raise SystemExit("SSO record does not contain both server generations")
if [item.get("generation") for item in generations] != [1, 2]:
    raise SystemExit("SSO server generations are not ordered 1 then 2")
for item in generations:
    if item.get("image_id") != os.environ["SSO_RECORD_IMAGE_ID"]:
        raise SystemExit("SSO server generation used a different image")
    if item.get("flush_rc") != 0 or item.get("stop_rc") != 0 or item.get("exit_code") not in (0, 143) or item.get("oom_killed") is not False:
        raise SystemExit("SSO server generation did not flush and stop cleanly")
if not generations[0].get("container_id") or generations[0].get("container_id") == generations[1].get("container_id"):
    raise SystemExit("SSO server process identity did not change across restart")
for field in ("database_mount_sha256", "server_environment_sha256"):
    if not generations[0].get(field) or generations[0].get(field) != generations[1].get(field):
        raise SystemExit("SSO server restart changed its persistent database or auth configuration")
PY
  then
    coverage_journey_fail "persistent SSO provenance is missing or inconsistent"
    return 1
  fi
  if ! gocoverdir_complete "$RAW/journeys/sso/server"; then
    coverage_journey_fail "persistent SSO server generations did not produce coverage counters"
    return 1
  fi
  COVERAGE_JOURNEY_SERVER_DIRS+=("$RAW/journeys/sso/server")
  COVERAGE_JOURNEY_NAMES+=(sso)

  cli_raw="$RAW/cli-journeys-merged"
  server_raw="$RAW/server-journeys-merged"
  merge_gocoverdirs "$cli_raw" "$RAW/cli" "${COVERAGE_JOURNEY_CLI_DIRS[@]}" \
    || { coverage_journey_fail "could not merge complete CLI journey profiles"; return 1; }
  merge_gocoverdirs "$server_raw" "$RAW/server" "${COVERAGE_JOURNEY_SERVER_DIRS[@]}" \
    || { coverage_journey_fail "could not merge complete server journey profiles"; return 1; }
  rm -rf "$RAW/cli" "$RAW/server"
  mv "$cli_raw" "$RAW/cli"
  mv "$server_raw" "$RAW/server"

  JOURNEY_MANIFEST_SHA="$CANDIDATE_SHA" \
  JOURNEY_MANIFEST_IMAGE_ID="$IMAGE_ID" \
  JOURNEY_MANIFEST_BUILD_CONTEXT="$BUILD_CONTEXT" \
  JOURNEY_MANIFEST_PROVENANCE="$IMAGE_PROVENANCE" \
  JOURNEY_MANIFEST_VERIFIED="$IMAGE_VERIFIED" \
  python3 - "$RAW/journeys/manifest.json" "$RAW/journeys" <<'PY'
import json
import os
import pathlib
import sys

records = []
for lane in ("local", "auth", "distributed", "owner-memory", "sso"):
    record = json.loads((pathlib.Path(sys.argv[2]) / lane / "provenance.json").read_text())
    if record["complete"] is not True or record["candidate_sha"] != os.environ["JOURNEY_MANIFEST_SHA"]:
        raise SystemExit("incomplete or foreign lane record: " + lane)
    if record["image_id"] != os.environ["JOURNEY_MANIFEST_IMAGE_ID"]:
        raise SystemExit("lane image differs from pinned candidate: " + lane)
    records.append(record)
manifest = {
    "schema_version": 1,
    "kind": "real-integration-coverage-journeys",
    "candidate_sha": os.environ["JOURNEY_MANIFEST_SHA"],
    "image_id": os.environ["JOURNEY_MANIFEST_IMAGE_ID"],
    "build_context": json.loads(os.environ["JOURNEY_MANIFEST_BUILD_CONTEXT"]),
    "image_provenance": os.environ["JOURNEY_MANIFEST_PROVENANCE"],
    "verified": os.environ["JOURNEY_MANIFEST_VERIFIED"] == "true",
    "complete": len(records) == 5,
    "lanes": records,
    "merged_into": ["cli", "server"],
}
pathlib.Path(sys.argv[1]).write_text(json.dumps(manifest, indent=2) + "\n")
PY
  log "merged five exact-image real integration journeys into CLI/server coverage"
}
