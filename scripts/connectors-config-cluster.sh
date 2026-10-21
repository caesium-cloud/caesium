#!/usr/bin/env bash
# C1's shared-catalog proof uses the existing distributed Helm topology and
# test/robustness/cluster helpers. Every resource belongs to this invocation.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "$ROOT/scripts/helm-cluster-fixture.sh"
for command in docker just kind kubectl helm python3; do
  command -v "$command" >/dev/null || { echo "missing command: $command" >&2; exit 1; }
done
candidate="$(git rev-parse HEAD)"
[[ "${CANDIDATE_SHA:-$candidate}" == "$candidate" ]] || { echo 'candidate does not match HEAD' >&2; exit 1; }
[[ -z "$(git status --porcelain)" ]] || { echo 'cluster qualification requires a clean candidate' >&2; exit 1; }
tag="${CAESIUM_CONNECTORS_TAG:-connectors-cluster-${candidate:0:12}}"
cluster="${CAESIUM_CONNECTORS_CLUSTER_ID:-connectors-${candidate:0:8}-$(date +%s)-$$}"
[[ "$cluster" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#cluster} -le 47 ]] || { echo 'invalid isolated cluster name' >&2; exit 1; }
artifacts="${CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS:-$ROOT/.tmp/connectors-cluster/$cluster}"
[[ "$artifacts" == /* ]] || { echo 'artifact directory must be absolute' >&2; exit 1; }
python3 - "$artifacts" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
if p.is_symlink() or (p.exists() and (not p.is_dir() or any(p.iterdir()))):
    raise SystemExit('refusing nonempty or invalid artifact directory')
p.mkdir(parents=True, mode=0o700)
p.chmod(0o700)
PY
namespace="$cluster"
kubeconfig="$artifacts/kubeconfig"
owned=false
claim="${TMPDIR:-/tmp}/caesium-connectors-cluster-claims/$cluster"
claimed=false
kc() { kubectl --kubeconfig "$kubeconfig" "$@"; }
kn() { kc --namespace "$namespace" "$@"; }
collect() {
  [[ -f "$kubeconfig" ]] || return 0
  kn get pods -o wide >"$artifacts/pods.txt" 2>/dev/null || true
  kn get events --sort-by=.lastTimestamp >"$artifacts/events.txt" 2>/dev/null || true
  for pod in caesium-0 caesium-1 caesium-2 connector-fixture connector-config-test; do
    kn logs "$pod" --all-containers=true 2>/dev/null |
      sed -E 's/csk_[[:alnum:]_-]+/[REDACTED-API-KEY]/g' >"$artifacts/$pod.log" || true
  done
  kn exec connector-fixture -- cat /artifacts/events.jsonl >"$artifacts/events.jsonl" 2>/dev/null || true
  kn exec connector-fixture -- cat /artifacts/temporal-version.txt >"$artifacts/temporal-version.txt" 2>/dev/null || true
  kn exec connector-fixture -- cat /artifacts/server-version.txt >"$artifacts/server-version.txt" 2>/dev/null || true
  kn exec connector-config-test -- cat /artifacts/config-cluster-evidence.json >"$artifacts/config-cluster-evidence.json" 2>/dev/null || true
}
cleanup() {
  local result=$?
  trap - EXIT INT TERM
  set +e
  collect
  if [[ "$owned" == true ]]; then
    # A name is not ownership: preserve a node replaced outside this run.
    if python3 - "$artifacts/kind-node-ids.json" <<'PY'
import json, subprocess, sys
try:
    expected=json.load(open(sys.argv[1]))
    current={name:subprocess.check_output(["docker","inspect","--format","{{.Id}}",name],text=True).strip() for name in expected}
    if not expected or expected != current: raise ValueError("owned kind node identity changed")
except (ValueError,OSError,subprocess.CalledProcessError) as error:
    print("preserving cluster because ownership could not be verified: "+str(error),file=sys.stderr)
    raise SystemExit(1)
PY
    then
      if ! kind delete cluster --name "$cluster" >"$artifacts/cleanup.log" 2>&1; then result=1; fi
    else
      result=1
    fi
  fi
  if [[ "$claimed" == true ]]; then rmdir "$claim" || result=1; fi
  printf '%s\n' "$result" >"$artifacts/exit-status"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$(dirname "$claim")"
if ! mkdir "$claim" 2>/dev/null; then echo 'isolated cluster name already claimed' >&2; exit 1; fi
claimed=true
if kind get clusters 2>/dev/null | grep -qxF "$cluster"; then
  echo 'refusing an existing cluster' >&2; exit 1
fi
printf '%s\n' "$candidate" >"$artifacts/candidate-sha.txt"
git status --porcelain >"$artifacts/source-status.txt"
git ls-files -z | python3 -c '
import hashlib,json,sys
paths=sys.stdin.buffer.read().split(b"\0")
records=[]
from pathlib import Path
for raw in paths:
    if not raw: continue
    p=Path(raw.decode())
    data=str(p.readlink()).encode() if p.is_symlink() else p.read_bytes()
    records.append({"path":str(p),"sha256":hashlib.sha256(data).hexdigest()})
json.dump(records,sys.stdout,indent=2)
' >"$artifacts/source-checksums.json"
server_image="caesiumcloud/caesium:$tag-test"
fixture_image="caesiumcloud/caesium-connectors-fixture:$tag"
runner_image="${CAESIUM_INTEGRATION_RUNNER_IMAGE:-caesiumcloud/caesium-connectors-runner:$tag}"
case "${CAESIUM_CONNECTORS_REUSE_IMAGES:-false}" in
  true)
    verify_reuse_images() {
    python3 - "$candidate" "$artifacts/reuse-image-identities.json" "$server_image" "$runner_image" "$fixture_image" <<'PYREUSE'
import json, os, re, subprocess, sys
candidate, target, *refs = sys.argv[1:]
roles = ('server', 'runner', 'fixture')
result = {'candidate_sha': candidate}
for role, ref in zip(roles, refs):
    expected = os.environ.get('CAESIUM_CONNECTORS_REUSE_' + role.upper() + '_IMAGE_ID', '')
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', expected):
        raise SystemExit('image reuse requires first-phase immutable ' + role + ' identity')
    producer_name = {'server': 'CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID',
                     'runner': 'CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID'}.get(role)
    if producer_name and os.environ.get(producer_name) and os.environ[producer_name] != expected:
        raise SystemExit('image reuse differs from producer ' + role + ' identity')
    try:
        observed = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', ref],
                                           text=True, stderr=subprocess.PIPE, timeout=30).strip()
    except (OSError, subprocess.SubprocessError):
        raise SystemExit('image reuse cannot inspect ' + role + ' tag') from None
    if observed != expected:
        raise SystemExit('image reuse ' + role + ' tag differs from tested first-phase image')
    result[role] = {'tag': ref, 'expected_image_id': expected, 'observed_image_id': observed}
with open(target, 'w') as output: json.dump(result, output, indent=2)
PYREUSE
    }
    verify_reuse_images
    ;;
  false)
    # Independent manual qualification builds this clean candidate. The F2
    # lane can reuse the images its fixture just built or loaded for this SHA.
    CAESIUM_SKIP_IMAGE_BUILD=false just tag="$tag" builder-full build-test
    docker build --build-arg "BUILDER_IMAGE=caesiumcloud/caesium-builder:$tag-full" \
      -f build/Dockerfile.integration -t "$runner_image" .
    CAESIUM_CONNECTORS_TAG="$tag" CAESIUM_CONNECTORS_BUILDER_IMAGE="caesiumcloud/caesium-builder:$tag-full" \
      sh scripts/connectors-fixture.sh build
    ;;
  *) echo 'image reuse option must be true or false' >&2; exit 1 ;;
esac
kind_image="${CAESIUM_CONNECTORS_KIND_IMAGE:-kindest/node:v1.37.0}"
docker image inspect "$kind_image" >/dev/null 2>&1 || docker pull "$kind_image"
docker image inspect --format '{{.Id}}' "$server_image" >"$artifacts/server-image-id.txt"
candidate_digest="$(cat "$artifacts/server-image-id.txt")"
helm version --short >"$artifacts/helm-version.txt"
docker version --format '{{.Server.Version}}' >"$artifacts/docker-version.txt"
if kind get clusters 2>/dev/null | grep -qxF "$cluster"; then
  echo 'isolated cluster name appeared during builds; refusing it' >&2; exit 1
fi
if ! helm_fixture_create_cluster "$cluster" "$kind_image" "$kubeconfig"; then
  echo 'kind creation failed; no cluster deletion without confirmed ownership' >&2; exit 1
fi
kind get nodes --name "$cluster" >"$artifacts/kind-nodes.txt"
python3 - "$artifacts/kind-nodes.txt" >"$artifacts/kind-node-ids.json" <<'PY'
import json,subprocess,sys
names=open(sys.argv[1]).read().split()
if not names: raise SystemExit('created cluster has no nodes')
json.dump({name:subprocess.check_output(["docker","inspect","--format","{{.Id}}",name],text=True).strip() for name in names},sys.stdout)
PY
owned=true
load_image() {
  helm_fixture_load_image "$cluster" "$1" "$artifacts"
}
# Recheck after cluster startup, immediately before import; no changed tag can
# enter this topology under an earlier receipt.
if [[ "${CAESIUM_CONNECTORS_REUSE_IMAGES:-false}" == true ]]; then verify_reuse_images; fi
for ref in "$server_image" "$fixture_image" "$runner_image" busybox:1.36.1; do
  docker image inspect "$ref" >/dev/null 2>&1 || docker pull "$ref"
  load_image "$ref"
done
# Derive runtime identities from the loaded candidate before creating any app
# pods. CRI can report an import index whose digest is absent from the tagged
# manifest row, so first bind its status.id to our saved/imported seed IDs.
node="$(head -n1 "$artifacts/kind-nodes.txt")"
docker exec "$node" ctr -n k8s.io images ls >"$artifacts/imported-images.txt"
python3 test/robustness/hostlogic.py ctr-image-shas "$server_image" \
  <"$artifacts/imported-images.txt" >"$artifacts/imported-server-digests.txt"
python3 test/robustness/hostlogic.py collect-identities \
  "$artifacts/server-image-id.txt" "$artifacts/imported-server-digests.txt" >"$artifacts/candidate-seed-identities.txt"
python3 - "$node" "$server_image" "$artifacts" <<'PY'
import json, re, subprocess, sys
from pathlib import Path
node, image, directory = sys.argv[1:]
try:
    raw = subprocess.run(['docker', 'exec', node, 'crictl', 'inspecti', image], check=True,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30).stdout
    document = json.loads(raw)
except (OSError, ValueError, subprocess.SubprocessError):
    raise SystemExit('loaded candidate CRI inspection is unavailable or invalid') from None
status = document.get('status') if isinstance(document, dict) else None
if not isinstance(status, dict) or not isinstance(status.get('id'), str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', status['id']):
    raise SystemExit('loaded candidate CRI inspection lacks a valid status.id')
tags, digests = status.get('repoTags'), status.get('repoDigests')
if not isinstance(tags, list) or not all(isinstance(tag, str) for tag in tags):
    raise SystemExit('loaded candidate CRI inspection lacks valid repoTags')
canonical = image if image.startswith('docker.io/') else 'docker.io/' + image
if canonical not in tags:
    raise SystemExit('loaded candidate CRI inspection does not name the canonical candidate tag')
if not isinstance(digests, list) or not digests or not all(isinstance(digest, str) and re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}', digest) for digest in digests):
    raise SystemExit('loaded candidate CRI inspection lacks valid repoDigests')
artifacts = Path(directory)
# Persist only identity fields. Raw imageSpec, environment values and labels
# never become artifacts or inputs to identity extraction.
projected = {'status': {'id': status['id'], 'repoTags': tags, 'repoDigests': digests}}
(artifacts / 'loaded-candidate-cri.json').write_text(json.dumps(projected, indent=2) + '\n')
(artifacts / 'loaded-candidate-cri-id.txt').write_text(status['id'] + '\n')
PY
python3 test/robustness/hostlogic.py image-match "$artifacts/candidate-seed-identities.txt" \
  "$(cat "$artifacts/loaded-candidate-cri-id.txt")"
candidate_digest="$(python3 test/robustness/hostlogic.py collect-identities \
  "$artifacts/candidate-seed-identities.txt" "$artifacts/loaded-candidate-cri.json" | paste -sd, -)"
[[ "$candidate_digest" == sha256:* ]] || { echo 'loaded candidate identity missing' >&2; exit 1; }
printf '%s\n' "$candidate_digest" >"$artifacts/candidate-identities.txt"
kc create namespace "$namespace"
python3 - "$fixture_image" "$candidate" "$namespace" >"$artifacts/fixture.json" <<'PY'
import json,sys
image,sha,ns=sys.argv[1:]
env={"USER":"caesium-fixture","HOME":"/tmp","CAESIUM_CONNECTORS_ARTIFACTS":"/artifacts",
     "CAESIUM_CONNECTORS_CANDIDATE_SHA":sha,"CAESIUM_CONNECTORS_CAESIUM_URL":"http://caesium:8080",
     "CAESIUM_CONNECTORS_PROXY_LISTEN":"0.0.0.0:7234","CAESIUM_CONNECTORS_CONTROL_LISTEN":"0.0.0.0:7240",
     "CAESIUM_CONNECTORS_TLS_DNS":"connector-fixture,connector-fixture."+ns+".svc.cluster.local"}
pod={"apiVersion":"v1","kind":"Pod","metadata":{"name":"connector-fixture","labels":{"app":"connector-fixture"}},
     "spec":{"restartPolicy":"Never","containers":[{"name":"fixture","image":image,"imagePullPolicy":"Never",
     "env":[{"name":k,"value":v} for k,v in env.items()],"volumeMounts":[{"name":"artifacts","mountPath":"/artifacts"}]}],
     "volumes":[{"name":"artifacts","emptyDir":{}}]}}
service={"apiVersion":"v1","kind":"Service","metadata":{"name":"connector-fixture"},"spec":{"selector":{"app":"connector-fixture"},
         "ports":[{"name":"grpc","port":7234,"targetPort":7234},{"name":"control","port":7240,"targetPort":7240}]}}
json.dump({"apiVersion":"v1","kind":"List","items":[pod,service]},sys.stdout)
PY
kn apply -f "$artifacts/fixture.json"
kn wait --for=condition=Ready pod/connector-fixture --timeout=180s
ready=false
for ((i=0; i<120; i++)); do
  if kn exec connector-fixture -- curl -fsS http://127.0.0.1:7240/health >"$artifacts/fixture-health.json" 2>/dev/null; then ready=true; break; fi
  sleep 1
done
[[ "$ready" == true ]] || { echo 'real fixture is not ready' >&2; exit 1; }
mkdir -m700 "$artifacts/tls"
for file in ca.crt tls.crt tls.key; do
  kn exec connector-fixture -- cat "/artifacts/tls/$file" >"$artifacts/tls/$file"
  chmod 600 "$artifacts/tls/$file"
done
kn create secret generic connector-tls --from-file="$artifacts/tls"
cat >"$artifacts/connections.yaml" <<'YAML'
version: 1
connections:
  - id: fixture
    provider: temporal
    endpoint: connector-fixture:7234
    scope: default
    enabled: true
    credentials:
      certificatePaths: [/artifacts/tls/ca.crt, /artifacts/tls/tls.crt, /artifacts/tls/tls.key]
YAML
kn create configmap connectors-config \
  --from-file=caesium-0.yaml="$artifacts/connections.yaml" \
  --from-file=caesium-1.yaml="$artifacts/connections.yaml" \
  --from-file=caesium-2.yaml="$artifacts/connections.yaml"
# The Docker baseline policy path is preserved below, so mount its actual file.
kn create configmap connector-policy --from-file=two-namespaces.yaml=test/testdata/policy/two-namespaces.yaml
# Derive the environment from the tracked Kubernetes baseline instead of
# duplicating its feature flags. The same Helm chart supplies peer discovery.
helm template caesium helm/caesium --namespace "$namespace" --values helm/caesium/ci/test-values-k8s.yaml \
  --show-only templates/statefulset.yaml >"$artifacts/baseline.yaml"
kc create --dry-run=client --validate=false -f "$artifacts/baseline.yaml" -o json >"$artifacts/baseline.json"
python3 - "$artifacts/baseline.json" "$tag" >"$artifacts/values.json" <<'PY'
import json,re,sys
from pathlib import Path
base=json.load(open(sys.argv[1]))
native={"POD_NAME","POD_NAMESPACE","POD_IP","CAESIUM_LOG_LEVEL","CAESIUM_PORT","CAESIUM_NODE_ADDRESS",
        "CAESIUM_DATABASE_PATH","CAESIUM_DATABASE_TYPE","CAESIUM_KUBERNETES_NAMESPACE"}
env={v["name"]:v for v in base["spec"]["template"]["spec"]["containers"][0]["env"] if v["name"] not in native}
extra={"CAESIUM_EXECUTION_MODE":"distributed","CAESIUM_DATABASE_SHARDS":"1","CAESIUM_DATABASE_VOTERS":"3",
       "CAESIUM_DATABASE_STANDBYS":"0","CAESIUM_AUTH_MODE":"api-key","CAESIUM_AUTH_REQUIRE_TLS":"false",
       "CAESIUM_AUTH_KEY_HASH_SECRET":"connector-config-cluster-test-key-hash-secret-000001",
       "CAESIUM_CONNECTORS_ENABLED":"true","CAESIUM_CONNECTORS_CONFIG_FILE":"/connectors/$(POD_NAME).yaml"}
# Track the Docker baseline's CAESIUM feature envs, including future gates.
# Preserve chart-specific values (Kubernetes engine, distributed topology).
recipe = Path('justfile').read_text().split('integration-up:', 1)[1].split('\n\n', 1)[0]
for line in recipe.splitlines():
    match = re.match(r'\s*-e (CAESIUM_[A-Z0-9_]+)=(.*)', line)
    if not match:
        continue
    name, value = match.groups()
    if name in native:
        continue
    value = value.strip().removesuffix(chr(92)).strip()
    value = value.replace('{{ event_ingest_api_key }}', 'integration-test-key').replace('{{ contract_deprecation_window }}', '5s')
    if '{{' in value:
        raise SystemExit('unresolved baseline connector cluster env: ' + name)
    env.setdefault(name, {"name":name,"value":value})
env.update({k:{"name":k,"value":v} for k,v in extra.items()})
json.dump({"replicaCount":3,"image":{"repository":"caesiumcloud/caesium","tag":sys.argv[2]+"-test","pullPolicy":"Never"},
 "config":{"extraEnv":list(env.values())},"persistence":{"enabled":True},"resources":{"limits":{"cpu":"1000m","memory":"768Mi"}},
 "extraVolumes":[{"name":"connector-policy","configMap":{"name":"connector-policy"}},{"name":"connectors","configMap":{"name":"connectors-config"}},{"name":"connector-tls","secret":{"secretName":"connector-tls","defaultMode":288}}],
 "extraVolumeMounts":[{"name":"connector-policy","mountPath":"/etc/caesium/policy-fixture","readOnly":True},{"name":"connectors","mountPath":"/connectors","readOnly":True},{"name":"connector-tls","mountPath":"/artifacts/tls","readOnly":True}]},sys.stdout)
PY
helm_fixture_install_chart "$kubeconfig" "$namespace" caesium helm/caesium \
  --values helm/caesium/ci/test-values-k8s.yaml --values "$artifacts/values.json" --wait --timeout 480s
# OnDelete lets the scenario restart one exact member while other members keep
# their startup file. Prefix the actual chart command, preserving peer setup.
kn patch statefulset caesium --type=merge -p '{"spec":{"updateStrategy":{"type":"OnDelete","rollingUpdate":null}}}'
kn get statefulset caesium -o json >"$artifacts/statefulset.json"
python3 - "$artifacts/statefulset.json" >"$artifacts/command-patch.json" <<'PY'
import json,sys
obj=json.load(open(sys.argv[1]))
original=obj["spec"]["template"]["spec"]["containers"][0]["args"][0]
prefix='previous="/connectors/${POD_NAME}.previous"\nif [ -f "$previous" ]; then export CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT="$(cat "$previous")"; fi\n'
json.dump([{"op":"replace","path":"/spec/template/spec/containers/0/args/0","value":prefix+original}],sys.stdout)
PY
kn patch statefulset caesium --type=json --patch-file "$artifacts/command-patch.json"
admin_key="$(kn logs caesium-0 2>/dev/null | awk '/csk_/ && !found {for(i=1;i<=NF;i++) if($i ~ /^csk_/) {print $i;found=1;break}}' | tr -d '\r')"
[[ -n "$admin_key" ]] || { echo 'cluster bootstrap key missing' >&2; exit 1; }
printf '%s' "$admin_key" >"$artifacts/admin-key"
chmod 600 "$artifacts/admin-key"
kn create secret generic connector-test-auth --from-file=key="$artifacts/admin-key"
rm "$artifacts/admin-key"
unset admin_key
python3 - "$runner_image" "$candidate" "$candidate_digest" "$namespace" >"$artifacts/runner.json" <<'PY'
import json,sys
image,sha,digest,ns=sys.argv[1:]
sa={"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"connector-config-test"}}
role={"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"connector-config-test"},
 "rules":[{"apiGroups":[""],"resources":["pods"],"verbs":["get","list","watch","delete"]},
          {"apiGroups":[""],"resources":["configmaps"],"verbs":["get","patch","update"]},
          {"apiGroups":[""],"resources":["persistentvolumeclaims"],"verbs":["get","list","watch"]}]}
binding={"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"connector-config-test"},
 "subjects":[{"kind":"ServiceAccount","name":"connector-config-test","namespace":ns}],
 "roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"connector-config-test"}}
env={"POD_NAMESPACE":ns,"CAESIUM_CONNECTORS_CONFIG_CLUSTER":"true","CAESIUM_CONNECTORS_CLUSTER_CONFIGMAP":"connectors-config",
 "CAESIUM_CONNECTORS_FIXTURE_CONTROL_URL":"http://connector-fixture:7240","CAESIUM_CONNECTORS_ARTIFACTS":"/artifacts",
 "CAESIUM_CONNECTORS_CANDIDATE_SHA":sha,"CANDIDATE_SHA":sha,"CAESIUM_CONNECTORS_CLUSTER_CANDIDATE_DIGEST":digest}
pod={"apiVersion":"v1","kind":"Pod","metadata":{"name":"connector-config-test"},"spec":{"serviceAccountName":"connector-config-test",
 "restartPolicy":"Never","containers":[{"name":"test","image":image,"imagePullPolicy":"Never",
 "command":["/bin/sh","-c"],"args":["/usr/local/bin/caesium-integration.test -test.v -test.count=1 -test.timeout=20m -test.run '^TestConnectorsConfigCluster$' > /artifacts/test.log 2>&1; result=$?; cat /artifacts/test.log; echo $result >/artifacts/exit-status; sleep 600; exit $result"],
 "env":[*({"name":k,"value":v} for k,v in env.items()),{"name":"CAESIUM_AUTH_ADMIN_KEY","valueFrom":{"secretKeyRef":{"name":"connector-test-auth","key":"key"}}}],
 "volumeMounts":[{"name":"artifacts","mountPath":"/artifacts"}]}],"volumes":[{"name":"artifacts","emptyDir":{}}]}}
json.dump({"apiVersion":"v1","kind":"List","items":[sa,role,binding,pod]},sys.stdout)
PY
kn apply -f "$artifacts/runner.json"
finished=false
for ((i=0; i<1260; i++)); do
  if kn exec connector-config-test -- cat /artifacts/exit-status >"$artifacts/test-exit" 2>/dev/null; then finished=true; break; fi
  phase="$(kn get pod connector-config-test -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "$phase" != Failed && "$phase" != Succeeded ]] || break
  sleep 1
done
collect
[[ "$finished" == true && "$(cat "$artifacts/test-exit")" == 0 ]] || { echo 'cluster scenario failed or timed out' >&2; exit 1; }
[[ "$(grep -c '^--- PASS: TestConnectorsConfigCluster ' "$artifacts/connector-config-test.log" || true)" == 1 ]] || { echo 'cluster scenario must pass exactly once' >&2; exit 1; }
if grep -qE -- '--- (FAIL|SKIP): TestConnectorsConfigCluster([ /]|$)' "$artifacts/connector-config-test.log"; then
  echo 'selected cluster scenario skipped or failed' >&2; exit 1
fi
[[ "$(git rev-parse HEAD)" == "$candidate" && -z "$(git status --porcelain)" ]] || { echo 'candidate changed during qualification' >&2; exit 1; }
printf 'Connector shared-catalog evidence: %s\n' "$artifacts"
