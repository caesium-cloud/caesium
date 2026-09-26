#!/usr/bin/env bash
# F4 single-node previous-release upgrade qualification: host controller.
#
# Owns every image, volume, network and container the qualification touches and
# drives the integration-tagged runner in test/lifecycle/standalone_test.go once
# per phase. The runner never starts or inspects a container; this script writes
# each container fact it needs into the artifacts directory, so a fact that was
# never observed is reported BLOCKED rather than passed.
#
# Shape mirrors scripts/robustness.sh (B1): host controller + a Go runner that
# is compiled explicitly with -tags=integration inside the builder image,
# because the precompiled ./test binary does not contain subpackage tests.
#
#   CANDIDATE_SHA=$(git rev-parse HEAD)
#   CAESIUM_LIFECYCLE_ID="lifecycle-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
#   CAESIUM_LIFECYCLE_ARTIFACTS="$(mktemp -d)" \
#   CAESIUM_LIFECYCLE_PREV_IMAGE="caesiumcloud/caesium:v0.1.0" \
#   CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
#     bash scripts/lifecycle-tests.sh
#
# F2 cluster lane (requires the exclusive Docker/kind/Helm lane):
#   CANDIDATE_SHA=$(git rev-parse HEAD)
#   CAESIUM_LIFECYCLE_MODE=cluster \
#   CAESIUM_LIFECYCLE_ID="lifecycle-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
#   CAESIUM_LIFECYCLE_ARTIFACTS="$(mktemp -d)" \
#   CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
#   CAESIUM_LIFECYCLE_KIND_IMAGE=kindest/node:v1.33.1 \
#     bash scripts/lifecycle-tests.sh
#
# Leave the candidate image unbuilt: the harness builds it itself (`just
# tag="$CANDIDATE_SHA" build-release`) from this checkout. Cluster mode always
# blocks a pre-existing candidate tag so its archive can be bound to this run's
# clean commit. Standalone mode records a supplied image as unverified unless
# its explicit override is set (see candidate-image-provenance below).
#
# Every resource this script creates carries CAESIUM_LIFECYCLE_ID in its name,
# and teardown removes only those. It never touches a pre-existing container,
# volume or network, never publishes a host port, and never bind-mounts the
# dqlite data directory (a bind mount does not inherit the release image's
# 10001:10001 ownership; a fresh named volume does). Task containers the fixture
# jobs launch are matched by a per-invocation ownership token (see OWNER_TOKEN),
# never by a substring of the lifecycle id, and only once THIS invocation has
# established ownership of its named resources.
#
# The qualification record ($CAESIUM_LIFECYCLE_ARTIFACTS/qualification.json) says
# "pass" only when every expected case recorded a pass (or a recorded outcome)
# under THIS invocation's lifecycle id AND every phase returned 0. A run that
# aborts leaves the "incomplete" placeholder written at startup, never a stale
# pass from an earlier invocation.
#
# Optional:
#   CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1 (standalone mode only)
#       proceed (and record it) when the candidate image's provenance cannot be
#       established — a pre-existing/supplied image, or a build from a dirty
#       working tree. Without it such a run is BLOCKED, not qualified.
#   CAESIUM_LIFECYCLE_KEEP=1   leave owned resources in place for debugging.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lifecycle-snapshot-phase.sh
source "$ROOT/scripts/lifecycle-snapshot-phase.sh"
cd "$ROOT"

# F2's cluster lane is a separate mode. The F4 standalone path below is kept
# byte-for-byte in its existing control flow and still defaults when unset.
if [[ "${CAESIUM_LIFECYCLE_MODE:-standalone}" == "cluster" ]]; then
  cluster_die() {
    local reason="$*"
    if [[ -n "${LC_ART:-}" && -f "$LC_ART/cluster-qualification.json" ]] && command -v python3 >/dev/null 2>&1; then
      LC_ART="$LC_ART" LC_REASON="$reason" python3 - <<'PY' || true
import json,os,pathlib
p=pathlib.Path(os.environ['LC_ART'],'cluster-qualification.json')
record=json.loads(p.read_text())
if record.get('result')=='incomplete':
  record['result']='blocked'
  record['detail']=os.environ['LC_REASON']
  p.write_text(json.dumps(record,indent=2)+'\n')
PY
    fi
    printf 'cluster lifecycle: %s\n' "$reason" >&2
    exit 1
  }
  # Docker's image .Id can be a BuildKit index, while CRI reports the config
  # digest. Verify the complete single-platform archive before either identity
  # is allowed to identify a candidate pod. The expected platform manifest is
  # obtained from the exact tag built from the clean checkout by this run.
  lc_verify_candidate_archive() {
    local archive="$1" tag="$2" platform="$3" manifest_digest="$4" source_id="$5" proof="$6" sha="$7"
    LC_ARCHIVE="$archive" LC_ARCHIVE_TAG="$tag" LC_ARCHIVE_PLATFORM="$platform" \
      LC_ARCHIVE_MANIFEST="$manifest_digest" LC_ARCHIVE_SOURCE_ID="$source_id" \
      LC_ARCHIVE_PROOF="$proof" LC_ARCHIVE_SHA="$sha" python3 - <<'PY'
import hashlib,json,os,pathlib,re,tarfile
archive=pathlib.Path(os.environ['LC_ARCHIVE'])
tag=os.environ['LC_ARCHIVE_TAG'];platform=os.environ['LC_ARCHIVE_PLATFORM']
expected_manifest=os.environ['LC_ARCHIVE_MANIFEST'];source_id=os.environ['LC_ARCHIVE_SOURCE_ID']
proof=pathlib.Path(os.environ['LC_ARCHIVE_PROOF']);sha=os.environ['LC_ARCHIVE_SHA']
def require(ok,detail):
  if not ok:raise SystemExit(detail)
def valid_digest(value):
  return isinstance(value,str) and re.fullmatch(r'sha256:[0-9a-f]{64}',value) is not None
require(re.fullmatch(r'[0-9a-f]{40}',sha) is not None,'candidate commit is not a full SHA')
require(tag=='caesiumcloud/caesium:'+sha,'candidate archive tag differs from checkout SHA')
require(valid_digest(source_id) and valid_digest(expected_manifest),'Docker image identities are malformed')
require(platform in ('linux/amd64','linux/arm64'),'unsupported candidate platform')
target_os,target_arch=platform.split('/',1)
def read_blob(tar,digest):
  require(valid_digest(digest),f'invalid blob digest {digest!r}')
  path='blobs/sha256/'+digest.split(':',1)[1]
  member=tar.getmember(path)
  require(member.isfile(),f'blob {digest} is not a regular file')
  data=tar.extractfile(member).read()
  require(hashlib.sha256(data).hexdigest()==digest.split(':',1)[1],f'blob {digest} hash mismatch')
  return data
with tarfile.open(archive) as tar:
  members=tar.getmembers()
  require(len(members)==len({m.name for m in members}),'candidate archive has duplicate paths')
  manifest=json.load(tar.extractfile('manifest.json'))
  index_bytes=tar.extractfile('index.json').read()
  index=json.loads(index_bytes)
  require(len(manifest)==1 and manifest[0].get('RepoTags')==[tag],
    f'candidate archive must contain exactly tag {tag}')
  require(index.get('schemaVersion')==2 and len(index.get('manifests',[]))==1,
    'candidate archive must contain one platform manifest')
  descriptor=index['manifests'][0]
  require(descriptor.get('platform',{}).get('os')==target_os and
    descriptor.get('platform',{}).get('architecture')==target_arch,
    f'candidate archive platform differs from {platform}')
  require(descriptor.get('digest')==expected_manifest,
    f'candidate archive manifest differs from Docker platform identity {expected_manifest}')
  manifest_bytes=read_blob(tar,expected_manifest)
  require(descriptor.get('size')==len(manifest_bytes),'candidate platform manifest size mismatch')
  image_manifest=json.loads(manifest_bytes)
  require(image_manifest.get('schemaVersion')==2,'invalid candidate platform manifest')
  config_descriptor=image_manifest.get('config',{})
  config_digest=config_descriptor.get('digest','')
  require(valid_digest(config_digest),'candidate config digest missing or malformed')
  require(manifest[0].get('Config')=='blobs/sha256/'+config_digest.split(':',1)[1],
    'candidate Docker manifest Config differs from platform manifest')
  config_bytes=read_blob(tar,config_digest)
  require(config_descriptor.get('size')==len(config_bytes),'candidate config size mismatch')
  config=json.loads(config_bytes)
  require(config.get('os')==target_os and config.get('architecture')==target_arch,
    f'candidate config platform differs from {platform}')
  layers=image_manifest.get('layers',[])
  require(bool(layers),'candidate platform manifest has no layers')
  paths=[]
  for layer in layers:
    digest=layer.get('digest','')
    data=read_blob(tar,digest)
    require(layer.get('size')==len(data),f'candidate layer {digest} size mismatch')
    paths.append('blobs/sha256/'+digest.split(':',1)[1])
  require(manifest[0].get('Layers')==paths,
    'candidate Docker manifest layers differ from platform manifest')
  archive_index='sha256:'+hashlib.sha256(index_bytes).hexdigest()
  record={'archive_ref':archive.name,'repo_tag':tag,'candidate_sha':sha,'platform':platform,
    'source_docker_image_id':source_id,'source_platform_manifest_digest':expected_manifest,
    'archive_sha256':hashlib.sha256(archive.read_bytes()).hexdigest(),
    'archive_index_digest':archive_index,'verified_platform_manifest_digest':expected_manifest,
    'verified_config_digest':config_digest,
    'verified_layer_digests':[layer['digest'] for layer in layers],
    'verification':'built tag, platform manifest, config and all layer hashes matched'}
  proof.write_text(json.dumps(record,indent=2)+'\n')
  print(config_digest)
PY
  }
  lc_verify_candidate_imports() {
    local archive="$1" proof="$2" node_list="$3" log_dir="$4" output="$5" tag="$6"
    LC_ARCHIVE="$archive" LC_ARCHIVE_PROOF="$proof" LC_ARCHIVE_NODES="$node_list" \
      LC_ARCHIVE_LOG_DIR="$log_dir" LC_ARCHIVE_IMPORT_PROOF="$output" LC_ARCHIVE_TAG="$tag" python3 - <<'PY'
import hashlib,json,os,pathlib,re
archive=pathlib.Path(os.environ['LC_ARCHIVE']);proof_path=pathlib.Path(os.environ['LC_ARCHIVE_PROOF'])
proof=json.loads(proof_path.read_text());tag=os.environ['LC_ARCHIVE_TAG']
if proof['repo_tag']!=tag or hashlib.sha256(archive.read_bytes()).hexdigest()!=proof['archive_sha256']:
  raise SystemExit('candidate archive changed or tag differs after verification')
expected={proof['archive_index_digest'],proof['verified_platform_manifest_digest']}
nodes=pathlib.Path(os.environ['LC_ARCHIVE_NODES']).read_text().splitlines()
if len(nodes)!=4 or len(set(nodes))!=4 or any(not n for n in nodes):
  raise SystemExit(f'candidate import expects four distinct owned nodes, got {nodes!r}')
refs={tag,'docker.io/'+tag};observed=[]
for node in nodes:
  path=pathlib.Path(os.environ['LC_ARCHIVE_LOG_DIR'])/f'candidate-import-{node}.txt'
  lines=path.read_text().splitlines()
  rows=[line.split() for line in lines if line.split() and line.split()[0] in refs]
  if not rows:raise SystemExit(f'{node}: no imported candidate tag {tag}')
  for row in rows:
    digests=[field for field in row[1:] if re.fullmatch(r'sha256:[0-9a-f]{64}',field)]
    if len(digests)!=1 or digests[0] not in expected:
      raise SystemExit(f'{node}: candidate tag target {digests!r} differs from verified archive {sorted(expected)}')
    observed.append({'node':node,'repo_tag':row[0],'imported_target_digest':digests[0]})
pathlib.Path(os.environ['LC_ARCHIVE_IMPORT_PROOF']).write_text(json.dumps({
  'archive_proof':proof_path.name,'node_imports':observed},indent=2)+'\n')
PY
  }
  # Host-only controls exercise the exact Python verifiers used by the live
  # cluster path. They need neither Docker nor a Kubernetes context.
  if [[ "${CAESIUM_LIFECYCLE_ARCHIVE_VERIFY_ONLY:-0}" == 1 ]]; then
    lc_verify_candidate_archive "$LC_TEST_ARCHIVE" "$LC_TEST_TAG" "$LC_TEST_PLATFORM" \
      "$LC_TEST_MANIFEST" "$LC_TEST_SOURCE" "$LC_TEST_PROOF" "$LC_TEST_SHA"
    exit
  fi
  if [[ "${CAESIUM_LIFECYCLE_IMPORT_VERIFY_ONLY:-0}" == 1 ]]; then
    lc_verify_candidate_imports "$LC_TEST_ARCHIVE" "$LC_TEST_PROOF" "$LC_TEST_NODES" \
      "$LC_TEST_LOG_DIR" "$LC_TEST_IMPORT_PROOF" "$LC_TEST_TAG"
    exit
  fi
  if [[ "${CAESIUM_LIFECYCLE_ARCHIVE_SELFTEST:-0}" == 1 ]]; then
    python3 - "$ROOT/scripts/lifecycle-tests.sh" <<'PY'
import hashlib,io,json,os,pathlib,shutil,subprocess,sys,tarfile,tempfile
script=sys.argv[1];sha='a'*40;tag='caesiumcloud/caesium:'+sha
source='sha256:'+'b'*64;platform='linux/arm64'
def encoded(value):return json.dumps(value,separators=(',',':')).encode()
def digest(data):return 'sha256:'+hashlib.sha256(data).hexdigest()
config=encoded({'os':'linux','architecture':'arm64'})
layer=b'candidate-layer-negative-control'
config_id=digest(config);layer_id=digest(layer)
manifest=encoded({'schemaVersion':2,'config':{'digest':config_id,'size':len(config)},
  'layers':[{'digest':layer_id,'size':len(layer)}]})
manifest_id=digest(manifest)
index=encoded({'schemaVersion':2,'manifests':[{'digest':manifest_id,'size':len(manifest),
  'platform':{'os':'linux','architecture':'arm64'}}]})
docker_manifest=encoded([{'Config':'blobs/sha256/'+config_id[7:],'RepoTags':[tag],
  'Layers':['blobs/sha256/'+layer_id[7:]]}])
blobs={'manifest.json':docker_manifest,'index.json':index,
  'blobs/sha256/'+config_id[7:]:config,'blobs/sha256/'+layer_id[7:]:layer,
  'blobs/sha256/'+manifest_id[7:]:manifest}
def archive(path,contents):
  with tarfile.open(path,'w') as tar:
    for name,data in contents.items():
      info=tarfile.TarInfo(name);info.size=len(data)
      tar.addfile(info,io.BytesIO(data))
def run(mode,path,proof,nodes=None,logs=None,import_proof=None,expected=manifest_id):
  env=os.environ.copy();env.pop('CAESIUM_LIFECYCLE_ARCHIVE_SELFTEST',None)
  env.update({'CAESIUM_LIFECYCLE_MODE':'cluster',mode:'1','LC_TEST_ARCHIVE':str(path),
    'LC_TEST_TAG':tag,'LC_TEST_PLATFORM':platform,'LC_TEST_MANIFEST':expected,
    'LC_TEST_SOURCE':source,'LC_TEST_PROOF':str(proof),'LC_TEST_SHA':sha,
    'LC_TEST_NODES':str(nodes or ''),'LC_TEST_LOG_DIR':str(logs or ''),
    'LC_TEST_IMPORT_PROOF':str(import_proof or '')})
  return subprocess.run(['bash',script],env=env,text=True,capture_output=True)
def expect_reject(name,result):
  if result.returncode==0:raise SystemExit(f'{name} unexpectedly passed')
with tempfile.TemporaryDirectory(prefix='caesium-f2-image-selftest-') as tmp:
  root=pathlib.Path(tmp);good=root/'candidate.tar';proof=root/'candidate.json'
  archive(good,blobs)
  result=run('CAESIUM_LIFECYCLE_ARCHIVE_VERIFY_ONLY',good,proof)
  if result.returncode or result.stdout.strip()!=config_id:
    raise SystemExit(f'valid candidate archive rejected: {result.stderr}')
  if json.loads(proof.read_text())['verified_config_digest']!=config_id:
    raise SystemExit('valid archive proof omitted config identity')
  variants={
    'wrong tag':{'manifest.json':encoded([{'Config':'blobs/sha256/'+config_id[7:],
      'RepoTags':['caesiumcloud/caesium:wrong'],'Layers':['blobs/sha256/'+layer_id[7:]]}])},
    'wrong platform':{'index.json':encoded({'schemaVersion':2,'manifests':[
      {'digest':manifest_id,'size':len(manifest),'platform':{'os':'linux','architecture':'amd64'}}]})},
    'corrupt config':{'blobs/sha256/'+config_id[7:]:config+b'x'},
    'corrupt layer':{'blobs/sha256/'+layer_id[7:]:layer+b'x'},
    'wrong layer list':{'manifest.json':encoded([{'Config':'blobs/sha256/'+config_id[7:],
      'RepoTags':[tag],'Layers':[]}])},
  }
  for name,changes in variants.items():
    bad=root/(name.replace(' ','-')+'.tar');archive(bad,{**blobs,**changes})
    expect_reject(name,run('CAESIUM_LIFECYCLE_ARCHIVE_VERIFY_ONLY',bad,root/(bad.stem+'.json')))
  expect_reject('wrong Docker platform identity',run('CAESIUM_LIFECYCLE_ARCHIVE_VERIFY_ONLY',
    good,root/'wrong-manifest.json',expected='sha256:'+'c'*64))
  logs=root/'logs';logs.mkdir();names=[f'owned-node-{n}' for n in range(4)]
  nodes=root/'nodes.txt';nodes.write_text('\n'.join(names)+'\n')
  imported='sha256:'+hashlib.sha256(index).hexdigest()
  for node in names:
    (logs/f'candidate-import-{node}.txt').write_text(f'REF TYPE DIGEST SIZE PLATFORMS LABELS\n'
      f'docker.io/{tag} application/vnd.oci.image.index.v1+json {imported} 100 linux/arm64 -\n')
  import_proof=root/'imports.json'
  result=run('CAESIUM_LIFECYCLE_IMPORT_VERIFY_ONLY',good,proof,nodes,logs,import_proof)
  if result.returncode or len(json.loads(import_proof.read_text())['node_imports'])!=4:
    raise SystemExit(f'valid four-node import rejected: {result.stderr}')
  bad_log=logs/f'candidate-import-{names[-1]}.txt'
  good_log=bad_log.read_text()
  bad_log.write_text(good_log.replace(imported,'sha256:'+'d'*64))
  expect_reject('foreign node target',run('CAESIUM_LIFECYCLE_IMPORT_VERIFY_ONLY',
    good,proof,nodes,logs,import_proof))
  bad_log.write_text('REF TYPE DIGEST SIZE PLATFORMS LABELS\n')
  expect_reject('missing node import',run('CAESIUM_LIFECYCLE_IMPORT_VERIFY_ONLY',
    good,proof,nodes,logs,import_proof))
  bad_log.write_text(good_log)
  changed=root/'changed.tar';shutil.copyfile(good,changed)
  with changed.open('ab') as out:out.write(b'changed-after-verification')
  expect_reject('archive changed after verification',run('CAESIUM_LIFECYCLE_IMPORT_VERIFY_ONLY',
    changed,proof,nodes,logs,import_proof))
print('candidate archive selftest: valid archive/import passed; 9 negative controls rejected')
PY
    exit
  fi
  for cmd in docker kind kubectl helm python3 just; do
    command -v "$cmd" >/dev/null 2>&1 || cluster_die "missing $cmd"
  done
  : "${CAESIUM_LIFECYCLE_ID:?set a unique DNS-1123 cluster name}"
  : "${CAESIUM_LIFECYCLE_ARTIFACTS:?set an artifact directory}"
  : "${CAESIUM_LIFECYCLE_CANDIDATE_IMAGE:?set caesiumcloud/caesium:<candidate SHA>}"
  LC_ID="$CAESIUM_LIFECYCLE_ID"
  LC_SHA="${CANDIDATE_SHA:-${CAESIUM_LIFECYCLE_CANDIDATE_IMAGE##*:}}"
  [[ "$LC_ID" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]] || cluster_die "invalid lifecycle id $LC_ID"
  [[ "$LC_SHA" =~ ^[0-9a-f]{40}$ ]] || cluster_die "candidate tag must name a full git SHA"
  LC_PREV="${CAESIUM_LIFECYCLE_PREV_IMAGE:-caesiumcloud/caesium:v0.1.0}"
  [[ "$LC_PREV" == "caesiumcloud/caesium:v0.1.0" ]] || cluster_die "previous release must be pinned v0.1.0"
  [[ "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" == "caesiumcloud/caesium:$LC_SHA" ]] || cluster_die "candidate image/tag and SHA disagree"
  LC_ART="$CAESIUM_LIFECYCLE_ARTIFACTS"
  mkdir -p "$LC_ART"
  LC_ART="$(cd "$LC_ART" && pwd)"
  case "$LC_ART/" in "$ROOT/"*) cluster_die "cluster artifacts must be outside the candidate checkout" ;; esac
  LC_KUBE="$LC_ART/kubeconfig"
  LC_VALUES="$ROOT/helm/caesium/ci/test-values-lifecycle.yaml"
  LC_TASK="${CAESIUM_LIFECYCLE_TASK_IMAGE:-alpine:3.23}"
  LC_KIND="${CAESIUM_LIFECYCLE_KIND_IMAGE:-kindest/node:v1.33.1}"
  LC_RUNNER="caesium-lifecycle-runner:$LC_ID"
  LC_OWNED=0
  LC_STARTED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  LC_PAIR="${CAESIUM_LIFECYCLE_PAIR:-v0.1.0-to-candidate}"
  lc_require_absent_cluster() {
    local clusters nodes
    clusters="$(kind get clusters 2>&1)" || cluster_die "kind get clusters failed: $clusters"
    if printf '%s\n' "$clusters" | grep -qxF "$LC_ID"; then
      cluster_die "kind cluster $LC_ID already exists; refusing to claim or delete it"
    fi
    nodes="$(docker ps -a --format '{{.Names}}' 2>&1)" || cluster_die "docker ps failed: $nodes"
    if printf '%s\n' "$nodes" | grep -qxF "${LC_ID}-control-plane"; then
      cluster_die "docker container ${LC_ID}-control-plane already exists; refusing to claim cluster $LC_ID"
    fi
  }
  [[ ! -e "$LC_KUBE" ]] || cluster_die "$LC_KUBE exists; refusing to adopt another cluster's kubeconfig"
  lc_require_absent_cluster
  # Artifacts may be reused deliberately; no prior case or observation can
  # count toward this invocation's complete expected-case manifest.
  rm -rf "$LC_ART/cases" "$LC_ART/cluster-cases" "$LC_ART/cluster-logs"
  rm -f "$LC_ART/cluster-qualification.json" "$LC_ART/cluster-fixture.json" \
    "$LC_ART/cluster-mixed-window.json" "$LC_ART/cluster-mixed-crossing.json" \
    "$LC_ART/cluster-host-observation.json" "$LC_ART/cluster-ordinal0-host.json" \
    "$LC_ART/cluster-post-storage.json" "$LC_ART/cluster-raw-before.json" \
    "$LC_ART/cluster-raw-after.json" "$LC_ART/cluster-attempt-proofs.json" \
    "$LC_ART/cluster-snapshot-leader.json" "$LC_ART/cluster-address-classification.json" \
    "$LC_ART/cluster-snapshot-write-count.json" \
    "$LC_ART/candidate-platform.tar" "$LC_ART/candidate-image-archive.json" \
    "$LC_ART/candidate-image-node-imports.json" \
    "$LC_ART/manifest-normalized.diff" "$LC_ART/manifest-live-normalized.diff"
  rm -f "$LC_ART"/cluster-snapshot-update-batch-*.json
  rm -f "$LC_ART"/cluster-snapshot-disputed-batch-*.json
  mkdir -p "$LC_ART/cases" "$LC_ART/cluster-cases" "$LC_ART/cluster-logs"
  cp "$ROOT/test/lifecycle/versions.json" "$LC_ART/versions.json"
  LC_ID="$LC_ID" LC_SHA="$LC_SHA" LC_PAIR="$LC_PAIR" LC_STARTED="$LC_STARTED" LC_ART="$LC_ART" python3 - <<'PY'
import json,os,pathlib
pathlib.Path(os.environ['LC_ART'],'cluster-qualification.json').write_text(json.dumps({
  'kind':'caesium-cluster-lifecycle-qualification','lifecycle_id':os.environ['LC_ID'],
  'candidate_sha':os.environ['LC_SHA'],'pair':os.environ['LC_PAIR'],
  'started_at':os.environ['LC_STARTED'],'result':'incomplete',
  'detail':'host controller has not completed all required F2 cases'},indent=2)+'\n')
PY
  lc_ns() { kubectl --kubeconfig "$LC_KUBE" --namespace "$LC_ID" "$@"; }
  LC_SIGNALLED=0
  lc_cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    set +e
    if [[ -n "${LC_PHASE_PID:-}" ]]; then
      lc_stop_phase_group "$LC_PHASE_PID"
      LC_PHASE_PID=""
    fi
    if [[ "$LC_SIGNALLED" == 1 ]]; then rc=143; fi
    if [[ "$LC_OWNED" == 1 ]]; then
      lc_ns logs pod/lifecycle-runner -c recorder >"$LC_ART/cluster-logs/recorder.log" 2>&1 || true
      lc_ns get pods -o wide >"$LC_ART/cluster-logs/pods-final.txt" 2>&1 || true
      lc_ns --request-timeout=10s get events --sort-by=.metadata.creationTimestamp -o wide \
        >"$LC_ART/cluster-logs/events-final.txt" 2>&1 || true
      for n in 0 1 2; do
        lc_ns --request-timeout=10s logs "caesium-$n" -c caesium --tail=2000 --timestamps=true \
          >"$LC_ART/cluster-logs/caesium-$n-current.log" 2>&1 || true
        lc_ns --request-timeout=10s logs "caesium-$n" -c caesium --previous \
          >"$LC_ART/cluster-logs/caesium-$n-previous.log" 2>&1 || true
      done
      if [[ "${CAESIUM_LIFECYCLE_KEEP:-0}" != 1 ]]; then
        if ! kind delete cluster --name "$LC_ID" >"$LC_ART/cluster-logs/kind-delete.log" 2>&1; then
          rc=1
          LC_ART="$LC_ART" python3 - <<'PY' || true
import json,os,pathlib
p=pathlib.Path(os.environ['LC_ART'],'cluster-qualification.json')
if p.exists():
  record=json.loads(p.read_text())
  record['result']='fail'
  record.setdefault('failed_gates',[]).append('owned_cluster_cleanup')
  record['cleanup_detail']='kind delete cluster failed; see cluster-logs/kind-delete.log'
  p.write_text(json.dumps(record,indent=2)+'\n')
PY
          printf 'cluster lifecycle: owned kind cluster %s could not be deleted; see %s\n' "$LC_ID" "$LC_ART/cluster-logs/kind-delete.log" >&2
        fi
      fi
    fi
    # An interrupted or otherwise incomplete qualification cannot signal
    # success merely because the last foreground command exited zero.
    if [[ "$rc" == 0 && -f "$LC_ART/cluster-qualification.json" ]]; then
      local outcome
      outcome="$(LC_ART="$LC_ART" python3 - <<'PY'
import json,os,pathlib
print(json.loads(pathlib.Path(os.environ['LC_ART'],'cluster-qualification.json').read_text()).get('result','incomplete'))
PY
)" || rc=1
      [[ "$outcome" == pass ]] || rc=1
    fi
    exit "$rc"
  }
  trap lc_cleanup EXIT
  trap 'LC_SIGNALLED=1; exit 130' INT
  trap 'LC_SIGNALLED=1; lc_stop_phase_group "${LC_PHASE_PID:-}"; exit 143' TERM
  lc_case() {
    LC_CASE="$1" LC_STATUS="$2" LC_DETAIL="$3" LC_EVIDENCE="${4:-}" LC_ART="$LC_ART" LC_ID="$LC_ID" python3 - <<'PY'
import json,os,pathlib,re
name=os.environ['LC_CASE']
path=pathlib.Path(os.environ['LC_ART'],'cluster-cases',re.sub('[^A-Za-z0-9_-]','-',name)+'.json')
rec={'name':name,'status':os.environ['LC_STATUS'],
  'detail':os.environ['LC_DETAIL'],'lifecycle_id':os.environ['LC_ID']}
if os.environ['LC_EVIDENCE']:
  rec['observations']=json.loads(pathlib.Path(os.environ['LC_EVIDENCE']).read_text())
path.write_text(json.dumps(rec,indent=2)+'\n')
PY
  }
  lc_phase() {
    local phase="$1" image_id="$2" base="$3" log="$1"
    if [[ -n "${LC_SNAPSHOT_BATCH:-}" ]]; then log="$phase-$LC_SNAPSHOT_BATCH"; fi
    lc_ns exec pod/lifecycle-runner -c runner -- env \
      CAESIUM_LIFECYCLE_ID="$LC_ID" CAESIUM_LIFECYCLE_PAIR="$LC_PAIR" \
      CAESIUM_LIFECYCLE_ARTIFACTS=/artifacts \
      CAESIUM_LIFECYCLE_BASE_URL="$base" \
      CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID="$image_id" \
      CAESIUM_LIFECYCLE_PREVIOUS_IMAGE_ID="$LC_PREV_IDS" \
      CAESIUM_LIFECYCLE_CANDIDATE_IMAGE_ID="$LC_CAND_ID" \
      CAESIUM_LIFECYCLE_TASK_IMAGE="$LC_TASK" \
      CAESIUM_LIFECYCLE_INTERNAL_TOKEN=caesium-lifecycle-internal-token-not-for-production-use \
      CAESIUM_MANUAL_TRIGGER_API_KEY=caesium-lifecycle-manual-key \
      CAESIUM_LIFECYCLE_PHASE="$phase" \
      CAESIUM_LIFECYCLE_SNAPSHOT_BATCH="${LC_SNAPSHOT_BATCH:-}" \
      /lifecycle.test -test.v -test.count=1 -test.run "^TestLifecycleCluster${phase}$" -test.timeout=12m \
      >"$LC_ART/cluster-logs/$log.log" 2>&1
  }
  lc_info() {
    local pod="$1" file="$2"
    lc_ns exec "$pod" -c caesium -- cat /var/lib/caesium/dqlite/info.yaml >"$file"
  }
  lc_base() {
    local ip
    ip="$(lc_ns get pod caesium-0 -o jsonpath='{.status.podIP}')"
    [[ -n "$ip" ]] || cluster_die "caesium-0 has no pod IP"
    printf 'http://%s:8080' "$ip"
  }
  lc_copy_runner_artifacts() {
    lc_ns cp -c runner lifecycle-runner:/artifacts/. "$LC_ART/" >/dev/null
  }
  lc_collect_info() {
    local stage="$1" n
    for n in 0 1 2; do
      lc_info "caesium-$n" "$LC_ART/cluster-logs/$stage-info-$n.yaml" || return 1
      lc_ns get pod "caesium-$n" -o jsonpath='{.status.podIP}' \
        >"$LC_ART/cluster-logs/$stage-ip-$n.txt" || return 1
    done
    LC_ART="$LC_ART" LC_STAGE="$stage" python3 - <<'PY'
import json,os,pathlib,re
art=pathlib.Path(os.environ['LC_ART']);stage=os.environ['LC_STAGE'];out=[]
for n in range(3):
  raw=(art/'cluster-logs'/f'{stage}-info-{n}.yaml').read_text()
  ip=(art/'cluster-logs'/f'{stage}-ip-{n}.txt').read_text().strip()
  vals={k:v for k,v in re.findall(r'(?m)^\s*(ID|Address):\s*[\'\"]?([^\'\"\s]+)',raw)}
  if not all(k in vals for k in ('ID','Address')):raise SystemExit(f'info.yaml for caesium-{n} lacks ID/Address')
  if not ip:raise SystemExit(f'caesium-{n} has no pod IP')
  out.append({'name':f'caesium-{n}','id':int(vals['ID']),'address':vals['Address'],'pod_ip':ip})
  if stage=='before' and vals['Address']!=f'{ip}:9001':
    raise SystemExit(f'previous caesium-{n} info.yaml address {vals["Address"]} differs from pod IP {ip}')
(art/f'{stage}-info.json').write_text(json.dumps(out,indent=2)+'\n')
PY
  }

  [[ "$(git rev-parse HEAD)" == "$LC_SHA" ]] || cluster_die "candidate SHA differs from this checkout HEAD"
  [[ -z "$(git status --porcelain)" ]] || cluster_die "refusing image build from dirty checkout"
  docker pull "$LC_PREV" >"$LC_ART/cluster-logs/pull-previous.log" 2>&1 || cluster_die "pull previous release failed"
  LC_PREV_ID="$(docker image inspect --format '{{.Id}}' "$LC_PREV")"
  LC_PREV_DIGESTS="$(docker image inspect --format '{{join .RepoDigests ","}}' "$LC_PREV")"
  LC_ARCH="$(docker image inspect --format '{{.Architecture}}' "$LC_PREV")"
  case "$LC_ARCH" in
    arm64|amd64) ;;
    aarch64) LC_ARCH=arm64 ;;
    *) cluster_die "unsupported previous-release platform architecture $LC_ARCH" ;;
  esac
  LC_PLATFORM="linux/$LC_ARCH"
  LC_PREV_INSPECT_PLATFORM_ID="$(docker image inspect --platform "$LC_PLATFORM" --format '{{.Id}}' "$LC_PREV")"
  LC_PREV_ID="$LC_PREV_ID" LC_PREV_DIGESTS="$LC_PREV_DIGESTS" LC_ART="$LC_ART" LC_PAIR="$LC_PAIR" python3 - <<'PY' || cluster_die 'previous release digest does not match versions.json'
import json,os,pathlib
doc=json.loads(pathlib.Path(os.environ['LC_ART'],'versions.json').read_text())
p=next(x for x in doc['pairs'] if x['id']==os.environ['LC_PAIR'])
want=set(p['previous']['digests'].values())
got={x.split('@',1)[1] for x in os.environ['LC_PREV_DIGESTS'].split(',') if '@' in x}
if not want.intersection(got):raise SystemExit(f'old image has {got}, expected one of {want}')
print('pinned previous digest:', sorted(want.intersection(got)))
PY
  LC_PREV_PLATFORM_DIGEST="$(LC_ART="$LC_ART" LC_PAIR="$LC_PAIR" LC_PLATFORM="$LC_PLATFORM" python3 - <<'PY'
import json,os,pathlib
doc=json.loads(pathlib.Path(os.environ['LC_ART'],'versions.json').read_text())
p=next(x for x in doc['pairs'] if x['id']==os.environ['LC_PAIR'])
print(p['previous']['digests'][os.environ['LC_PLATFORM']])
PY
)"
  # A pre-existing tag cannot establish which checkout produced its bytes.
  # Cluster qualification therefore always builds this exact tag itself.
  if docker image inspect "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" >/dev/null 2>&1; then
    cluster_die "candidate image pre-exists; remove the tag so this clean checkout can build it"
  fi
  just "tag=$LC_SHA" build-release >"$LC_ART/cluster-logs/build-candidate.log" 2>&1 || cluster_die "candidate build failed"
  LC_PROVENANCE=built-by-this-run
  LC_CAND_SOURCE_ID="$(docker image inspect --format '{{.Id}}' "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE")"
  LC_CAND_PLATFORM_DIGEST="$(docker image inspect --platform "$LC_PLATFORM" --format '{{.Id}}' "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE")"
  [[ "$LC_CAND_SOURCE_ID" =~ ^sha256:[0-9a-f]{64}$ && "$LC_CAND_SOURCE_ID" != "$LC_PREV_ID" ]] \
    || cluster_die "candidate build did not produce a distinct image index"
  [[ "$LC_CAND_PLATFORM_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || cluster_die "candidate build did not expose a platform manifest for $LC_PLATFORM"
  docker pull "$LC_TASK" >"$LC_ART/cluster-logs/pull-task.log" 2>&1 || cluster_die "task image pull failed"
  docker image inspect "$LC_KIND" >/dev/null 2>&1 || docker pull "$LC_KIND" >"$LC_ART/cluster-logs/pull-kind.log" 2>&1 || cluster_die "kind image unavailable"
  LC_BUILDER="${CAESIUM_LIFECYCLE_BUILDER_IMAGE:-caesiumcloud/caesium-builder:latest}"
  docker image inspect "$LC_BUILDER" >/dev/null 2>&1 || just builder >"$LC_ART/cluster-logs/builder.log" 2>&1 || cluster_die "builder unavailable"
  docker run --rm -v "$ROOT":/bld/caesium -v "$LC_ART":/artifacts -w /bld/caesium \
    -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false "$LC_BUILDER" \
    go test -tags=integration -c ./test/lifecycle -o /artifacts/lifecycle.test \
    >"$LC_ART/cluster-logs/compile.log" 2>&1 || cluster_die "integration-tagged lifecycle runner did not compile"
  [[ -x "$LC_ART/lifecycle.test" ]] || cluster_die "runner binary absent"
  docker build -t "$LC_RUNNER" -f - "$LC_ART" >"$LC_ART/cluster-logs/build-runner.log" 2>&1 <<'DOCKERFILE' || cluster_die "runner image build failed"
FROM alpine:3.23
COPY lifecycle.test /lifecycle.test
RUN chmod 0555 /lifecycle.test
DOCKERFILE
  cat >"$LC_ART/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $LC_ID
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
  # Image builds may take minutes. Re-check immediately before claiming this
  # cluster name; a failed kind/Docker inventory must never imply absence.
  lc_require_absent_cluster
  if ! kind create cluster --name "$LC_ID" --image "$LC_KIND" --config "$LC_ART/kind.yaml" \
    --kubeconfig "$LC_KUBE" --wait 120s >"$LC_ART/cluster-logs/kind-create.log" 2>&1; then
    # Any failed create might be a concurrent name conflict, including Docker's
    # "already in use" wording. Without a successful create we cannot prove
    # ownership and must leave any same-name cluster for explicit inspection.
    cluster_die "kind create failed; ownership not claimed; inspect cluster-logs/kind-create.log"
  fi
  LC_OWNED=1
  kind get nodes --name "$LC_ID" >"$LC_ART/cluster-logs/kind-nodes.txt" \
    2>"$LC_ART/cluster-logs/kind-nodes-error.log" || cluster_die "cannot enumerate owned kind nodes"
  [[ $(wc -l <"$LC_ART/cluster-logs/kind-nodes.txt") -eq 4 ]] \
    || cluster_die "owned kind cluster does not have four nodes"
  while IFS= read -r node; do
    [[ "$node" == "$LC_ID-"* ]] || cluster_die "kind returned a node outside owned cluster: $node"
  done <"$LC_ART/cluster-logs/kind-nodes.txt"
  lc_run_timed() {
    local seconds="$1" log="$2"
    shift 2
    python3 - "$seconds" "$log" "$@" <<'PY'
import os,signal,subprocess,sys,time
seconds=int(sys.argv[1]);log=sys.argv[2];command=sys.argv[3:]
with open(log,'a') as out:
  try:process=subprocess.Popen(command,stdout=out,stderr=subprocess.STDOUT,start_new_session=True)
  except OSError as error:
    out.write(f'command could not start: {command!r}: {error}\n')
    raise SystemExit(127)
  try:
    code=process.wait(timeout=seconds)
  except subprocess.TimeoutExpired:
    out.write(f'command timed out after {seconds}s: {command!r}\n')
    out.flush()
    group=process.pid
    try:os.killpg(group,signal.SIGTERM)
    except ProcessLookupError:pass
    # The direct child can exit on TERM while its descendants ignore it.
    # Keep the original group ID and kill that group after the grace period
    # even when process.wait() would already report the parent as finished.
    time.sleep(3)
    try:
      os.killpg(group,signal.SIGKILL)
      out.write(f'sent SIGKILL to timed-out process group {group}\n')
    except ProcessLookupError:pass
    try:process.wait(timeout=3)
    except subprocess.TimeoutExpired:
      out.write(f'direct child did not reap after group SIGKILL: {command!r}\n')
    raise SystemExit(124)
raise SystemExit(code)
PY
  }
  lc_wait_containerd() {
    local deadline=$((SECONDS + 120)) node ready remaining
    while (( SECONDS < deadline )); do
      ready=1
      while IFS= read -r node; do
        remaining=$((deadline - SECONDS))
        if (( remaining <= 0 )); then
          printf '%s owned kind node containerd readiness timed out after 120s\n' \
            "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$LC_ART/cluster-logs/containerd-readiness.log"
          return 1
        fi
        if (( remaining > 8 )); then remaining=8; fi
        if ! lc_run_timed "$remaining" "$LC_ART/cluster-logs/containerd-readiness.log" \
            docker exec --privileged "$node" ctr --namespace=k8s.io images ls; then
          printf '%s %s containerd probe failed\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$node" \
            >>"$LC_ART/cluster-logs/containerd-readiness.log"
          ready=0
        fi
      done <"$LC_ART/cluster-logs/kind-nodes.txt"
      if [[ "$ready" == 1 ]]; then
        printf '%s all owned kind node containerd sockets ready\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
          >>"$LC_ART/cluster-logs/containerd-readiness.log"
        return 0
      fi
      sleep 2
    done
    printf '%s owned kind node containerd readiness timed out after 120s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$LC_ART/cluster-logs/containerd-readiness.log"
    return 1
  }
  lc_kind_load() {
    local label="$1" subcommand="$2" attempt
    shift 2
    for attempt in 1 2 3; do
      lc_wait_containerd || return 1
      printf 'attempt %s: kind load %s\n' "$attempt" "$label" >>"$LC_ART/cluster-logs/kind-load-$label.log"
      if lc_run_timed 180 "$LC_ART/cluster-logs/kind-load-$label.log" \
          kind load "$subcommand" --name "$LC_ID" "$@"; then
        return 0
      fi
      if [[ "$attempt" != 3 ]]; then sleep 3; fi
    done
    return 1
  }
  # Docker Desktop keeps a multi-platform index while storing only the local
  # platform's child. kind's default --all-platforms import asks for missing
  # children; export one verified platform without retagging the release.
  docker image save --platform "$LC_PLATFORM" --output "$LC_ART/previous-platform.tar" "$LC_PREV" \
    >"$LC_ART/cluster-logs/save-previous.log" 2>&1 || cluster_die "cannot export pinned previous platform $LC_PLATFORM"
  docker image save --platform "$LC_PLATFORM" --output "$LC_ART/candidate-platform.tar" "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" \
    >"$LC_ART/cluster-logs/save-candidate.log" 2>&1 || cluster_die "cannot export built candidate platform $LC_PLATFORM"
  [[ "$(docker image inspect --format '{{.Id}}' "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE")" == "$LC_CAND_SOURCE_ID" ]] \
    || cluster_die "candidate tag changed between build and archive export"
  [[ "$(docker image inspect --platform "$LC_PLATFORM" --format '{{.Id}}' "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE")" == "$LC_CAND_PLATFORM_DIGEST" ]] \
    || cluster_die "candidate platform changed between build and archive export"
  LC_CAND_CONFIG_ID="$(lc_verify_candidate_archive "$LC_ART/candidate-platform.tar" \
    "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" "$LC_PLATFORM" "$LC_CAND_PLATFORM_DIGEST" \
    "$LC_CAND_SOURCE_ID" "$LC_ART/candidate-image-archive.json" "$LC_SHA" \
    2>"$LC_ART/cluster-logs/candidate-archive-verification.log")" \
    || cluster_die "candidate archive does not bind the built tag to its platform config"
  docker image save --platform "$LC_PLATFORM" --output "$LC_ART/task-platform.tar" "$LC_TASK" \
    >"$LC_ART/cluster-logs/save-task.log" 2>&1 || cluster_die "cannot export task platform $LC_PLATFORM"
  LC_PREV_CONFIG_ID="$(LC_ART="$LC_ART" LC_PREV="$LC_PREV" LC_PLATFORM="$LC_PLATFORM" \
    LC_PREV_PLATFORM_DIGEST="$LC_PREV_PLATFORM_DIGEST" python3 - \
    2>"$LC_ART/cluster-logs/previous-archive-verification.log" <<'PY'
import hashlib,json,os,pathlib,re,tarfile
art=pathlib.Path(os.environ['LC_ART']);archive=art/'previous-platform.tar'
tag=os.environ['LC_PREV'];platform=os.environ['LC_PLATFORM']
pinned=os.environ['LC_PREV_PLATFORM_DIGEST']
def require(condition,detail):
  if not condition:raise SystemExit(detail)
def digest_blob(tar,digest):
  require(re.fullmatch(r'sha256:[0-9a-f]{64}',digest),f'invalid archive digest {digest!r}')
  member=tar.getmember('blobs/sha256/'+digest.split(':',1)[1])
  require(member.isfile(),f'archive blob {digest} is not a file')
  data=tar.extractfile(member).read()
  require(hashlib.sha256(data).hexdigest()==digest.split(':',1)[1],f'archive blob {digest} hash mismatch')
  return data
with tarfile.open(archive) as tar:
  names=tar.getnames()
  require(len(names)==len(set(names)),'archive contains duplicate paths')
  manifest=json.load(tar.extractfile('manifest.json'))
  index=json.load(tar.extractfile('index.json'))
  require(len(manifest)==1 and manifest[0].get('RepoTags')==[tag],
    f'archive does not contain exactly the pinned repo tag {tag}')
  require(index.get('schemaVersion')==2 and len(index.get('manifests',[]))==1,
    'archive index must contain one platform manifest')
  descriptor=index['manifests'][0]
  target_os,target_arch=platform.split('/',1)
  require(descriptor.get('platform',{}).get('os')==target_os and
    descriptor.get('platform',{}).get('architecture')==target_arch,
    f'archive platform differs from {platform}')
  require(descriptor.get('digest')==pinned,f'archive manifest differs from pinned {pinned}')
  manifest_bytes=digest_blob(tar,pinned)
  require(descriptor.get('size')==len(manifest_bytes),'platform manifest size mismatch')
  image_manifest=json.loads(manifest_bytes)
  require(image_manifest.get('schemaVersion')==2,'invalid platform image manifest')
  config_descriptor=image_manifest.get('config',{})
  config_digest=config_descriptor.get('digest','')
  require(manifest[0].get('Config')=='blobs/sha256/'+config_digest.removeprefix('sha256:'),
    'archive Config path differs from pinned platform manifest')
  config_bytes=digest_blob(tar,config_digest)
  require(config_descriptor.get('size')==len(config_bytes),'image config size mismatch')
  config=json.loads(config_bytes)
  require(config.get('os')==target_os and config.get('architecture')==target_arch,
    f'image config platform differs from {platform}')
  layers=image_manifest.get('layers',[])
  require(bool(layers),'platform manifest has no layers')
  paths=['blobs/sha256/'+layer['digest'].removeprefix('sha256:') for layer in layers]
  require(manifest[0].get('Layers')==paths,'archive layer list differs from pinned platform manifest')
  for layer in layers:
    data=digest_blob(tar,layer['digest'])
    require(layer.get('size')==len(data),f'layer {layer["digest"]} size mismatch')
  index_bytes=tar.extractfile('index.json').read()
  record={'archive_ref':'previous-platform.tar','repo_tag':tag,'platform':platform,
    'archive_sha256':hashlib.sha256(archive.read_bytes()).hexdigest(),
    'archive_index_digest':'sha256:'+hashlib.sha256(index_bytes).hexdigest(),
    'pinned_platform_manifest_digest':pinned,'verified_config_digest':config_digest,
    'verified_layer_digests':[layer['digest'] for layer in layers],
    'verification':'archive tag, platform, manifest, config and layer hashes matched'}
  (art/'previous-image-archive.json').write_text(json.dumps(record,indent=2)+'\n')
  print(config_digest)
PY
)" || cluster_die "previous platform archive does not bind the pinned release to a verified config digest"
  LC_PREV_IDS="$LC_PREV_ID,$LC_PREV_PLATFORM_DIGEST,$LC_PREV_CONFIG_ID"
  lc_kind_load previous image-archive "$LC_ART/previous-platform.tar" \
    || cluster_die "kind previous platform import failed after bounded containerd retries"
  while IFS= read -r node; do
    lc_run_timed 20 "$LC_ART/cluster-logs/previous-import-$node.txt" \
      docker exec --privileged "$node" ctr --namespace=k8s.io images ls \
      || cluster_die "cannot inspect previous image import on owned node $node"
  done <"$LC_ART/cluster-logs/kind-nodes.txt"
  LC_ART="$LC_ART" LC_PREV="$LC_PREV" python3 - \
    2>"$LC_ART/cluster-logs/previous-import-verification.log" <<'PY' \
    || cluster_die "owned kind nodes did not import the verified previous image"
import hashlib,json,os,pathlib,re
art=pathlib.Path(os.environ['LC_ART']);tag=os.environ['LC_PREV']
proof=json.loads((art/'previous-image-archive.json').read_text())
if hashlib.sha256((art/'previous-platform.tar').read_bytes()).hexdigest()!=proof['archive_sha256']:
  raise SystemExit('previous image archive changed during kind import')
expected={proof['archive_index_digest'],proof['pinned_platform_manifest_digest']}
refs={tag,'docker.io/'+tag}
nodes=(art/'cluster-logs/kind-nodes.txt').read_text().splitlines()
observed=[]
for node in nodes:
  lines=(art/'cluster-logs'/f'previous-import-{node}.txt').read_text().splitlines()
  rows=[line.split() for line in lines if line.split() and line.split()[0] in refs]
  if not rows:raise SystemExit(f'{node}: no imported row for {tag}')
  for row in rows:
    digests=[field for field in row[1:] if re.fullmatch(r'sha256:[0-9a-f]{64}',field)]
    if len(digests)!=1 or digests[0] not in expected:
      raise SystemExit(f'{node}: imported tag digest {digests!r} differs from verified archive {sorted(expected)}')
    observed.append({'node':node,'repo_tag':row[0],'imported_target_digest':digests[0]})
if len({entry['node'] for entry in observed})!=4:
  raise SystemExit(f'expected four owned node imports, got {len({entry["node"] for entry in observed})}')
(art/'previous-image-node-imports.json').write_text(json.dumps({
  'archive_proof':'previous-image-archive.json','node_imports':observed},indent=2)+'\n')
PY
  lc_case previous-release-digest pass "pulled $LC_PREV for $LC_PLATFORM; repo digest $LC_PREV_ID; platform manifest $LC_PREV_PLATFORM_DIGEST; verified config $LC_PREV_CONFIG_ID; Docker platform inspect $LC_PREV_INSPECT_PLATFORM_ID; all owned nodes imported the verified archive" "$LC_ART/previous-image-node-imports.json"
  lc_kind_load task image-archive "$LC_ART/task-platform.tar" \
    || cluster_die "kind task platform import failed after bounded containerd retries"
  lc_kind_load candidate image-archive "$LC_ART/candidate-platform.tar" \
    || cluster_die "kind candidate platform import failed after bounded containerd retries"
  while IFS= read -r node; do
    lc_run_timed 20 "$LC_ART/cluster-logs/candidate-import-$node.txt" \
      docker exec --privileged "$node" ctr --namespace=k8s.io images ls \
      || cluster_die "cannot inspect candidate image import on owned node $node"
  done <"$LC_ART/cluster-logs/kind-nodes.txt"
  lc_verify_candidate_imports "$LC_ART/candidate-platform.tar" \
    "$LC_ART/candidate-image-archive.json" "$LC_ART/cluster-logs/kind-nodes.txt" \
    "$LC_ART/cluster-logs" "$LC_ART/candidate-image-node-imports.json" \
    "$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" \
    2>"$LC_ART/cluster-logs/candidate-import-verification.log" \
    || cluster_die "owned kind nodes did not import the verified candidate image"
  LC_CAND_ARCHIVE_INDEX="$(LC_ART="$LC_ART" python3 - <<'PY'
import json,os,pathlib
print(json.loads(pathlib.Path(os.environ['LC_ART'],'candidate-image-archive.json').read_text())['archive_index_digest'])
PY
)"
  LC_CAND_ID="$LC_CAND_ARCHIVE_INDEX,$LC_CAND_PLATFORM_DIGEST,$LC_CAND_CONFIG_ID"
  lc_case candidate-image-identity pass "clean checkout $LC_SHA built $CAESIUM_LIFECYCLE_CANDIDATE_IMAGE; all owned nodes imported its verified $LC_PLATFORM archive; allowed pod IDs $LC_CAND_ID" "$LC_ART/candidate-image-node-imports.json"
  lc_kind_load runner docker-image "$LC_RUNNER" \
    || cluster_die "kind runner image import failed after bounded containerd retries"
  helm template caesium "$ROOT/helm/caesium" --namespace "$LC_ID" --values "$LC_VALUES" \
    --set image.tag=v0.1.0 >"$LC_ART/manifest-before.yaml" || cluster_die "previous Helm render failed"
  helm template caesium "$ROOT/helm/caesium" --namespace "$LC_ID" --values "$LC_VALUES" \
    --set "image.tag=$LC_SHA" >"$LC_ART/manifest-after.yaml" || cluster_die "candidate Helm render failed"
  LC_PREV="$LC_PREV" LC_CAND="$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" LC_ART="$LC_ART" python3 - <<'PY' || cluster_die "rendered Helm resources changed beyond caesium image"
import difflib,os,pathlib
art=pathlib.Path(os.environ['LC_ART'])
a=(art/'manifest-before.yaml').read_text();b=(art/'manifest-after.yaml').read_text()
old='image: "'+os.environ['LC_PREV']+'"';new='image: "'+os.environ['LC_CAND']+'"'
if a.count(old)!=1 or b.count(new)!=1:raise SystemExit('rendered manifest lacks exactly one pinned server image')
na=a.replace(old,'image: "__LIFECYCLE_IMAGE__"');nb=b.replace(new,'image: "__LIFECYCLE_IMAGE__"')
diff=list(difflib.unified_diff(na.splitlines(),nb.splitlines(),fromfile='before',tofile='after'))
(art/'manifest-normalized.diff').write_text('\n'.join(diff)+'\n' if diff else '')
if diff:raise SystemExit('\n'.join(diff[:60]))
PY
  helm install caesium "$ROOT/helm/caesium" --kubeconfig "$LC_KUBE" --namespace "$LC_ID" \
    --create-namespace --values "$LC_VALUES" --set image.tag=v0.1.0 --wait --timeout 300s \
    >"$LC_ART/cluster-logs/helm-install.log" 2>&1 || cluster_die "previous-release Helm install failed"
  helm get manifest caesium --kubeconfig "$LC_KUBE" --namespace "$LC_ID" \
    >"$LC_ART/manifest-installed.yaml" || cluster_die "cannot read installed Helm manifest"
  lc_ns wait --for=condition=Ready pod/caesium-0 pod/caesium-1 pod/caesium-2 --timeout=300s \
    >"$LC_ART/cluster-logs/previous-ready.log" 2>&1 || cluster_die "previous pods not all Ready"
  lc_collect_info before || cluster_die "cannot record all previous-release info.yaml IDs and addresses"
  cat >"$LC_ART/runner.yaml" <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: lifecycle-runner, namespace: $LC_ID}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: lifecycle-runner, namespace: $LC_ID}
rules:
  - apiGroups: [""]
    resources: ["pods", "persistentvolumeclaims"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: lifecycle-runner, namespace: $LC_ID}
subjects: [{kind: ServiceAccount, name: lifecycle-runner, namespace: $LC_ID}]
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: lifecycle-runner}
---
apiVersion: v1
kind: Service
metadata: {name: lifecycle-recorder, namespace: $LC_ID}
spec:
  selector: {app.kubernetes.io/name: lifecycle-runner}
  ports:
    - {name: http, port: 8090, targetPort: 8090}
    - {name: pod-name, port: 8091, targetPort: 8091}
---
apiVersion: v1
kind: Pod
metadata:
  name: lifecycle-runner
  namespace: $LC_ID
  labels: {app.kubernetes.io/name: lifecycle-runner}
spec:
  serviceAccountName: lifecycle-runner
  restartPolicy: Never
  tolerations:
    - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
  nodeSelector: {node-role.kubernetes.io/control-plane: ""}
  volumes: [{name: artifacts, emptyDir: {}}]
  containers:
    - name: runner
      image: $LC_RUNNER
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "sleep 86400"]
      volumeMounts: [{name: artifacts, mountPath: /artifacts}]
    - name: recorder
      image: $LC_RUNNER
      imagePullPolicy: IfNotPresent
      command: ["/lifecycle.test"]
      args: ["-test.v", "-test.run", "^TestLifecycleClusterRecorder$", "-test.timeout", "24h"]
      env: [{name: CAESIUM_LIFECYCLE_CLUSTER_RECORDER, value: "1"}]
      ports: [{containerPort: 8090, name: http}, {containerPort: 8091, name: pod-name}]
      readinessProbe:
        httpGet: {path: /health, port: pod-name}
        periodSeconds: 1
EOF
  lc_ns apply -f "$LC_ART/runner.yaml" >"$LC_ART/cluster-logs/runner-create.log" 2>&1 || cluster_die "runner create failed"
  lc_ns wait --for=condition=Ready pod/lifecycle-runner --timeout=120s \
    >"$LC_ART/cluster-logs/runner-ready.log" 2>&1 || cluster_die "runner not Ready"
  lc_ns cp "$LC_ART/versions.json" lifecycle-runner:/artifacts/versions.json -c runner || cluster_die "cannot copy matrix to runner"
  LC_OLD_BASE="$(lc_base)"
  lc_phase Seed "$LC_PREV_IDS" "$LC_OLD_BASE" || cluster_die "previous-release cluster seed failed (see cluster-logs/Seed.log)"
  lc_copy_runner_artifacts
  LC_MIXED_RC=0
  lc_phase MixedWindow "$LC_PREV_IDS" "$LC_OLD_BASE" &
  LC_MIXED_PID=$!
  LC_HELM_RC=0
  helm upgrade caesium "$ROOT/helm/caesium" --kubeconfig "$LC_KUBE" --namespace "$LC_ID" \
    --values "$LC_VALUES" --set "image.tag=$LC_SHA" --wait --timeout 600s \
    >"$LC_ART/cluster-logs/helm-upgrade.log" 2>&1 || LC_HELM_RC=$?
  LC_GET_RC=0
  helm get manifest caesium --kubeconfig "$LC_KUBE" --namespace "$LC_ID" \
    >"$LC_ART/manifest-upgraded.yaml" || LC_GET_RC=$?
  if [[ "$LC_GET_RC" == 0 ]]; then
    LC_PREV="$LC_PREV" LC_CAND="$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" LC_ART="$LC_ART" python3 - <<'PY' || LC_GET_RC=$?
import difflib,os,pathlib
art=pathlib.Path(os.environ['LC_ART'])
a=(art/'manifest-installed.yaml').read_text();b=(art/'manifest-upgraded.yaml').read_text()
old='image: "'+os.environ['LC_PREV']+'"';new='image: "'+os.environ['LC_CAND']+'"'
if a.count(old)!=1 or b.count(new)!=1:raise SystemExit('installed/upgraded manifest lacks exactly one expected image')
a=a.replace(old,'image: "__LIFECYCLE_IMAGE__"');b=b.replace(new,'image: "__LIFECYCLE_IMAGE__"')
diff=list(difflib.unified_diff(a.splitlines(),b.splitlines(),fromfile='installed',tofile='upgraded'))
(art/'manifest-live-normalized.diff').write_text('\n'.join(diff)+'\n' if diff else '')
if diff:raise SystemExit('\n'.join(diff[:60]))
PY
  fi
  wait "$LC_MIXED_PID" || LC_MIXED_RC=$?
  lc_ns get pods -o wide >"$LC_ART/cluster-logs/pods-after-upgrade.txt" 2>&1 || true
  lc_ns get pods -o json >"$LC_ART/cluster-pods-after-upgrade.json" 2>&1 || true
  for n in 0 1 2; do
    lc_ns logs "caesium-$n" -c caesium --tail=-1 >"$LC_ART/cluster-logs/candidate-$n.log" 2>&1 || true
    lc_ns logs "caesium-$n" -c caesium --previous --tail=-1 >"$LC_ART/cluster-logs/candidate-$n-previous.log" 2>&1 || true
  done
  # A retained-PVC node can be killed before info.yaml is readable. Classify
  # the known address mismatch only from all three independent observations:
  # persisted old address, newly assigned pod IP, and exit-1 process log.
  LC_ADDRESS_BLOCKED=0
  LC_ART="$LC_ART" python3 - <<'PY' && LC_ADDRESS_BLOCKED=1 || true
import json,os,pathlib,re
art=pathlib.Path(os.environ['LC_ART'])
prior={row['name']:row for row in json.loads((art/'before-info.json').read_text())}
pods=json.loads((art/'cluster-pods-after-upgrade.json').read_text())
observations=[]
for pod in pods.get('items',[]):
  name=pod.get('metadata',{}).get('name','')
  if name not in prior:continue
  ip=pod.get('status',{}).get('podIP','')
  statuses=[s for s in pod.get('status',{}).get('containerStatuses',[]) if s.get('name')=='caesium']
  exits=[s.get(state,{}).get('terminated',{}).get('exitCode') for s in statuses for state in ('state','lastState')]
  old=prior[name]['address']
  current=(art/'cluster-logs'/f'candidate-{name.rsplit("-",1)[1]}.log').read_text()
  previous=(art/'cluster-logs'/f'candidate-{name.rsplit("-",1)[1]}-previous.log').read_text()
  evidence='\n'.join((current,previous))
  mismatch=bool(ip and old!=f'{ip}:9001' and 1 in exits and
    re.search(r'in info\.yaml does not match|address[^\n]*does not match',evidence,re.I))
  observations.append({'pod':name,'persisted_address':old,'new_pod_ip':ip,
    'expected_address':f'{ip}:9001' if ip else None,'observed_exit_codes':exits,
    'address_mismatch_log':mismatch,'log_files':[
      f'cluster-logs/candidate-{name.rsplit("-",1)[1]}.log',
      f'cluster-logs/candidate-{name.rsplit("-",1)[1]}-previous.log']})
record={'classification':'blocked-by-prerequisite' if any(o['address_mismatch_log'] for o in observations) else 'not-observed',
  'observations':observations}
(art/'cluster-address-classification.json').write_text(json.dumps(record,indent=2)+'\n')
if record['classification']!='blocked-by-prerequisite':raise SystemExit(1)
PY
  LC_INFO_RC=0
  lc_collect_info after || LC_INFO_RC=$?
  LC_ART="$LC_ART" LC_HELM_RC="$LC_HELM_RC" LC_GET_RC="$LC_GET_RC" python3 - <<'PY'
import json,os,pathlib
art=pathlib.Path(os.environ['LC_ART'])
load=lambda name:json.loads((art/name).read_text()) if (art/name).exists() else []
pods=load('cluster-pods-after-upgrade.json')
pod_statuses={}
for pod in pods.get('items',[]):
  name=pod.get('metadata',{}).get('name','')
  if name not in [f'caesium-{n}' for n in range(3)]:continue
  status=pod.get('status',{})
  containers=[c for c in status.get('containerStatuses',[]) if c.get('name')=='caesium']
  c=containers[0] if containers else {}
  pod_statuses[name]={'phase':status.get('phase',''),'pod_ip':status.get('podIP',''),
    'ready':c.get('ready',False),'restart_count':c.get('restartCount',-1),
    'state':c.get('state',{}),'last_state':c.get('lastState',{})}
record={'before_info':load('before-info.json'),'after_info':load('after-info.json'),
  'address_classification':load('cluster-address-classification.json'),
  'pod_statuses':pod_statuses,
  'manifest_diff':(art/'manifest-normalized.diff').read_text().splitlines(),
  'manifest_live_captured':int(os.environ['LC_GET_RC'])==0,
  'manifest_live_diff':(art/'manifest-live-normalized.diff').read_text().splitlines() if (art/'manifest-live-normalized.diff').exists() else [],
  'helm_exit_code':int(os.environ['LC_HELM_RC']),
  'pod_logs':{f'caesium-{n}':(art/'cluster-logs'/f'candidate-{n}.log').read_text() for n in range(3)},
  'previous_pod_logs':{f'caesium-{n}':(art/'cluster-logs'/f'candidate-{n}-previous.log').read_text() for n in range(3)}}
(art/'cluster-host-observation.json').write_text(json.dumps(record,indent=2)+'\n')
PY
  lc_ns cp "$LC_ART/cluster-host-observation.json" lifecycle-runner:/artifacts/cluster-host-observation.json -c runner \
    || cluster_die "cannot copy host observations into runner"
  LC_AFTER_RC=0
  lc_phase AfterUpgrade "$LC_CAND_ID" "$(lc_base)" || LC_AFTER_RC=$?
  lc_copy_runner_artifacts || true
  [[ "$LC_MIXED_RC" == 0 ]] || lc_case mixed-version-dispatch-and-completion blocked "mixed-window runner failed or could not observe protocol-2 peers"
  if [[ "$LC_ADDRESS_BLOCKED" == 1 ]]; then
    lc_case rolling-upgrade-three-voters blocked "blocked-by-prerequisite: retained-PVC address mismatch persisted after #536; old address, new pod IP, exit 1 and process logs recorded" "$LC_ART/cluster-address-classification.json"
  elif [[ "$LC_INFO_RC" != 0 ]]; then
    lc_case rolling-upgrade-three-voters blocked "post-upgrade info.yaml could not be captured on all members"
  elif [[ "$LC_GET_RC" != 0 ]]; then
    lc_case rolling-upgrade-three-voters blocked "installed/upgraded Helm manifest could not be compared as image-only"
  elif [[ "$LC_AFTER_RC" != 0 ]]; then
    # A recorded runner case identifies whether upgrade state was blocked or
    # failed. A later retained-history failure must not overwrite its pass.
    [[ -f "$LC_ART/cases/rolling-upgrade-three-voters.json" ]] || \
      lc_case rolling-upgrade-three-voters blocked "runner produced no upgrade case; inspect AfterUpgrade.log and pod/Raft observations"
  fi
  LC_ROLLING_PASS=0
  if LC_ART="$LC_ART" LC_ID="$LC_ID" python3 - <<'PY'; then
import json,os,pathlib
path=pathlib.Path(os.environ['LC_ART'],'cases','rolling-upgrade-three-voters.json')
try:
  record=json.loads(path.read_text())
except (OSError,ValueError):
  raise SystemExit(1)
raise SystemExit(0 if record.get('lifecycle_id')==os.environ['LC_ID'] and record.get('status')=='pass' else 1)
PY
    LC_ROLLING_PASS=1
  fi
  # F2's destructive cases are reported individually. Their launch requires a
  # healthy upgraded quorum. The runner proves that state from pods, the live
  # manifest and direct Raft membership; Helm --wait can time out after that
  # proof, and a later retained-history assertion can make AfterUpgrade fail.
  if [[ "$LC_ROLLING_PASS" != 1 || "$LC_INFO_RC" != 0 || "$LC_GET_RC" != 0 || "$LC_ADDRESS_BLOCKED" == 1 ]]; then
    for name in joining-ordinal-1-replacement ordinal-0-disk-loss snapshot-catch-up storage-snapshot-restore rollback-recorded-outcome; do
      lc_case "$name" blocked "cannot run after failed/unobservable three-member upgrade"
    done
  else
    # A helper mounts the PVC only while ordinal 2 is scaled down. Its file
    # reads come from that local volume: no HTTP/SQL request can be answered by
    # a surviving leader. These are storage-level experiments, not a product
    # backup or restore command.
    lc_storage_helper_start() {
      cat >"$LC_ART/storage-helper.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata: {name: lifecycle-storage, namespace: $LC_ID}
spec:
  restartPolicy: Never
  securityContext: {runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001}
  containers:
    - name: storage
      image: $LC_TASK
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts: [{name: data, mountPath: /data}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: data-caesium-2}
EOF
      lc_ns apply -f "$LC_ART/storage-helper.yaml" >/dev/null
      lc_ns wait --for=condition=Ready pod/lifecycle-storage --timeout=120s >/dev/null
      lc_ns exec pod/lifecycle-storage -c storage -- test -f /data/info.yaml
    }
    lc_storage_helper_stop() {
      lc_ns delete pod lifecycle-storage --ignore-not-found=true --wait=true --timeout=120s \
        >>"$LC_ART/cluster-logs/storage-helper-delete.log" 2>&1
    }
    lc_scale_two() {
      lc_ns scale statefulset/caesium --replicas=2 >/dev/null
      lc_ns wait --for=delete pod/caesium-2 --timeout=120s >/dev/null
    }
    lc_scale_three() {
      lc_storage_helper_stop || return 1
      lc_ns scale statefulset/caesium --replicas=3 >/dev/null
      lc_ns wait --for=condition=Ready pod/caesium-2 --timeout=300s >/dev/null
    }
    lc_manifest_local() {
      lc_ns exec pod/lifecycle-storage -c storage -- sh -c \
        'cd /data && find . -type f -exec sha256sum {} \; | sort' >"$1"
    }
    lc_files() {
      lc_ns exec "$1" -c caesium -- sh -c \
        'cd /var/lib/caesium/dqlite && find . -type f -exec ls -ln {} \; | sort' >"$2"
    }
    lc_snapshot_hashes() {
      lc_ns exec "$1" -c caesium -- sh -c \
        'cd /var/lib/caesium/dqlite && for f in snapshot-*-*-*; do case "$f" in *.meta) continue;; esac; test -f "$f" && sha256sum "$f"; done' \
        >"$2" || return 1
      [[ -s "$2" ]]
    }
    # Preserve the live state at the write failure, before storage-helper
    # cleanup or kind teardown can erase the evidence. The runner records a
    # five-second direct readback of the disputed annotation from each survivor;
    # a missing readback remains explicitly unavailable, never a successful
    # write or a snapshot pass.
    lc_capture_snapshot_write_failure() {
      local batch="$1" prefix="$LC_ART/cluster-logs/snapshot-failure-batch-$1"
      local pods_rc=0 events_rc=0 log0_rc=0 log1_rc=0 prev_log0_rc=0 prev_log1_rc=0 copy_rc=0
      lc_ns --request-timeout=10s get pods -o json >"$prefix-pods.json" 2>&1 || pods_rc=$?
      lc_ns --request-timeout=10s logs caesium-0 -c caesium --previous --tail=2000 --timestamps=true \
        >"$prefix-caesium-0.previous.log" 2>&1 || prev_log0_rc=$?
      lc_ns --request-timeout=10s logs caesium-1 -c caesium --previous --tail=2000 --timestamps=true \
        >"$prefix-caesium-1.previous.log" 2>&1 || prev_log1_rc=$?
      lc_ns --request-timeout=10s get events --sort-by=.metadata.creationTimestamp -o wide \
        >"$prefix-events.txt" 2>&1 || events_rc=$?
      lc_ns --request-timeout=10s logs caesium-0 -c caesium --tail=2000 --timestamps=true \
        >"$prefix-caesium-0.log" 2>&1 || log0_rc=$?
      lc_ns --request-timeout=10s logs caesium-1 -c caesium --tail=2000 --timestamps=true \
        >"$prefix-caesium-1.log" 2>&1 || log1_rc=$?
      lc_run_timed 20 "$prefix-artifact-copy.log" kubectl --kubeconfig "$LC_KUBE" \
        --namespace "$LC_ID" cp -c runner lifecycle-runner:/artifacts/. "$LC_ART/" || copy_rc=$?
      LC_ART="$LC_ART" LC_ID="$LC_ID" LC_SNAP_BATCH="$batch" LC_DIAG_PODS_RC="$pods_rc" \
        LC_DIAG_EVENTS_RC="$events_rc" LC_DIAG_LOG0_RC="$log0_rc" \
        LC_DIAG_LOG1_RC="$log1_rc" LC_DIAG_PREV_LOG0_RC="$prev_log0_rc" \
        LC_DIAG_PREV_LOG1_RC="$prev_log1_rc" LC_DIAG_COPY_RC="$copy_rc" \
        python3 "$ROOT/scripts/lifecycle-snapshot-failure.py"
    }
    # Exit 0 only after both surviving voters' actual files prove truncation;
    # 2 means another bounded batch is needed, 3 is the on-disk safety cap,
    # and 1 means evidence is missing or a survivor changed/restarted.
    lc_snapshot_progress() {
      LC_ART="$LC_ART" LC_SNAP_BATCH="$1" python3 - <<'PY'
import json,os,pathlib,re,sys
art=pathlib.Path(os.environ['LC_ART']);base=art/'cluster-logs'
batch=int(os.environ['LC_SNAP_BATCH']);out=base/f'snapshot-batch-{batch:02d}.json'
obs={'batch':batch,'batch_size':500,'batch_cap':18,'leader_file_byte_cap':1536*1024*1024}
def require(ok,msg):
  if not ok:raise ValueError(msg)
def load(path):
  p=pathlib.Path(path)
  require(p.is_file() and p.stat().st_size>0,f'missing {p.name}')
  return json.loads(p.read_text())
def files(name):
  p=base/name
  require(p.is_file() and p.stat().st_size>0,f'missing {name}')
  found=[]
  for line in p.read_text().splitlines():
    fields=line.split()
    require(len(fields)>=9 and fields[0].startswith('-') and fields[4].isdigit(),
      f'unparseable file observation in {name}: {line!r}')
    found.append((fields[-1],int(fields[4])))
  require(bool(found),f'empty file observation {name}')
  return found
def indexes(rows):
  snapshots=[];segments=[]
  for path,_ in rows:
    m=re.search(r'(?:^|/)snapshot-(\d+)-(\d+)-(\d+)$',path)
    if m:snapshots.append(int(m.group(2)))
    m=re.search(r'(?:^|/)(\d+)-(\d+)$',path)
    if m:segments.append((int(m.group(1)),int(m.group(2))))
  return snapshots,segments
try:
  before=load(base/'leader-before.json');after=load(base/f'leader-after-batch-{batch:02d}.json')
  for key in ('name','uid','ip','address','image_id','survivors'):
    require(key in before and key in after,f'missing leader identity {key}')
    require(before[key]==after[key],f'Raft leader or survivor changed during writes: {key}')
  require(before['name'] in ('caesium-0','caesium-1'),'leader is not a surviving member')
  require(set(before['survivors'])=={'caesium-0','caesium-1'},'missing survivor identity')
  for name,identity in before['survivors'].items():
    for key in ('uid','container_id','restart_count','started_at'):
      require(identity.get(key) not in (None,''),f'{name} missing {key}')
  original=load(art/'cluster-snapshot-write-count.json')
  require(original.get('acknowledged_applies')==1400 and original.get('required_applies')==1400,
    'distinct catalog write floor was not acknowledged')
  total=1400;job_id=None;update_batches=[]
  for n in range(1,batch+1):
    rec=load(art/f'cluster-snapshot-update-batch-{n:02d}.json')
    require(rec.get('batch')==n and rec.get('required_applies')==500 and
      rec.get('acknowledged_applies')==500 and
      rec.get('first_annotation')==(n-1)*500+1 and rec.get('last_annotation')==n*500,
      f'update batch {n} has missing or non-monotonic acknowledgements')
    require(bool(rec.get('job_id')) and bool(rec.get('alias')),f'update batch {n} lost catalog identity')
    if job_id is None:job_id=rec['job_id']
    require(rec['job_id']==job_id,f'update batch {n} changed catalog identity')
    total+=500;update_batches.append(rec)
  before_snapshots,_=indexes(files('leader-before-snapshot-files.txt'))
  stopped_snapshots,stopped_segments=indexes(files('stopped-member-before-writes-files.txt'))
  stopped_bound=load(base/'stopped-member-raft-index.json')
  require(stopped_bound.get('last_persisted_index_upper_bound',0)>0,
    'stopped member open Raft tail has no measured upper bound')
  leader_rows=files(f'leader-after-batch-{batch:02d}-snapshot-files.txt')
  leader_snapshots,leader_segments=indexes(leader_rows)
  require(before_snapshots and stopped_segments and leader_snapshots and leader_segments,
    'missing leader/stopped snapshot or Raft segment indexes')
  require(all(start<=end for start,end in stopped_segments+leader_segments),
    'invalid Raft segment range')
  stopped_end=max(end for _,end in stopped_segments)
  stopped_upper=stopped_bound['last_persisted_index_upper_bound']
  require(stopped_upper>=stopped_end,'stopped member index bound is below closed segments')
  leader_bytes=sum(size for _,size in leader_rows)
  other_rows=files(f'other-after-batch-{batch:02d}-snapshot-files.txt')
  other_snapshots,other_segments=indexes(other_rows)
  require(other_snapshots and other_segments,'missing other survivor snapshot or Raft segment indexes')
  require(all(start<=end for start,end in other_segments),'invalid other survivor Raft segment range')
  leader_truncated=(max(leader_snapshots)>max(before_snapshots) and
    max(leader_snapshots)>stopped_upper and
    min(start for start,_ in leader_segments)>stopped_upper+1)
  other_truncated=(max(other_snapshots)>stopped_upper and
    min(start for start,_ in other_segments)>stopped_upper+1)
  truncated=leader_truncated and other_truncated
  obs.update({'leader_before':before,'leader_after':after,
    'acknowledged_distinct_applies':1400,'acknowledged_update_applies':total-1400,
    'acknowledged_total_applies':total,'update_batches':update_batches,
    'leader_before_snapshot_indexes':before_snapshots,
    'stopped_snapshot_indexes':stopped_snapshots,
    'stopped_segment_ranges':stopped_segments,'stopped_segment_end':stopped_end,
    'stopped_member_index_bound':stopped_bound,
    'leader_snapshot_indexes':leader_snapshots,'leader_segment_ranges':leader_segments,
    'other_survivor':'caesium-1' if before['name']=='caesium-0' else 'caesium-0',
    'other_snapshot_indexes':other_snapshots,'other_segment_ranges':other_segments,
    'leader_file_bytes':leader_bytes,'other_file_bytes':sum(size for _,size in other_rows),
    'leader_truncation_proved':leader_truncated,
    'other_truncation_proved':other_truncated,'truncation_proved':truncated})
  out.write_text(json.dumps(obs,indent=2)+'\n')
  if truncated:sys.exit(0)
  if leader_bytes>=obs['leader_file_byte_cap']:
    print(f'leader on-disk bytes {leader_bytes} reached cap',file=sys.stderr)
    sys.exit(3)
  sys.exit(2)
except (OSError,ValueError,KeyError,TypeError,json.JSONDecodeError) as exc:
  obs['measurement_error']=str(exc)
  out.write_text(json.dumps(obs,indent=2)+'\n')
  print(f'snapshot batch {batch}: {exc}',file=sys.stderr)
  sys.exit(1)
PY
    }
    # The closed filename ceiling omits entries in open-N. Read the stopped
    # PVC's open segment bytes while it is exclusively mounted by the helper.
    # This decoder follows dqlite v1.18.7's uv_segment.c/uv_encoding.c format;
    # unknown, changed or corrupt bytes block the case rather than shrinking
    # the upper bound on what the stopped member could replay locally.
    lc_stopped_raft_index() {
      local names="$LC_ART/cluster-logs/stopped-open-names.txt" name
      mkdir -p "$LC_ART/cluster-logs/stopped-open-segments"
      LC_ART="$LC_ART" python3 - >"$names" <<'PY'
import os,pathlib,re
p=pathlib.Path(os.environ['LC_ART'],'cluster-logs','stopped-member-before-writes-files.txt')
names=[]
for line in p.read_text().splitlines():
  fields=line.split()
  if not fields:continue
  m=re.fullmatch(r'\./(open-(\d+))',fields[-1])
  if m:names.append((int(m.group(2)),m.group(1)))
if len(names)!=len(set(n for n,_ in names)):
  raise SystemExit('duplicate stopped open segment counter')
for _,name in sorted(names):print(name)
PY
      [[ "$?" == 0 ]] || return 1
      while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        lc_ns cp -c storage "lifecycle-storage:/data/$name" \
          "$LC_ART/cluster-logs/stopped-open-segments/$name" || return 1
      done <"$names"
      LC_ART="$LC_ART" python3 - <<'PY'
import hashlib,json,os,pathlib,re,struct,sys
art=pathlib.Path(os.environ['LC_ART']);base=art/'cluster-logs'
out=base/'stopped-member-raft-index.json'
obs={'decoder':'dqlite v1.18.7 uv_segment.c/uv_encoding.c','open_segments':[]}
def require(ok,msg):
  if not ok:raise ValueError(msg)
def crc32(data):
  # libraft byteCrc32 uses the non-reflected 0x04c11db7 polynomial, seed 0.
  table=[]
  for i in range(256):
    x=i<<24
    for _ in range(8):x=((x<<1)^0x04c11db7 if x&0x80000000 else x<<1)&0xffffffff
    table.append(x)
  value=0
  for byte in data:value=((value<<8)^table[((value>>24)^byte)&255])&0xffffffff
  return value
def open_count(name,expected_size):
  data=(base/'stopped-open-segments'/name).read_bytes()
  require(len(data)==expected_size,f'{name} changed size during copy')
  if not any(data):return 0
  require(len(data)>=8 and struct.unpack_from('<Q',data)[0]==1,
    f'{name} has unknown Raft disk format')
  offset=8;count=0
  while offset<len(data):
    if data[offset:offset+16]==b'\0'*16:
      require(not any(data[offset:]),f'{name} has nonzero data after open tail')
      break
    require(offset+24<=len(data),f'{name} has incomplete batch preamble')
    header_crc,data_crc=struct.unpack_from('<II',data,offset)
    n=struct.unpack_from('<Q',data,offset+8)[0]
    require(0<n<=8*1024*1024//32,f'{name} has invalid batch count {n}')
    header_end=offset+16+16*n
    require(header_end<=len(data),f'{name} batch header exceeds file')
    header=memoryview(data)[offset+8:header_end]
    require(crc32(header)==header_crc,f'{name} batch header CRC mismatch')
    payload_size=0
    for i in range(n):
      term,kind,size=struct.unpack_from('<QB3xI',data,offset+16+i*16)
      require(term>0 and kind in (1,2,3) and size%8==0,
        f'{name} has invalid entry metadata')
      payload_size+=size
    payload_end=header_end+payload_size
    require(payload_end<=len(data),f'{name} batch payload exceeds file')
    require(crc32(memoryview(data)[header_end:payload_end])==data_crc,
      f'{name} batch data CRC mismatch')
    count+=n;offset=payload_end
  return count
try:
  rows=(base/'stopped-member-before-writes-files.txt').read_text().splitlines()
  sizes={};closed=[];snapshots=[];opens=[]
  for line in rows:
    fields=line.split()
    require(len(fields)>=9 and fields[4].isdigit(),f'unparseable stopped file: {line!r}')
    path=fields[-1];sizes[path]=int(fields[4])
    m=re.fullmatch(r'\./(\d+)-(\d+)',path)
    if m:closed.append((int(m.group(1)),int(m.group(2))))
    m=re.fullmatch(r'\./snapshot-\d+-(\d+)-\d+',path)
    if m:snapshots.append(int(m.group(1)))
    m=re.fullmatch(r'\./open-(\d+)',path)
    if m:opens.append((int(m.group(1)),path[2:]))
  require(closed and snapshots,'stopped member has no closed segment or snapshot index')
  require(all(start<=end for start,end in closed),'invalid closed Raft range')
  require(len(opens)==len(set(n for n,_ in opens)),'duplicate open Raft counter')
  manifest={}
  for line in (base/'stopped-member-before-writes.sha256').read_text().splitlines():
    fields=line.split()
    require(len(fields)==2 and re.fullmatch(r'[0-9a-f]{64}',fields[0]),
      'invalid stopped-volume hash manifest')
    manifest[fields[1]]=fields[0]
  total_open=0;empty_seen=False
  for _,name in sorted(opens):
    path='./'+name;copy=base/'stopped-open-segments'/name
    require(path in manifest and copy.is_file(),f'{name} lacks stable stopped-volume copy')
    digest=hashlib.sha256(copy.read_bytes()).hexdigest()
    require(digest==manifest[path],f'{name} copied bytes differ from stopped-volume manifest')
    count=open_count(name,sizes[path])
    require(not (empty_seen and count>0),f'{name} contains entries after an empty open segment')
    if count==0:empty_seen=True
    total_open+=count
    obs['open_segments'].append({'name':name,'sha256':digest,'bytes':sizes[path],'entries':count})
  closed_end=max(end for _,end in closed)
  upper=max(closed_end,max(snapshots))+total_open
  obs.update({'closed_segment_end':closed_end,'snapshot_index':max(snapshots),
    'open_entry_count':total_open,'last_persisted_index_upper_bound':upper})
  out.write_text(json.dumps(obs,indent=2)+'\n')
except (OSError,ValueError,KeyError,TypeError,struct.error) as exc:
  obs['measurement_error']=str(exc)
  out.write_text(json.dumps(obs,indent=2)+'\n')
  print(f'stopped Raft index: {exc}',file=sys.stderr)
  sys.exit(1)
PY
    }
    lc_storage_files() {
      lc_ns exec pod/lifecycle-storage -c storage -- sh -c \
        'cd /data && find . -type f -exec ls -ln {} \; | sort' >"$1"
    }
    # Snapshot phase helpers live in scripts/lifecycle-snapshot-phase.sh.
    # A probe failure still appends a sample and must not change the write rc.
    # The 1Gi cap is not raised here.

    # Snapshot catch-up: the stopped member misses at least 1,400 distinct
    # acknowledged catalog writes. If that does not exhaust dqlite's retained
    # trailing log, add at most eighteen 500-write updates to one catalog job.
    # Measure the actual leader files after every batch; never infer a pass
    # from the number of writes or a configured snapshot threshold.
    LC_SNAP_RC=0
    LC_SNAP_TRUNCATED=0
    LC_SNAP_REASON=""
    LC_SNAP_EVIDENCE=""
    # 15s between rounds, 40 rounds: finite inside the 12m write-phase timeout.
    LC_MEM_ACTIVE=0
    LC_MEM_BATCHES=""
    LC_MEM_APPLY_BATCH=""
    LC_MEM_SEQ=0
    LC_MEM_INTERVAL=15
    LC_MEM_SAMPLE_CAP=40
    LC_PHASE_PID=""
    LC_PHASE_DONE=""
    lc_scale_two || LC_SNAP_RC=$?
    if [[ "$LC_SNAP_RC" == 0 ]]; then
      lc_phase SnapshotLeader "$LC_CAND_ID" "$(lc_base)" || LC_SNAP_RC=$?
      cp "$LC_ART/cluster-logs/SnapshotLeader.log" "$LC_ART/cluster-logs/SnapshotLeader-before.log" || LC_SNAP_RC=$?
      lc_copy_runner_artifacts || LC_SNAP_RC=$?
      cp "$LC_ART/cluster-snapshot-leader.json" "$LC_ART/cluster-logs/leader-before.json" || LC_SNAP_RC=$?
      LC_LEADER="$(LC_ART="$LC_ART" python3 - <<'PY'
import json,os,pathlib
record=json.loads(pathlib.Path(os.environ['LC_ART'],'cluster-logs/leader-before.json').read_text())
assert record['name'] in ('caesium-0','caesium-1')
print(record['name'])
PY
)" || LC_SNAP_RC=$?
      if [[ "$LC_SNAP_RC" == 0 ]]; then
        lc_files "$LC_LEADER" "$LC_ART/cluster-logs/leader-before-snapshot-files.txt" || LC_SNAP_RC=$?
        if [[ "$LC_LEADER" == caesium-0 ]]; then LC_OTHER=caesium-1; else LC_OTHER=caesium-0; fi
      fi
    fi
    if [[ "$LC_SNAP_RC" == 0 ]]; then
      lc_storage_helper_start || LC_SNAP_RC=$?
      lc_manifest_local "$LC_ART/cluster-logs/stopped-member-before-writes.sha256" || LC_SNAP_RC=$?
      lc_storage_files "$LC_ART/cluster-logs/stopped-member-before-writes-files.txt" || LC_SNAP_RC=$?
      if [[ "$LC_SNAP_RC" == 0 ]]; then
        lc_stopped_raft_index >"$LC_ART/cluster-logs/stopped-member-raft-index.log" 2>&1 || LC_SNAP_RC=$?
        if [[ "$LC_SNAP_RC" != 0 ]]; then
          LC_SNAP_REASON="stopped member open Raft tail could not be bounded from verified local bytes"
        fi
      fi
      lc_storage_helper_stop || LC_SNAP_RC=$?
    fi
    if [[ "$LC_SNAP_RC" == 0 ]]; then
      for LC_BATCH in {0..18}; do
        printf -v LC_BATCH_TAG '%02d' "$LC_BATCH"
        lc_run_snapshot_phase "$LC_BATCH"
        if [[ "$LC_SNAP_RC" != 0 ]]; then
          LC_MEM_APPLY_BATCH="$LC_BATCH"
          lc_memory_sample_members apply-error "$LC_BATCH" 1 || true
          LC_SNAP_REASON="catalog write phase failed at batch $LC_BATCH_TAG"
          lc_capture_snapshot_write_failure "$LC_BATCH_TAG" || true
          LC_SNAP_EVIDENCE="$LC_ART/cluster-logs/snapshot-failure-batch-$LC_BATCH_TAG.json"
          break
        fi
        lc_phase SnapshotLeader "$LC_CAND_ID" "$(lc_base)" || LC_SNAP_RC=$?
        cp "$LC_ART/cluster-logs/SnapshotLeader.log" "$LC_ART/cluster-logs/SnapshotLeader-batch-$LC_BATCH_TAG.log" || LC_SNAP_RC=$?
        lc_copy_runner_artifacts || LC_SNAP_RC=$?
        cp "$LC_ART/cluster-snapshot-leader.json" "$LC_ART/cluster-logs/leader-after-batch-$LC_BATCH_TAG.json" || LC_SNAP_RC=$?
        if [[ "$LC_SNAP_RC" == 0 ]]; then
          lc_files "$LC_LEADER" "$LC_ART/cluster-logs/leader-after-batch-$LC_BATCH_TAG-snapshot-files.txt" || LC_SNAP_RC=$?
          lc_files "$LC_OTHER" "$LC_ART/cluster-logs/other-after-batch-$LC_BATCH_TAG-snapshot-files.txt" || LC_SNAP_RC=$?
        fi
        if [[ "$LC_SNAP_RC" != 0 ]]; then
          LC_SNAP_REASON="a survivor or its on-disk files became unobservable at batch $LC_BATCH_TAG"
          break
        fi
        LC_PROGRESS_RC=0
        lc_snapshot_progress "$LC_BATCH" >"$LC_ART/cluster-logs/snapshot-progress-batch-$LC_BATCH_TAG.log" 2>&1 || LC_PROGRESS_RC=$?
        LC_SNAP_EVIDENCE="$LC_ART/cluster-logs/snapshot-batch-$LC_BATCH_TAG.json"
        case "$LC_PROGRESS_RC" in
          0)
            LC_SNAP_TRUNCATED=1
            cp "$LC_ART/cluster-logs/leader-after-batch-$LC_BATCH_TAG.json" "$LC_ART/cluster-logs/leader-after.json" || LC_SNAP_RC=$?
            cp "$LC_ART/cluster-logs/leader-after-batch-$LC_BATCH_TAG-snapshot-files.txt" "$LC_ART/cluster-logs/leader-after-snapshot-files.txt" || LC_SNAP_RC=$?
            cp "$LC_ART/cluster-logs/other-after-batch-$LC_BATCH_TAG-snapshot-files.txt" "$LC_ART/cluster-logs/other-after-snapshot-files.txt" || LC_SNAP_RC=$?
            lc_snapshot_hashes "$LC_LEADER" "$LC_ART/cluster-logs/leader-before-rejoin-snapshots.sha256" || LC_SNAP_RC=$?
            break
            ;;
          2) ;;
          3) LC_SNAP_RC=1; LC_SNAP_REASON="leader on-disk bytes reached the 1536 MiB snapshot workload cap at batch $LC_BATCH_TAG"; break ;;
          *) LC_SNAP_RC=1; LC_SNAP_REASON="snapshot evidence or survivor identity invalid at batch $LC_BATCH_TAG"; break ;;
        esac
      done
      if [[ "$LC_SNAP_RC" == 0 && "$LC_SNAP_TRUNCATED" != 1 ]]; then
        LC_SNAP_RC=1
        LC_SNAP_REASON="both survivors did not truncate past stopped member within 1400 distinct plus 9000 update applies"
      fi
    fi
    if [[ "$LC_SNAP_RC" == 0 ]]; then
      lc_scale_three || LC_SNAP_RC=$?
      if [[ "$LC_SNAP_RC" == 0 ]]; then
        lc_phase PostStorage "$LC_CAND_ID" "$(lc_base)" || LC_SNAP_RC=$?
        lc_copy_runner_artifacts || true
      fi
    fi
    if [[ "$LC_SNAP_RC" == 0 ]]; then
      lc_files caesium-2 "$LC_ART/cluster-logs/rejoined-member-files.txt" || LC_SNAP_RC=$?
      lc_snapshot_hashes caesium-2 "$LC_ART/cluster-logs/rejoined-member-snapshots.sha256" || LC_SNAP_RC=$?
      lc_ns logs caesium-2 -c caesium >"$LC_ART/cluster-logs/rejoined-member.log" 2>&1 || LC_SNAP_RC=$?
    fi
    if [[ "$LC_SNAP_RC" != 0 ]]; then
      if [[ -n "$LC_SNAP_EVIDENCE" && ! -s "$LC_SNAP_EVIDENCE" ]]; then LC_SNAP_EVIDENCE=""; fi
      lc_snapshot_case blocked "${LC_SNAP_REASON:-stopped member or rejoin failed}; inspect cluster-logs/snapshot-progress-batch-*.log and phase logs" "$LC_SNAP_EVIDENCE"
    else
      LC_ART="$LC_ART" LC_SNAP_BATCH="$LC_BATCH" python3 - <<'PY' && LC_SNAP_MEASURED=1 || LC_SNAP_MEASURED=0
import json,pathlib,re,os
base=pathlib.Path(os.environ['LC_ART'],'cluster-logs')
batch=int(os.environ['LC_SNAP_BATCH'])
progress=[json.loads((base/f'snapshot-batch-{n:02d}.json').read_text()) for n in range(batch+1)]
if any(p.get('batch')!=n or p.get('acknowledged_total_applies')!=1400+500*n for n,p in enumerate(progress)):
  raise SystemExit('missing or non-monotonic per-batch acknowledgement evidence')
if not progress[-1].get('truncation_proved'):
  raise SystemExit('host did not measure leader truncation before rejoin')
def paths(name):
  return [line.split()[-1] for line in (base/name).read_text().splitlines() if line.split()]
def snapshots(name):
  out=[]
  for p in paths(name):
    m=re.search(r'(?:^|/)snapshot-(\d+)-(\d+)-(\d+)$',p)
    if m:out.append(int(m.group(2)))
  return out
def segments(name):
  out=[]
  for p in paths(name):
    m=re.search(r'(?:^|/)(\d+)-(\d+)$',p)
    if m:out.append((int(m.group(1)),int(m.group(2))))
  return out
def snapshot_hashes(name):
  out={}
  for line in (base/name).read_text().splitlines():
    fields=line.split()
    if len(fields)!=2 or not re.fullmatch(r'[0-9a-f]{64}',fields[0]):
      raise SystemExit(f'invalid snapshot hash row in {name}: {line!r}')
    m=re.fullmatch(r'(?:\./)?snapshot-\d+-(\d+)-\d+',fields[1])
    if not m:raise SystemExit(f'unexpected snapshot path in {name}: {fields[1]}')
    out.setdefault(int(m.group(1)),set()).add(fields[0])
  if not out:raise SystemExit(f'no snapshot bytes hashed in {name}')
  return out
before=snapshots('leader-before-snapshot-files.txt')
after=snapshots('leader-after-snapshot-files.txt')
stopped=segments('stopped-member-before-writes-files.txt')
stopped_snapshots=snapshots('stopped-member-before-writes-files.txt')
stopped_bound=json.loads((base/'stopped-member-raft-index.json').read_text())
leader_after_segments=segments('leader-after-snapshot-files.txt')
other_after_segments=segments('other-after-snapshot-files.txt')
other_after_snapshots=snapshots('other-after-snapshot-files.txt')
rejoined=snapshots('rejoined-member-files.txt')
leader_hashes=snapshot_hashes('leader-before-rejoin-snapshots.sha256')
rejoined_hashes=snapshot_hashes('rejoined-member-snapshots.sha256')
obs={'leader_before':json.loads((base/'leader-before.json').read_text()),
     'leader_after':json.loads((base/'leader-after.json').read_text()),
     'acknowledged_total_applies':progress[-1]['acknowledged_total_applies'],
     'batch_evidence':[f'snapshot-batch-{n:02d}.json' for n in range(batch+1)],
     'leader_file_bytes':progress[-1]['leader_file_bytes'],
     'leader_before_snapshot_indexes':before,'leader_after_snapshot_indexes':after,
     'stopped_segment_ranges':stopped,'stopped_snapshot_indexes':stopped_snapshots,
     'stopped_member_index_bound':stopped_bound,
     'leader_after_segment_ranges':leader_after_segments,
     'other_survivor':progress[-1]['other_survivor'],
     'other_after_segment_ranges':other_after_segments,
     'other_after_snapshot_indexes':other_after_snapshots,
     'rejoined_snapshot_indexes':rejoined,
     'leader_snapshot_hashes':{str(k):sorted(v) for k,v in leader_hashes.items()},
     'rejoined_snapshot_hashes':{str(k):sorted(v) for k,v in rejoined_hashes.items()}}
(base/'snapshot-threshold.json').write_text(json.dumps(obs,indent=2)+'\n')
if not after or not stopped or not leader_after_segments or not other_after_segments or not other_after_snapshots or not rejoined:
  raise SystemExit('missing snapshot or segment index')
if obs['leader_after']!=progress[-1]['leader_after']:
  raise SystemExit('final leader identity differs from measured pre-rejoin leader')
stopped_end=stopped_bound.get('last_persisted_index_upper_bound')
if not isinstance(stopped_end,int) or stopped_end<max(end for _,end in stopped):
  raise SystemExit('stopped member open tail upper bound is missing or invalid')
if max(after)<=max(before or [0]) or max(after)<=stopped_end:
  raise SystemExit('leader snapshot did not cross stopped member index')
if min(start for start,_ in leader_after_segments)<=stopped_end+1:
  raise SystemExit('leader still has log segments that could serve stopped member without snapshot')
if max(other_after_snapshots)<=stopped_end or min(start for start,_ in other_after_segments)<=stopped_end+1:
  raise SystemExit('other survivor still has log segments that could serve stopped member without snapshot')
if max(rejoined)<=stopped_end or max(rejoined)<=max(stopped_snapshots or [0]):
  raise SystemExit('rejoined member has no new local snapshot beyond its stopped index bound')
# Dqlite retains only recent snapshots. A transferred snapshot can be pruned
# before this post-rejoin observation, so matching bytes corroborate the
# installation but cannot be required. Neither survivor kept the stopped
# member's next log entry, making snapshot installation necessary for rejoin.
shared=[{'index':index,'sha256':digest} for index in leader_hashes.keys() & rejoined_hashes.keys()
        for digest in leader_hashes[index] & rejoined_hashes[index]
        if index>stopped_end and index in after and index in rejoined]
obs['matched_transferred_snapshots']=sorted(shared,key=lambda item:item['index'])
common_indexes=(leader_hashes.keys() & rejoined_hashes.keys() & set(after) & set(rejoined))
if any(not leader_hashes[index] & rejoined_hashes[index] for index in common_indexes if index>stopped_end):
  raise SystemExit('same-index leader and rejoined snapshot bytes differ')
log=(base/'rejoined-member.log').read_text()
obs['install_snapshot_log_lines']=[line for line in log.splitlines()
  if re.search(r'install.?snapshot|snapshot[^\n]*install',line,re.I)][:20]
obs['snapshot_install_inferred_from_two_survivor_log_gap']=True
(base/'snapshot-threshold.json').write_text(json.dumps(obs,indent=2)+'\n')
PY
      if [[ "$LC_SNAP_MEASURED" == 1 ]]; then
        lc_snapshot_case pass "both survivors truncated beyond stopped open-tail bound after $((1400 + 500 * LC_BATCH)) acknowledged applies; rejoined member retained a newer local snapshot and durable state" "$LC_ART/cluster-logs/snapshot-threshold.json"
      else
        LC_FINAL_EVIDENCE=""
        if [[ -s "$LC_ART/cluster-logs/snapshot-threshold.json" ]]; then
          LC_FINAL_EVIDENCE="$LC_ART/cluster-logs/snapshot-threshold.json"
        fi
        lc_snapshot_case blocked "snapshot/segment bytes did not prove both survivor log gaps and stopped-member snapshot catch-up; inspect cluster-logs/snapshot-threshold.json" "$LC_FINAL_EVIDENCE"
        LC_SNAP_RC=1
      fi
    fi

    # Storage-level restore: a stopped member's PVC is copied, erased,
    # compared without the copy (negative control), restored and compared
    # before the server restarts. Every digest comes from the local helper
    # mount, so healthy peers cannot supply an answer.
    if [[ "$LC_SNAP_RC" != 0 ]]; then
      for name in storage-snapshot-restore joining-ordinal-1-replacement ordinal-0-disk-loss rollback-recorded-outcome; do
        lc_case "$name" blocked "prior snapshot fault did not rejoin/reconcile; shared cluster is not a valid baseline for another destructive case"
      done
    else
    LC_RESTORE_RC=0
    lc_scale_two || LC_RESTORE_RC=$?
    if [[ "$LC_RESTORE_RC" == 0 ]]; then lc_storage_helper_start || LC_RESTORE_RC=$?; fi
    if [[ "$LC_RESTORE_RC" == 0 ]]; then
      lc_manifest_local "$LC_ART/cluster-logs/snapshot-copy.sha256" || LC_RESTORE_RC=$?
      lc_storage_files "$LC_ART/cluster-logs/snapshot-copy-files.txt" || LC_RESTORE_RC=$?
      lc_ns exec pod/lifecycle-storage -c storage -- cat /data/info.yaml \
        >"$LC_ART/cluster-logs/snapshot-copy-info.yaml" || LC_RESTORE_RC=$?
      lc_ns exec pod/lifecycle-storage -c storage -- tar -cf - -C /data . \
        >"$LC_ART/cluster-logs/snapshot-copy.tar" || LC_RESTORE_RC=$?
      [[ -s "$LC_ART/cluster-logs/snapshot-copy.sha256" && -s "$LC_ART/cluster-logs/snapshot-copy.tar" ]] || LC_RESTORE_RC=1
    fi
    if [[ "$LC_RESTORE_RC" == 0 ]]; then
      lc_ns exec pod/lifecycle-storage -c storage -- sh -c \
        'test -f /data/info.yaml && cd /data && find . -mindepth 1 -maxdepth 1 -exec rm -rf {} \;' \
        || LC_RESTORE_RC=$?
      lc_manifest_local "$LC_ART/cluster-logs/negative-control.sha256" || LC_RESTORE_RC=$?
      if cmp -s "$LC_ART/cluster-logs/snapshot-copy.sha256" "$LC_ART/cluster-logs/negative-control.sha256"; then
        LC_RESTORE_RC=1
      fi
      lc_ns exec -i pod/lifecycle-storage -c storage -- tar -xpf - -C /data \
        <"$LC_ART/cluster-logs/snapshot-copy.tar" || LC_RESTORE_RC=$?
      lc_manifest_local "$LC_ART/cluster-logs/pre-start-restored.sha256" || LC_RESTORE_RC=$?
      cmp -s "$LC_ART/cluster-logs/snapshot-copy.sha256" "$LC_ART/cluster-logs/pre-start-restored.sha256" || LC_RESTORE_RC=1
      lc_storage_files "$LC_ART/cluster-logs/pre-start-restored-files.txt" || LC_RESTORE_RC=$?
      cmp -s "$LC_ART/cluster-logs/snapshot-copy-files.txt" "$LC_ART/cluster-logs/pre-start-restored-files.txt" || LC_RESTORE_RC=1
      lc_ns exec pod/lifecycle-storage -c storage -- cat /data/info.yaml \
        >"$LC_ART/cluster-logs/pre-start-restored-info.yaml" || LC_RESTORE_RC=$?
      cmp -s "$LC_ART/cluster-logs/snapshot-copy-info.yaml" "$LC_ART/cluster-logs/pre-start-restored-info.yaml" || LC_RESTORE_RC=1
    fi
    if [[ "$LC_RESTORE_RC" == 0 ]]; then
      lc_scale_three || LC_RESTORE_RC=$?
    else
      # A failed erase/extract/checksum leaves the member stopped. Starting it
      # could silently repair from healthy peers and falsely certify a copy.
      lc_storage_helper_stop || LC_RESTORE_RC=1
    fi
    if [[ "$LC_RESTORE_RC" == 0 ]]; then
      lc_phase PostStorage "$LC_CAND_ID" "$(lc_base)" || LC_RESTORE_RC=$?
      lc_copy_runner_artifacts || true
    fi
    if [[ "$LC_RESTORE_RC" == 0 ]]; then
      LC_ART="$LC_ART" python3 - <<'PY'
import hashlib,json,os,pathlib
base=pathlib.Path(os.environ['LC_ART'],'cluster-logs')
copy=(base/'snapshot-copy.sha256').read_text().splitlines()
restored=(base/'pre-start-restored.sha256').read_text().splitlines()
negative=(base/'negative-control.sha256').read_text().splitlines()
copy_files=(base/'snapshot-copy-files.txt').read_text().splitlines()
restored_files=(base/'pre-start-restored-files.txt').read_text().splitlines()
info=(base/'pre-start-restored-info.yaml').read_text()
copy_info=(base/'snapshot-copy-info.yaml').read_text()
tar=(base/'snapshot-copy.tar').read_bytes()
record={'stopped_member_copy_manifest':copy,'pre_start_restored_manifest':restored,
  'stopped_member_file_sizes':copy_files,'pre_start_restored_file_sizes':restored_files,
  'negative_control_manifest':negative,'negative_control_failed':negative!=copy,
  'local_content_assertion':copy==restored and copy_files==restored_files and len(copy)>0,
  'snapshot_tar_sha256':hashlib.sha256(tar).hexdigest(),
  'copied_info_yaml':copy_info,'restored_info_yaml':info,
  'info_identity_unchanged':copy_info==info,'member_stopped_during_checks':True}
(base/'restore-evidence.json').write_text(json.dumps(record,indent=2)+'\n')
PY
      lc_case storage-snapshot-restore pass "stopped-member copy and restored per-file SHA-256 matched before startup; omitted-copy negative control differed; local PVC bytes inspected without peers" "$LC_ART/cluster-logs/restore-evidence.json"
    else
      lc_case storage-snapshot-restore blocked "consistent copy, pre-start SHA-256 comparison, local-only read, negative control or rejoin failed; inspect cluster-logs/*sha256 and pre-start-restored-info.yaml"
    fi

    # Fresh-PVC joining member: PVC deletion is requested while the old pod
    # still holds it, then pod deletion releases the protection finalizer. The
    # StatefulSet must create a new claim/PV; UID and PV identity are checked
    # by the live runner before direct Cluster RPCs are accepted.
    if [[ "$LC_RESTORE_RC" != 0 ]]; then
      for name in joining-ordinal-1-replacement ordinal-0-disk-loss rollback-recorded-outcome; do
        lc_case "$name" blocked "prior restore fault did not rejoin/reconcile; shared cluster is not a valid baseline for another destructive case"
      done
    else
    LC_JOIN_RC=0
    LC_OLD1_UID="$(lc_ns get pod caesium-1 -o jsonpath='{.metadata.uid}')"
    lc_ns delete pvc data-caesium-1 --wait=false >/dev/null || LC_JOIN_RC=$?
    lc_ns delete pod caesium-1 --wait=false >/dev/null || LC_JOIN_RC=$?
    for _ in {1..120}; do
      LC_NEW1_UID="$(lc_ns get pod caesium-1 -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      if [[ -n "$LC_NEW1_UID" && "$LC_NEW1_UID" != "$LC_OLD1_UID" ]]; then break; fi
      sleep 2
    done
    [[ -n "${LC_NEW1_UID:-}" && "$LC_NEW1_UID" != "$LC_OLD1_UID" ]] || LC_JOIN_RC=1
    lc_ns wait --for=condition=Ready pod/caesium-1 --timeout=300s >/dev/null 2>&1 || LC_JOIN_RC=$?
    if [[ "$LC_JOIN_RC" == 0 ]]; then
      lc_phase JoiningOrdinalOne "$LC_CAND_ID" "$(lc_base)" || LC_JOIN_RC=$?
    fi
    lc_copy_runner_artifacts || true
    if [[ "$LC_JOIN_RC" != 0 ]]; then
      lc_case joining-ordinal-1-replacement blocked "fresh-PVC ordinal-1 pod or direct membership proof failed; inspect JoiningOrdinalOne.log, PVC/PV and pod events"
    fi

    # Ordinal 0 is intentionally last: replacing its PVC can create an
    # isolated self-bootstrap, so it must not contaminate the preceding cases.
    if [[ "$LC_JOIN_RC" != 0 ]]; then
      lc_case ordinal-0-disk-loss blocked "prior joining replacement did not reconcile; shared cluster is not a valid baseline for ordinal-0 disk loss"
      lc_case rollback-recorded-outcome blocked "prior joining replacement did not reconcile; no isolated migrated volume copy exists for rollback"
    else
    LC_ZERO_RC=0
    LC_OLD0_UID="$(lc_ns get pod caesium-0 -o jsonpath='{.metadata.uid}')"
    lc_ns delete pvc data-caesium-0 --wait=false >/dev/null || LC_ZERO_RC=$?
    lc_ns delete pod caesium-0 --wait=false >/dev/null || LC_ZERO_RC=$?
    for _ in {1..120}; do
      LC_NEW0_UID="$(lc_ns get pod caesium-0 -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      if [[ -n "$LC_NEW0_UID" && "$LC_NEW0_UID" != "$LC_OLD0_UID" ]]; then break; fi
      sleep 2
    done
    [[ -n "${LC_NEW0_UID:-}" && "$LC_NEW0_UID" != "$LC_OLD0_UID" ]] || LC_ZERO_RC=1
    # WaitForFirstConsumer keeps the new PVC Pending until this pod is
    # scheduled. A UID change alone is that Pending pod, which has no IP and
    # no volumeName. Ordinal-1 already waits for Ready; do the same here
    # before reading the store or choosing an HTTP base.
    if [[ "$LC_ZERO_RC" == 0 ]]; then
      lc_ns wait --for=condition=Ready pod/caesium-0 --timeout=300s >/dev/null 2>&1 || LC_ZERO_RC=$?
    fi
    lc_ns get pod caesium-0 -o json >"$LC_ART/cluster-logs/ordinal0-pod.json" 2>&1 || true
    lc_ns get pvc data-caesium-0 -o json >"$LC_ART/cluster-logs/ordinal0-pvc.json" 2>&1 || true
    if [[ "$LC_ZERO_RC" == 0 ]]; then
      lc_ns logs caesium-0 -c caesium --tail=-1 >"$LC_ART/cluster-logs/ordinal0.log" 2>&1 || true
      lc_ns exec caesium-0 -c caesium -- cat /var/lib/caesium/dqlite/info.yaml \
        >"$LC_ART/cluster-logs/ordinal0-info.yaml" 2>&1 || true
      lc_ns exec caesium-0 -c caesium -- sh -c \
        'cd /var/lib/caesium/dqlite && find . -type f -exec ls -ln {} \; | sort' \
        >"$LC_ART/cluster-logs/ordinal0-node-store.txt" 2>&1 || true
      LC_ART="$LC_ART" LC_OLD0_UID="$LC_OLD0_UID" LC_NEW0_UID="${LC_NEW0_UID:-}" python3 - <<'PY'
import json,os,pathlib
art=pathlib.Path(os.environ['LC_ART']);out={
 'old_uid':os.environ['LC_OLD0_UID'],'new_uid':os.environ['LC_NEW0_UID'],
 'pod':(art/'cluster-logs/ordinal0-pod.json').read_text(),
 'pvc':(art/'cluster-logs/ordinal0-pvc.json').read_text(),
 'info_yaml':(art/'cluster-logs/ordinal0-info.yaml').read_text(),
 'node_store':(art/'cluster-logs/ordinal0-node-store.txt').read_text(),
 'logs':(art/'cluster-logs/ordinal0.log').read_text()}
(art/'cluster-ordinal0-host.json').write_text(json.dumps(out,indent=2)+'\n')
PY
      lc_ns cp "$LC_ART/cluster-ordinal0-host.json" lifecycle-runner:/artifacts/cluster-ordinal0-host.json -c runner || LC_ZERO_RC=$?
      lc_phase OrdinalZeroLoss "$LC_CAND_ID" "$(lc_base)" || LC_ZERO_RC=$?
      lc_copy_runner_artifacts || true
    fi
    if [[ "$LC_ZERO_RC" != 0 && ! -f "$LC_ART/cases/ordinal-0-disk-loss.json" ]]; then
      lc_case ordinal-0-disk-loss blocked "ordinal-0 replacement did not become Ready with a readable store before the runner; inspect ordinal0-pod.json and ordinal0-pvc.json"
    fi
    lc_case rollback-recorded-outcome blocked "exploratory helm rollback requires an isolated copy of the candidate-migrated three-member volume set; the ordinal-0 disk-loss cluster is not a valid rollback baseline"
    fi
    fi
    fi
  fi
  LC_COPY_RC=0
  lc_copy_runner_artifacts || LC_COPY_RC=$?
  LC_ART="$LC_ART" LC_ID="$LC_ID" LC_SHA="$LC_SHA" LC_PAIR="$LC_PAIR" LC_STARTED="$LC_STARTED" \
    LC_PREV="$LC_PREV" LC_PREV_ID="$LC_PREV_ID" LC_CAND="$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE" \
    LC_CAND_ID="$LC_CAND_ID" LC_CAND_SOURCE_ID="$LC_CAND_SOURCE_ID" \
    LC_PROVENANCE="$LC_PROVENANCE" LC_HELM_RC="$LC_HELM_RC" \
    LC_MIXED_RC="$LC_MIXED_RC" LC_AFTER_RC="$LC_AFTER_RC" LC_GET_RC="$LC_GET_RC" \
    LC_INFO_RC="$LC_INFO_RC" LC_ADDRESS_BLOCKED="$LC_ADDRESS_BLOCKED" LC_COPY_RC="$LC_COPY_RC" python3 - <<'PY'
import datetime,json,os,pathlib
art=pathlib.Path(os.environ['LC_ART']);doc=json.loads((art/'versions.json').read_text())
p=next(x for x in doc['pairs'] if x['id']==os.environ['LC_PAIR']);expected=p['cluster']['required_cases']
def read_cases(directory):
  out={}
  for path in sorted((art/directory).glob('*.json')):
    try:r=json.loads(path.read_text())
    except Exception as e:r={'name':path.stem,'status':'blocked','detail':f'unreadable record: {e}'}
    if r.get('lifecycle_id')!=os.environ['LC_ID']:r=dict(r,status='blocked',detail='foreign lifecycle_id')
    out[r['name']]=r
  return out
# A runner assertion is the source of truth for its own case. Host-only fault
# cases fill gaps, and missing runner evidence remains blocked by default.
runner_cases=read_cases('cases')
host_cases=read_cases('cluster-cases')
by={**host_cases,**runner_cases}
for name in expected:
  by.setdefault(name,{'name':name,'status':'blocked','lifecycle_id':os.environ['LC_ID'],
    'detail':'required case produced no record'})
gates={'mixed_window_exit':int(os.environ['LC_MIXED_RC']),
  'after_upgrade_exit':int(os.environ['LC_AFTER_RC']),
  'helm_upgrade_exit':int(os.environ['LC_HELM_RC']),
  'live_manifest_exit':int(os.environ['LC_GET_RC']),
  'post_upgrade_info_exit':int(os.environ['LC_INFO_RC']),
  'address_prerequisite_blocked':int(os.environ['LC_ADDRESS_BLOCKED']),
  'final_artifact_copy_exit':int(os.environ['LC_COPY_RC'])}
if gates['mixed_window_exit']!=0 and by['mixed-version-dispatch-and-completion']['status']=='pass':
  by['mixed-version-dispatch-and-completion']=dict(by['mixed-version-dispatch-and-completion'],status='blocked',
    detail='mixed-window process failed despite a runner pass record')
# The runner verifies installed manifest, addresses, pod state and direct Raft
# membership before writing an upgrade pass. A Helm timeout or a later
# retained-history assertion does not erase that already-observed result.
# Preserve Helm's exit code as an observation; a wait timeout alone is not a
# failed gate once the runner has proved the upgraded three-voter state.
cases=[by[n] for n in expected]
failed=[r['name'] for r in cases if not (r['status']=='pass' or
  (r['name']=='rollback-recorded-outcome' and r['status']=='recorded-outcome' and r.get('observations')))]
failed_gates=[name for name,rc in gates.items() if rc!=0 and not (
  name=='helm_upgrade_exit' and by['rolling-upgrade-three-voters']['status']=='pass')]
record={'kind':'caesium-cluster-lifecycle-qualification','schema_version':1,
  'lifecycle_id':os.environ['LC_ID'],'pair':os.environ['LC_PAIR'],
  'candidate_sha':os.environ['LC_SHA'],'started_at':os.environ['LC_STARTED'],
  'finished_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),
  'topology':{'replicas':3,'persistent':True,'database_shards':1},
  'previous_image':{'ref':os.environ['LC_PREV'],'image_id':os.environ['LC_PREV_ID']},
  'candidate_image':{'ref':os.environ['LC_CAND'],
    'image_id':os.environ['LC_CAND_SOURCE_ID'],
    'source_docker_image_id':os.environ['LC_CAND_SOURCE_ID'],
    'verified_pod_image_ids':os.environ['LC_CAND_ID'].split(','),
    'archive_proof':'candidate-image-archive.json',
    'owned_node_imports':'candidate-image-node-imports.json',
    'provenance':os.environ['LC_PROVENANCE']},
  'helm_exit_code':int(os.environ['LC_HELM_RC']),
  'phase_exit_codes':gates,
  'expected_cases':expected,'cases':cases,'failed_cases':failed,'failed_gates':failed_gates,
  'result':'pass' if not failed and not failed_gates and os.environ['LC_PROVENANCE']=='built-by-this-run' else 'fail'}
(art/'cluster-qualification.json').write_text(json.dumps(record,indent=2)+'\n')
print('cluster lifecycle:',record['result'],'failed/blocked:',failed)
PY
  if LC_ART="$LC_ART" python3 - <<'PY'; then
import json,os,pathlib
record=json.loads(pathlib.Path(os.environ['LC_ART'],'cluster-qualification.json').read_text())
raise SystemExit(0 if record['result']=='pass' else 1)
PY
    exit 0
  fi
  cluster_die "F2 qualification incomplete; inspect $LC_ART/cluster-qualification.json for per-case evidence"
fi

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
require_env() { [[ -n "${!1:-}" ]] || die "$1 is required"; }

require_cmd docker
require_cmd python3

require_env CAESIUM_LIFECYCLE_ID
require_env CAESIUM_LIFECYCLE_ARTIFACTS
require_env CAESIUM_LIFECYCLE_PREV_IMAGE
require_env CAESIUM_LIFECYCLE_CANDIDATE_IMAGE

ID="$CAESIUM_LIFECYCLE_ID"
if [[ ! "$ID" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]]; then
  die "CAESIUM_LIFECYCLE_ID must be a lowercase DNS-1123 name of at most 40 characters, got '$ID'"
fi
# Must match suffixOf() in test/lifecycle/standalone_test.go: the fixture job
# aliases and the marker in every task container's command derive from it.
SUFFIX="$(printf '%s' "$ID" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9' | tail -c 12)"
[[ -n "$SUFFIX" ]] || die "CAESIUM_LIFECYCLE_ID has no alphanumeric characters"

PREV_IMAGE="$CAESIUM_LIFECYCLE_PREV_IMAGE"
CANDIDATE_IMAGE="$CAESIUM_LIFECYCLE_CANDIDATE_IMAGE"
case "$PREV_IMAGE" in
  *:latest|*:latest@*) die "the 'latest' tag is not a release (last pushed 2021, amd64-only); pin a released tag" ;;
  *:*) ;;
  *) die "CAESIUM_LIFECYCLE_PREV_IMAGE must be tagged, got '$PREV_IMAGE'" ;;
esac

CANDIDATE_SHA="${CANDIDATE_SHA:-${CANDIDATE_IMAGE##*:}}"
[[ -n "$CANDIDATE_SHA" && "$CANDIDATE_SHA" != "$CANDIDATE_IMAGE" ]] \
  || die "CANDIDATE_SHA is required (export it, or use caesiumcloud/caesium:<sha> as CAESIUM_LIFECYCLE_CANDIDATE_IMAGE)"

PAIR="${CAESIUM_LIFECYCLE_PAIR:-v0.1.0-to-candidate}"
MANUAL_KEY="${CAESIUM_LIFECYCLE_MANUAL_KEY:-caesium-lifecycle-manual-key}"
SOCK="${CAESIUM_SOCK:-/var/run/docker.sock}"
[[ -S "$SOCK" ]] || die "docker socket $SOCK is not a socket; the fixture jobs cannot launch task containers"

case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH=amd64 ;;
  aarch64|arm64) HOST_ARCH=arm64 ;;
  *) HOST_ARCH="$(uname -m)" ;;
esac
PLATFORM="${CAESIUM_PLATFORM:-linux/$HOST_ARCH}"
TASK_IMAGE="${CAESIUM_LIFECYCLE_TASK_IMAGE:-alpine:3.23}"

mkdir -p "$CAESIUM_LIFECYCLE_ARTIFACTS"
ARTIFACTS="$(cd "$CAESIUM_LIFECYCLE_ARTIFACTS" && pwd)"
export CAESIUM_LIFECYCLE_ARTIFACTS="$ARTIFACTS"

# Records from an earlier invocation are not evidence for this one. Purge the
# directories this script owns before writing anything, so a phase that never
# runs cannot inherit a passing case file. (Every surviving record is ALSO
# checked against this invocation's lifecycle id when the record is assembled.)
rm -rf "$ARTIFACTS/cases" "$ARTIFACTS/observations" "$ARTIFACTS/logs" "$ARTIFACTS/cli" \
       "$ARTIFACTS/qualification.json" "$ARTIFACTS/fixture.json" "$ARTIFACTS/lifecycle.test"
mkdir -p "$ARTIFACTS/cases" "$ARTIFACTS/observations" "$ARTIFACTS/logs" "$ARTIFACTS/cli"
cp "$ROOT/test/lifecycle/versions.json" "$ARTIFACTS/versions.json"

# An aborted run must leave a NON-passing record behind, never nothing (which a
# consumer could confuse with an older passing file) and never a stale pass.
PLACEHOLDER_ID="$ID" PLACEHOLDER_PAIR="$PAIR" PLACEHOLDER_SHA="$CANDIDATE_SHA" \
PLACEHOLDER_DEST="$ARTIFACTS/qualification.json" python3 - <<'PY'
import datetime, json, os, pathlib
pathlib.Path(os.environ["PLACEHOLDER_DEST"]).write_text(json.dumps({
    "schema_version": 1,
    "kind": "caesium-lifecycle-qualification",
    "pair": os.environ["PLACEHOLDER_PAIR"],
    "candidate_sha": os.environ["PLACEHOLDER_SHA"],
    "lifecycle_id": os.environ["PLACEHOLDER_ID"],
    "started_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "result": "incomplete",
    "detail": "scripts/lifecycle-tests.sh started but did not finish; this placeholder is "
              "overwritten only when the full record is assembled",
}, indent=2) + "\n")
PY

NET="${ID}-net"
ISO_NET="${ID}-iso-net"
VOL_DATA="${ID}-data"
VOL_ISO="${ID}-iso-data"
VOL_FAILTX="${ID}-failtx"
VOL_ROLLBACK="${ID}-rollback"
VOL_SHARDS="${ID}-shards"
CTR_PREV="${ID}-prev"
CTR_ISO="${ID}-iso"
CTR_CAND="${ID}-cand"
CTR_READDR="${ID}-readdr"
CTR_FAILTX="${ID}-failtx"
CTR_ROLLBACK="${ID}-rollback"
CTR_SHARDS="${ID}-shards"
SERVER_UID="${CAESIUM_LIFECYCLE_SERVER_USER:-10001:10001}"
START_EPOCH="$(date -u +%s)"

# Ownership token for THIS invocation.
#
# The fixture's task containers are launched by the server, so we cannot label
# them; the only handle is a marker in their command. The lifecycle id alone is
# not a safe handle: SUFFIX is the last 12 characters of the sanitized id, so
# two ids can share it, and a substring match would let "abc" claim "abcdef"'s
# live containers. The nonce makes the token unique per invocation, and cleanup
# matches it with a delimiter-anchored pattern (see TASK_MARKER_PATTERN) rather
# than `grep -F`, so neither a shared suffix nor a prefix can collide.
NONCE="$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
[[ -n "$NONCE" ]] || die "could not mint an ownership nonce"
OWNER_TOKEN="${SUFFIX}-${NONCE}"
TASK_MARKER="caesium-lc-owner=${OWNER_TOKEN}"
TASK_MARKER_PATTERN="(^|[^A-Za-z0-9_=-])${TASK_MARKER}([^A-Za-z0-9_=-]|\$)"
# Set to 1 only once every name this run claims has been checked and created.
# Until then cleanup must not sweep ANY task container: a run that aborts in
# refuse_existing has claimed nothing, and the containers carrying a similar
# marker belong to the invocation that is still using them.
OWNERSHIP_ESTABLISHED=0

# --------------------------------------------------------------------------
# Version matrix
# --------------------------------------------------------------------------
matrix_field() {
  CAESIUM_LIFECYCLE_MATRIX_PAIR="$PAIR" python3 - "$ARTIFACTS/versions.json" "$1" <<'PY'
import json, os, sys
path, expr = sys.argv[1], sys.argv[2]
doc = json.load(open(path))
want = os.environ["CAESIUM_LIFECYCLE_MATRIX_PAIR"]
pair = next((p for p in doc["pairs"] if p["id"] == want), None)
if pair is None:
    raise SystemExit(f"versions.json has no pair {want!r}")
cur = pair
for part in expr.split("."):
    cur = cur[part]
if isinstance(cur, (dict, list)):
    print(json.dumps(cur))
else:
    print(cur)
PY
}

PINNED_PREV_IMAGE="$(matrix_field previous.image)"
[[ "$PREV_IMAGE" == "$PINNED_PREV_IMAGE" ]] \
  || die "CAESIUM_LIFECYCLE_PREV_IMAGE ($PREV_IMAGE) is not the pinned image for pair $PAIR ($PINNED_PREV_IMAGE)"
PREV_RELEASE="$(matrix_field previous.release)"
SHARDS="$(matrix_field standalone.database_shards)"
DATA_DIR="$(matrix_field standalone.data_directory)"
NODE_ADDRESS="$(matrix_field standalone.node_address)"
ALT_NODE_ADDRESS="$(matrix_field standalone.alternate_node_address)"
ALT_SHARDS="$(matrix_field 'standalone.transitions' | python3 -c 'import json,sys; print(next(t["shards"] for t in json.load(sys.stdin) if t["id"]=="shard-count-change"))')"

# --------------------------------------------------------------------------
# Teardown: only resources carrying $ID.
# --------------------------------------------------------------------------
OWNED_CONTAINERS=()
OWNED_VOLUMES=()
OWNED_NETWORKS=()

own_container() { OWNED_CONTAINERS+=("$1"); }
own_volume() { OWNED_VOLUMES+=("$1"); }
own_network() { OWNED_NETWORKS+=("$1"); }

cleanup() {
  local status=$?
  set +e
  if [[ "${CAESIUM_LIFECYCLE_KEEP:-}" == "1" ]]; then
    log "CAESIUM_LIFECYCLE_KEEP=1; leaving owned resources in place"
    exit "$status"
  fi
  # Task containers the fixture jobs launched carry THIS invocation's ownership
  # token in their command. Sweep them only once this invocation actually owns
  # its named resources — otherwise a refused start (an id that is already
  # active) would kill the live containers of the invocation that owns it.
  if [[ "$OWNERSHIP_ESTABLISHED" == "1" ]]; then
    local stragglers
    stragglers="$(docker ps -a --no-trunc --format '{{.ID}}\t{{.Command}}' 2>/dev/null \
      | grep -E "$TASK_MARKER_PATTERN" | cut -f1)"
    if [[ -n "$stragglers" ]]; then
      log "cleanup: removing $(printf '%s\n' "$stragglers" | wc -l | tr -d ' ') task container(s) owned by $OWNER_TOKEN"
      # shellcheck disable=SC2086
      docker rm -f $stragglers >/dev/null 2>&1
    fi
  else
    log "cleanup: this invocation never established ownership; leaving every task container alone"
  fi
  local name
  for name in "${OWNED_CONTAINERS[@]:-}"; do
    [[ -n "$name" ]] || continue
    # Defense in depth against the same race start_server's ordering guards
    # against: only remove a named server container if it still carries THIS
    # invocation's ownership label (or the label cannot be read at all,
    # meaning the container is already gone). A container that raced us for
    # the name belongs to whichever invocation's `docker run` actually
    # created it, and carries that invocation's own label instead.
    local owner_label
    owner_label="$(docker inspect "$name" --format '{{index .Config.Labels "caesium-lc-owner"}}' 2>/dev/null || true)"
    if [[ -z "$owner_label" || "$owner_label" == "$OWNER_TOKEN" ]]; then
      docker rm -f "$name" >/dev/null 2>&1
    else
      log "cleanup: $name is labeled for owner '$owner_label', not ours ($OWNER_TOKEN); leaving it alone"
    fi
  done
  for name in "${OWNED_VOLUMES[@]:-}"; do
    [[ -n "$name" ]] || continue
    # Same defense in depth as the container loop above: create_owned_volume
    # only ever puts a name in OWNED_VOLUMES after confirming the label, but
    # verify again here rather than trusting that held.
    local vol_owner_label
    vol_owner_label="$(docker volume inspect "$name" --format '{{index .Labels "caesium-lc-owner"}}' 2>/dev/null || true)"
    if [[ -z "$vol_owner_label" || "$vol_owner_label" == "$OWNER_TOKEN" ]]; then
      docker volume rm -f "$name" >/dev/null 2>&1
    else
      log "cleanup: volume $name is labeled for owner '$vol_owner_label', not ours ($OWNER_TOKEN); leaving it alone"
    fi
  done
  for name in "${OWNED_NETWORKS[@]:-}"; do
    [[ -n "$name" ]] && docker network rm "$name" >/dev/null 2>&1
  done
  exit "$status"
}
trap cleanup EXIT INT TERM

refuse_existing() {
  local kind="$1" name="$2" existing
  case "$kind" in
    container) existing="$(docker ps -a --format '{{.Names}}' | grep -Fx "$name" || true)" ;;
    volume) existing="$(docker volume ls --format '{{.Name}}' | grep -Fx "$name" || true)" ;;
    network) existing="$(docker network ls --format '{{.Name}}' | grep -Fx "$name" || true)" ;;
  esac
  [[ -z "$existing" ]] || die "$kind $name already exists; refusing to claim or delete it"
}

# create_owned_volume claims a named volume for THIS invocation, the same way
# start_server claims a named container: `docker volume create` is idempotent
# — it succeeds on an EXISTING volume without creating anything and without
# signaling that it adopted rather than created — so a name collision after
# the `refuse_existing` preflight (another invocation's `docker volume create`
# won the name first) would otherwise be silently adopted, mounted writable
# and later deleted out from under the invocation that actually owns it. The
# ownership label can only be set at creation time, so it is the one thing
# that tells the two cases apart: create with THIS invocation's label, then
# read it back and refuse (without registering ownership, mounting or
# removing anything) unless it matches exactly.
create_owned_volume() {
  local name="$1" label
  docker volume create --label "$TASK_MARKER" "$name" >/dev/null
  label="$(docker volume inspect "$name" --format '{{index .Labels "caesium-lc-owner"}}' 2>/dev/null || true)"
  [[ "$label" == "$OWNER_TOKEN" ]] \
    || die "volume $name already exists and is not labeled for this invocation (label='${label:-<none>}', expected '$OWNER_TOKEN'); refusing to adopt, mount or delete it"
  own_volume "$name"
}

# --------------------------------------------------------------------------
# Claim every name this run uses, BEFORE any image work, and create the
# resources that need no image. Until this block completes, cleanup sweeps no
# task container at all (OWNERSHIP_ESTABLISHED above).
# --------------------------------------------------------------------------
for n in "$NET" "$ISO_NET"; do refuse_existing network "$n"; done
for v in "$VOL_DATA" "$VOL_ISO" "$VOL_FAILTX" "$VOL_ROLLBACK" "$VOL_SHARDS"; do refuse_existing volume "$v"; done
for c in "$CTR_PREV" "$CTR_ISO" "$CTR_CAND" "$CTR_READDR" "$CTR_FAILTX" "$CTR_ROLLBACK" "$CTR_SHARDS"; do
  refuse_existing container "$c"
done

docker network create "$NET" >/dev/null; own_network "$NET"
docker network create "$ISO_NET" >/dev/null; own_network "$ISO_NET"
create_owned_volume "$VOL_DATA"
create_owned_volume "$VOL_ISO"
OWNERSHIP_ESTABLISHED=1
log "ownership established: token $OWNER_TOKEN, resources named $ID-*"

# --------------------------------------------------------------------------
# Case records the shell owns (the Go runner writes its own).
# --------------------------------------------------------------------------
shell_case() {
  local name="$1" status="$2" duration="$3" detail="$4"
  CASE_NAME="$name" CASE_STATUS="$status" CASE_DURATION="$duration" CASE_DETAIL="$detail" \
  CASE_ID="$ID" CASE_DIR="$ARTIFACTS/cases" \
  CASE_OBSERVATIONS_FILE="${CASE_OBSERVATIONS_FILE:-}" python3 - <<'PY'
import json, os, pathlib, re
name = os.environ["CASE_NAME"]
rec = {
    "name": name,
    "phase": "host-controller",
    # Stamped so the finalizer can reject a record from another invocation.
    "lifecycle_id": os.environ["CASE_ID"],
    "status": os.environ["CASE_STATUS"],
    "duration_seconds": float(os.environ["CASE_DURATION"]),
    "detail": os.environ["CASE_DETAIL"],
}
obs_path = os.environ.get("CASE_OBSERVATIONS_FILE") or ""
if obs_path:
    rec["observations"] = json.loads(pathlib.Path(obs_path).read_text())
path = pathlib.Path(os.environ["CASE_DIR"]) / (re.sub(r"[^A-Za-z0-9_-]", "-", name) + ".json")
path.write_text(json.dumps(rec, indent=2) + "\n")
PY
  log "case $name: $status ($detail)"
}

# --------------------------------------------------------------------------
# Images
# --------------------------------------------------------------------------
log "pair=$PAIR candidate_sha=$CANDIDATE_SHA lifecycle_id=$ID artifacts=$ARTIFACTS platform=$PLATFORM"

# --------------------------------------------------------------------------
# Candidate provenance.
#
# The record names a candidate_sha, so something has to bind that SHA to the
# image actually qualified. There is no product version surface yet (F1
# prerequisite 3: the binary reports no build SHA), so the binding is external:
# this run either BUILT the image from a clean checkout at that SHA — recorded
# with the resulting image id — or it did not, in which case the image is
# supplied/unverified and the qualification is BLOCKED unless the operator
# explicitly overrides, which is itself recorded.
# --------------------------------------------------------------------------
provenance_start="$(date -u +%s)"
GIT_HEAD=""
GIT_DIRTY=false
GIT_DIFFSTAT=""
if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  GIT_HEAD="$(git -C "$ROOT" rev-parse HEAD)"
  if [[ -n "$(git -C "$ROOT" status --porcelain)" ]]; then
    GIT_DIRTY=true
    GIT_DIFFSTAT="$(git -C "$ROOT" status --porcelain | head -60)"
  fi
fi

ALLOW_UNVERIFIED="${CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE:-0}"
CANDIDATE_BUILT_HERE=false
PROVENANCE_REASONS=""
unverified_because() { PROVENANCE_REASONS="${PROVENANCE_REASONS}${1}"$'\n'; }
provenance_summary() { printf '%s' "$PROVENANCE_REASONS" | tr '\n' ';' | sed 's/;/; /g'; }

if docker image inspect "$CANDIDATE_IMAGE" >/dev/null 2>&1; then
  unverified_because "$CANDIDATE_IMAGE already existed on this host, so this run did not build it: nothing binds the image to $CANDIDATE_SHA"
else
  if [[ -z "$GIT_HEAD" ]]; then
    unverified_because "no git checkout is available at $ROOT, so a built image cannot be bound to a commit"
  else
    if [[ "$GIT_DIRTY" == true ]]; then
      if [[ "$ALLOW_UNVERIFIED" != "1" ]]; then
        log "dirty working tree:"
        printf '%s\n' "$GIT_DIFFSTAT"
        die "refusing to build $CANDIDATE_IMAGE from a dirty working tree: the image would be tagged with the clean SHA $GIT_HEAD it was not built from. Commit the changes, or set CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1 to record an explicitly non-qualifying run."
      fi
      unverified_because "built from a DIRTY working tree at $GIT_HEAD"
    fi
    if [[ "$CANDIDATE_SHA" != "$GIT_HEAD" ]]; then
      unverified_because "the candidate tag names $CANDIDATE_SHA but this checkout's HEAD is $GIT_HEAD"
    fi
  fi
  command -v just >/dev/null 2>&1 \
    || die "candidate image $CANDIDATE_IMAGE is absent and 'just' is not on PATH; build it with: CAESIUM_SKIP_IMAGE_BUILD=false just tag=$CANDIDATE_SHA build-release"
  log "candidate image $CANDIDATE_IMAGE absent; building it from this checkout"
  CAESIUM_SKIP_IMAGE_BUILD=false just tag="$CANDIDATE_SHA" build-release
  CANDIDATE_BUILT_HERE=true
fi
docker image inspect "$CANDIDATE_IMAGE" >/dev/null || die "candidate image $CANDIDATE_IMAGE is not present"
CANDIDATE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE")"
CANDIDATE_ARCH="$(docker image inspect --format '{{.Architecture}}' "$CANDIDATE_IMAGE")"
CANDIDATE_CREATED="$(docker image inspect --format '{{.Created}}' "$CANDIDATE_IMAGE")"
docker image inspect "$CANDIDATE_IMAGE" >"$ARTIFACTS/candidate-image.json"

PROVENANCE_VERIFIED=true
[[ -z "$PROVENANCE_REASONS" ]] || PROVENANCE_VERIFIED=false
PROVENANCE_OVERRIDE=false
[[ "$PROVENANCE_VERIFIED" == true || "$ALLOW_UNVERIFIED" != "1" ]] || PROVENANCE_OVERRIDE=true

PROV_BUILT="$CANDIDATE_BUILT_HERE" PROV_VERIFIED="$PROVENANCE_VERIFIED" \
PROV_OVERRIDE="$PROVENANCE_OVERRIDE" PROV_SHA="$CANDIDATE_SHA" PROV_GIT_HEAD="$GIT_HEAD" \
PROV_DIRTY="$GIT_DIRTY" PROV_DIFFSTAT="$GIT_DIFFSTAT" PROV_IMAGE="$CANDIDATE_IMAGE" \
PROV_IMAGE_ID="$CANDIDATE_IMAGE_ID" PROV_CREATED="$CANDIDATE_CREATED" \
PROV_REASONS="$PROVENANCE_REASONS" \
PROV_DEST="$ARTIFACTS/observations/candidate-provenance.json" python3 - <<'PY'
import json, os, pathlib
reasons = [r for r in os.environ["PROV_REASONS"].splitlines() if r.strip()]
built = os.environ["PROV_BUILT"] == "true"
verified = os.environ["PROV_VERIFIED"] == "true"
pathlib.Path(os.environ["PROV_DEST"]).write_text(json.dumps({
    "provenance": "built-by-this-run" if (built and verified) else "supplied/unverified",
    "built_by_this_run": built,
    "verified": verified,
    "override": os.environ["PROV_OVERRIDE"] == "true",
    "override_env": "CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE",
    "candidate_sha": os.environ["PROV_SHA"],
    "git_head": os.environ["PROV_GIT_HEAD"],
    "dirty": os.environ["PROV_DIRTY"] == "true",
    "dirty_status": [l for l in os.environ["PROV_DIFFSTAT"].splitlines() if l.strip()],
    "image_ref": os.environ["PROV_IMAGE"],
    "image_id": os.environ["PROV_IMAGE_ID"],
    "image_created": os.environ["PROV_CREATED"],
    "unverified_reasons": reasons,
}, indent=2) + "\n")
PY

if [[ "$PROVENANCE_VERIFIED" == true ]]; then
  shell_case "candidate-image-provenance" "pass" "$(( $(date -u +%s) - provenance_start ))" \
    "this run built $CANDIDATE_IMAGE ($CANDIDATE_IMAGE_ID) from the clean checkout at $GIT_HEAD, so candidate_sha binds to the image qualified"
elif [[ "$PROVENANCE_OVERRIDE" == true ]]; then
  shell_case "candidate-image-provenance" "pass" "$(( $(date -u +%s) - provenance_start ))" \
    "PROVENANCE UNVERIFIED, overridden by CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1: $(provenance_summary) image $CANDIDATE_IMAGE_ID is NOT bound to $CANDIDATE_SHA"
  log "WARNING: candidate provenance is unverified and was explicitly overridden; the record says so"
else
  shell_case "candidate-image-provenance" "blocked" "$(( $(date -u +%s) - provenance_start ))" \
    "candidate provenance cannot be established: $(provenance_summary) nothing binds $CANDIDATE_IMAGE ($CANDIDATE_IMAGE_ID) to candidate_sha $CANDIDATE_SHA. Delete the image and let this script build it, or set CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1 to record an explicitly non-qualifying run."
  die "candidate image provenance is unverified; refusing to qualify $CANDIDATE_SHA"
fi

log "pulling pinned previous release $PREV_IMAGE"
docker pull --platform "$PLATFORM" "$PREV_IMAGE" >/dev/null || die "docker pull $PREV_IMAGE failed"
docker image inspect "$PREV_IMAGE" >"$ARTIFACTS/previous-image.json"
PREV_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$PREV_IMAGE")"
PREV_ARCH="$(docker image inspect --format '{{.Architecture}}' "$PREV_IMAGE")"
PREV_REPO_DIGESTS="$(docker image inspect --format '{{join .RepoDigests ","}}' "$PREV_IMAGE")"

digest_start="$(date -u +%s)"
PREV_DIGEST="$(CAESIUM_LIFECYCLE_MATRIX_PAIR="$PAIR" \
  CAESIUM_LIFECYCLE_MATRIX="$ARTIFACTS/versions.json" \
  CAESIUM_LIFECYCLE_REPO_DIGESTS="$PREV_REPO_DIGESTS" \
  CAESIUM_LIFECYCLE_PLATFORM="$PLATFORM" python3 - <<'PY'
import json, os, sys
doc = json.load(open(os.environ["CAESIUM_LIFECYCLE_MATRIX"]))
pair = next(p for p in doc["pairs"] if p["id"] == os.environ["CAESIUM_LIFECYCLE_MATRIX_PAIR"])
pinned = pair["previous"]["digests"]
acceptable = {pinned["index"]}
per_arch = pinned.get(os.environ["CAESIUM_LIFECYCLE_PLATFORM"])
if per_arch:
    acceptable.add(per_arch)
raw = os.environ["CAESIUM_LIFECYCLE_REPO_DIGESTS"]
observed = {d.split("@", 1)[1] for d in raw.split(",") if "@" in d}
if not observed:
    print("the pulled previous-release image carries no repo digest", file=sys.stderr)
    raise SystemExit(1)
matched = observed & acceptable
if not matched:
    print(f"resolved digest(s) {sorted(observed)} match none of the pinned digests {sorted(acceptable)}",
          file=sys.stderr)
    raise SystemExit(1)
print(sorted(matched)[0])
PY
)" || die "the pulled $PREV_IMAGE is not the pinned release image"
[[ -n "$PREV_DIGEST" ]] || die "the pulled $PREV_IMAGE is not the pinned release image"
shell_case "previous-release-digest-pinned" "pass" "$(( $(date -u +%s) - digest_start ))" \
  "$PREV_IMAGE resolved to $PREV_REPO_DIGESTS, matching pinned $PREV_DIGEST, arch=$PREV_ARCH"

[[ "$PREV_ARCH" == "$HOST_ARCH" ]] \
  || die "previous-release image architecture $PREV_ARCH does not match host $HOST_ARCH"
[[ "$CANDIDATE_ARCH" == "$HOST_ARCH" ]] \
  || die "candidate image architecture $CANDIDATE_ARCH does not match host $HOST_ARCH"

docker pull --platform "$PLATFORM" "$TASK_IMAGE" >/dev/null || die "docker pull $TASK_IMAGE failed"

# --------------------------------------------------------------------------
# Runner: compile ./test/lifecycle explicitly with -tags=integration.
# --------------------------------------------------------------------------
BUILDER_IMAGE="${CAESIUM_LIFECYCLE_BUILDER_IMAGE:-caesiumcloud/caesium-builder:$CANDIDATE_SHA}"
if ! docker image inspect "$BUILDER_IMAGE" >/dev/null 2>&1; then
  BUILDER_IMAGE="caesiumcloud/caesium-builder:latest"
fi
docker image inspect "$BUILDER_IMAGE" >/dev/null \
  || die "no builder image (tried caesiumcloud/caesium-builder:$CANDIDATE_SHA and :latest); run 'just builder' first"

compile_start="$(date -u +%s)"
log "compiling ./test/lifecycle with -tags=integration in $BUILDER_IMAGE"
docker run --rm --platform "$PLATFORM" \
  -v "$ROOT":/bld/caesium \
  -v "$ARTIFACTS":/artifacts \
  -w /bld/caesium \
  -e CGO_ENABLED=0 \
  -e GOFLAGS=-buildvcs=false \
  "$BUILDER_IMAGE" \
  go test -tags=integration -c ./test/lifecycle -o /artifacts/lifecycle.test \
  || die "compiling ./test/lifecycle failed"
[[ -x "$ARTIFACTS/lifecycle.test" ]] || die "the compiled runner is missing at $ARTIFACTS/lifecycle.test"
shell_case "runner-compiled-with-integration-tag" "pass" "$(( $(date -u +%s) - compile_start ))" \
  "go test -tags=integration -c ./test/lifecycle in $BUILDER_IMAGE"

# --------------------------------------------------------------------------
# Docker socket access for the fixture jobs' task containers.
# --------------------------------------------------------------------------
SOCK_GID="$(docker run --rm -v "$SOCK":/var/run/docker.sock "$TASK_IMAGE" stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
if [[ -z "$SOCK_GID" ]]; then
  die "could not determine the docker socket's group inside a container; refusing to guess"
fi
SOCKET_MODE="group-add:$SOCK_GID"
log "docker socket access: --user $SERVER_UID --group-add $SOCK_GID (identical on both sides)"

# --------------------------------------------------------------------------
# The shared env block. Both sides of every supported transition get exactly
# this; the candidate-only variables are inert on the previous release and no
# fixture depends on them.
#
# ONE setting differs by side, deliberately and recorded: the run-queue DEQUEUER
# is off while the previous release seeds. cmd/start/start.go ORs
# CAESIUM_RUN_QUEUE_ENABLED and CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED when it
# decides whether to launch the dequeuer, so both have to be false to stop it.
# The queued-work fixture only exists if the row survives the transition, and a
# row can only survive it if nothing drains it — while a run that is still in
# flight when the server stops is never finalized again (local execution mode
# has no restart recovery), so holding the slot open with a live run would leave
# the queue permanently blocked and `job apply` permanently refused on that job.
# Admission to the run queue does not consult this flag (internal/run/store.go's
# ConcurrencyStrategyQueue branch), so the fixture is created exactly as F1
# describes: a trigger admitted to the queue while its predecessor is running.
# Draining it is the candidate's job, which is what assertion 5 measures.
server_env_args() {
  local shards="${1:-$SHARDS}" node_address="${2:-$NODE_ADDRESS}" dequeuer="${3:-true}"
  printf '%s\n' \
    "-e" "CAESIUM_LOG_LEVEL=debug" \
    "-e" "CAESIUM_DATABASE_PATH=$DATA_DIR" \
    "-e" "CAESIUM_DATABASE_SHARDS=$shards" \
    "-e" "CAESIUM_DATABASE_CONSOLE_ENABLED=true" \
    "-e" "CAESIUM_NODE_ADDRESS=$node_address" \
    "-e" "CAESIUM_MANUAL_TRIGGER_API_KEY=$MANUAL_KEY" \
    "-e" "CAESIUM_RUN_QUEUE_ENABLED=$dequeuer" \
    "-e" "CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=$dequeuer" \
    "-e" "CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms" \
    "-e" "DOCKER_HOST=unix:///var/run/docker.sock"
}

start_server() {
  local name="$1" image="$2" volume="$3" network="$4" shards="$5" node_address="$6" dequeuer="${7:-true}"
  local env_args=()
  while IFS= read -r line; do env_args+=("$line"); done < <(server_env_args "$shards" "$node_address" "$dequeuer")
  # Register ownership only once `docker run` actually names the container: if
  # another invocation's container has already claimed $name (a TOCTOU race
  # against the initial refuse_existing preflight, which ran once for every
  # name this invocation will ever use, long before some of them are actually
  # started), `docker run --name` fails and must NOT cause cleanup to
  # `rm -f` the container that won the race. The --label is a second,
  # independent guard: even a container that IS in OWNED_CONTAINERS is only
  # ever removed by cleanup if it still carries THIS invocation's ownership
  # token (see cleanup()).
  docker run -d --name "$name" \
    --platform "$PLATFORM" \
    --network "$network" \
    --user "$SERVER_UID" \
    --group-add "$SOCK_GID" \
    --label "$TASK_MARKER" \
    -v "$volume":"$DATA_DIR" \
    -v "$SOCK":/var/run/docker.sock \
    "${env_args[@]}" \
    "$image" start >/dev/null
  own_container "$name"
}

capture_state() {
  local name="$1" dest="$2"
  docker inspect "$name" --format \
    '{"status":"{{.State.Status}}","exit_code":{{.State.ExitCode}},"restart_count":{{.RestartCount}},"oom_killed":{{.State.OOMKilled}},"started_at":"{{.State.StartedAt}}","finished_at":"{{.State.FinishedAt}}","image_id":"{{.Image}}"}' \
    >"$dest"
}

# evaluate_graceful_stop reads a capture_state record and says whether the
# container actually shut down on its own within the `docker stop -t
# <timeout>` grace period, rather than being SIGKILLed when the timeout
# expired. `docker stop` exits 0 in BOTH cases, so its return code alone
# proves nothing: a container that ignores SIGTERM still reports exit 0 from
# `docker stop`, then shows up here with exit_code 137 (128+SIGKILL) and a
# stop duration at (or past) the timeout. Prints exactly one of
# pass|fail|blocked; the caller never derives the verdict any other way, so a
# synthetic capture_state record exercises the identical logic a real run
# uses.
evaluate_graceful_stop() {
  local state_file="$1" stop_seconds="$2" timeout="$3"
  STATE_FILE="$state_file" STOP_SECONDS="$stop_seconds" STOP_TIMEOUT="$timeout" python3 - <<'PY'
import json, os, sys

try:
    with open(os.environ["STATE_FILE"]) as f:
        state = json.load(f)
    stop_seconds = float(os.environ["STOP_SECONDS"])
    timeout = float(os.environ["STOP_TIMEOUT"])
except Exception as exc:
    print(f"could not read stop state: {exc}", file=sys.stderr)
    print("blocked")
    sys.exit(0)

reasons = []
if state.get("status") == "running":
    reasons.append("container is still running")
if state.get("exit_code") != 0:
    reasons.append(f"exit_code={state.get('exit_code')} (137 means SIGKILLed by the docker stop timeout)")
if state.get("oom_killed"):
    reasons.append("oom_killed=true")
if stop_seconds >= timeout:
    reasons.append(f"docker stop took {stop_seconds}s, which reached its {timeout}s timeout")

if reasons:
    print("; ".join(reasons), file=sys.stderr)
    print("fail")
else:
    print("pass")
PY
}

# classify_isolation_probe prints exactly one of reached|connect-fail|blocked
# for a `docker run ... nc -z` exit status. docker itself uses 125 (could not
# run the container), 126 (contained command not executable) and 127
# (contained command not found); alpine:3.23 nc -z uses 1 when the TCP connect
# fails. Anything other than 0 or 1 means the probe never executed and
# cannot prove isolation. The Go twin is classifyIsolationProbe in
# test/lifecycle/standalone_test.go; keep the two in lockstep
# (TestIsolationProbeClassification).
classify_isolation_probe() {
  python3 -c '
import sys
try:
    rc = int(sys.argv[1])
except (IndexError, ValueError):
    print("blocked")
    raise SystemExit(0)
if rc == 0:
    print("reached")
elif rc == 1:
    print("connect-fail")
else:
    print("blocked")
' "$1"
}

capture_logs() {
  local name="$1" dest="$2"
  docker logs "$name" >"$dest" 2>&1 || true
}

# wait_for_log blocks until a line appears in a container's log, so a snapshot
# taken afterwards cannot race the line the assertions look for.
#
# The match is a bash pattern on a captured string, never `docker logs | grep -q`:
# under `set -o pipefail` a matching `grep -q` exits as soon as it finds the
# needle, `docker logs` then dies of SIGPIPE, and the pipeline reports 141 — so
# the needle being present made the check report ABSENT. That cost 120s and
# emitted "the candidate did not log ..." for a line the log demonstrably had.
wait_for_log() {
  local name="$1" needle="$2" timeout="${3:-120}" i logs
  for ((i = 0; i < timeout; i++)); do
    logs="$(docker logs "$name" 2>&1 || true)"
    if [[ "$logs" == *"$needle"* ]]; then
      return 0
    fi
    if [[ "$(docker inspect "$name" --format '{{.State.Status}}' 2>/dev/null || echo gone)" != "running" ]]; then
      return 1
    fi
    sleep 1
  done
  return 1
}

isolation_start="$(date -u +%s)"
log "isolation: starting a second, independently named instance on its own network and volume"
start_server "$CTR_ISO" "$PREV_IMAGE" "$VOL_ISO" "$ISO_NET" "$SHARDS" "$NODE_ADDRESS" false
start_server "$CTR_PREV" "$PREV_IMAGE" "$VOL_DATA" "$NET" "$SHARDS" "$NODE_ADDRESS" false

run_phase() {
  # run_phase <phase> <test name> <image> <base url> <network> [extra docker args...]
  local phase="$1" test_name="$2" image="$3" base_url="$4" network="$5"
  shift 5
  local cli_dir
  case "$image" in
    "$PREV_IMAGE") cli_dir="$ARTIFACTS/cli/previous" ;;
    *) cli_dir="$ARTIFACTS/cli/candidate" ;;
  esac
  docker run --rm \
    --platform "$PLATFORM" \
    --network "$network" \
    --user 0:0 \
    --entrypoint /artifacts/lifecycle.test \
    -v "$ARTIFACTS":/artifacts \
    -v "$cli_dir":/cli:ro \
    -e CAESIUM_LIFECYCLE_PHASE="$phase" \
    -e CAESIUM_LIFECYCLE_ID="$ID" \
    -e CAESIUM_LIFECYCLE_PAIR="$PAIR" \
    -e CAESIUM_LIFECYCLE_ARTIFACTS=/artifacts \
    -e CAESIUM_LIFECYCLE_BASE_URL="$base_url" \
    -e CAESIUM_LIFECYCLE_TASK_MARKER="$TASK_MARKER" \
    -e CAESIUM_CLI_PATH=/cli/caesium \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY="$MANUAL_KEY" \
    "$@" \
    "$image" -test.v -test.count=1 -test.timeout 20m -test.run "^${test_name}\$" \
    2>&1 | tee -a "$ARTIFACTS/logs/runner-${phase}.log"
  return "${PIPESTATUS[0]}"
}

extract_cli() {
  local image="$1" dest="$2" ctr
  mkdir -p "$dest"
  ctr="$(docker create --platform "$PLATFORM" "$image" true)"
  docker cp "$ctr":/bin/caesium "$dest/caesium" >/dev/null
  docker rm -f "$ctr" >/dev/null 2>&1 || true
  chmod +x "$dest/caesium"
}

log "extracting each side's own CLI with docker cp"
extract_cli "$PREV_IMAGE" "$ARTIFACTS/cli/previous"
extract_cli "$CANDIDATE_IMAGE" "$ARTIFACTS/cli/candidate"

# Phase rc ledger. Every phase's exit status is folded into the qualification
# record: a phase that failed or never ran can never leave a passing record
# behind, whatever the case files happen to contain.
PHASE_RCS=""
phase_rc() { PHASE_RCS="${PHASE_RCS}${1}=${2}"$'\n'; }

# Self-check: the runner's own guard against an unobservable recorded outcome.
# It touches no server, so it runs before anything can be observed.
SELFCHECK_RC=0
run_phase "selfcheck" "TestLifecycleObservationValidation" "$CANDIDATE_IMAGE" "http://unused.invalid:8080" "$NET" \
  || SELFCHECK_RC=$?
phase_rc selfcheck "$SELFCHECK_RC"
[[ "$SELFCHECK_RC" -eq 0 ]] || die "the runner's observation-completeness self-check failed (rc=$SELFCHECK_RC)"

# The isolation proof: both instances run at once and neither network can
# resolve the other's container name.
if ! run_phase "probe" "TestLifecycleProbe" "$PREV_IMAGE" "http://${CTR_ISO}:8080" "$ISO_NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=isolation-second-instance; then
  phase_rc probe-isolation-second-instance 1
  die "the isolation instance could not be probed"
fi
phase_rc probe-isolation-second-instance 0
if ! run_phase "probe" "TestLifecycleProbe" "$PREV_IMAGE" "http://${CTR_PREV}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=isolation-primary-instance; then
  phase_rc probe-isolation-primary-instance 1
  die "the primary instance could not be probed"
fi
phase_rc probe-isolation-primary-instance 0
ISOLATION_OK="$(python3 - "$ARTIFACTS/observations/probe-isolation-primary-instance.json" \
                         "$ARTIFACTS/observations/probe-isolation-second-instance.json" <<'PY'
import json, sys
ok = all(json.load(open(p)).get("healthy") for p in sys.argv[1:])
print("yes" if ok else "no")
PY
)"

# Positive control first: the same helper image must REACH $CTR_PREV on $NET.
# Then the negative probe against $CTR_ISO must fail with nc's connect-fail
# status (1). docker 125 (could not run) or 127 (nc missing) is not proof of
# isolation — it used to look identical to "unreachable" and pass.
ISO_POS_LOG="$ARTIFACTS/logs/isolation-positive-control.log"
ISO_NEG_LOG="$ARTIFACTS/logs/isolation-negative-probe.log"
ISO_OBS="$ARTIFACTS/observations/isolation-probe.json"
ISO_POS_RC=0
ISO_NEG_RC=0
set +e
docker run --rm --network "$NET" "$TASK_IMAGE" sh -c "nc -z -w 3 $CTR_PREV 8080" >"$ISO_POS_LOG" 2>&1
ISO_POS_RC=$?
set -e
ISO_POS_CLASS="$(classify_isolation_probe "$ISO_POS_RC")"
set +e
docker run --rm --network "$NET" "$TASK_IMAGE" sh -c "nc -z -w 3 $CTR_ISO 8080" >"$ISO_NEG_LOG" 2>&1
ISO_NEG_RC=$?
set -e
ISO_NEG_CLASS="$(classify_isolation_probe "$ISO_NEG_RC")"

ISO_POS_RC="$ISO_POS_RC" ISO_NEG_RC="$ISO_NEG_RC" \
ISO_POS_CLASS="$ISO_POS_CLASS" ISO_NEG_CLASS="$ISO_NEG_CLASS" \
ISO_POS_LOG="$ISO_POS_LOG" ISO_NEG_LOG="$ISO_NEG_LOG" \
ISO_POS_TARGET="$CTR_PREV" ISO_NEG_TARGET="$CTR_ISO" \
ISO_NET_NAME="$NET" ISO_IMAGE="$TASK_IMAGE" ISO_OBS="$ISO_OBS" python3 - <<'PY'
import json, os, pathlib

def tail(path, n=4096):
    try:
        data = pathlib.Path(path).read_text(errors="replace")
    except Exception as exc:
        return f"<unreadable: {exc}>"
    if len(data) > n:
        return data[-n:]
    return data

rec = {
    "positive_control": {
        "target": os.environ["ISO_POS_TARGET"] + ":8080",
        "network": os.environ["ISO_NET_NAME"],
        "image": os.environ["ISO_IMAGE"],
        "exit_code": int(os.environ["ISO_POS_RC"]),
        "classification": os.environ["ISO_POS_CLASS"],
        "log": tail(os.environ["ISO_POS_LOG"]),
    },
    "negative_probe": {
        "target": os.environ["ISO_NEG_TARGET"] + ":8080",
        "network": os.environ["ISO_NET_NAME"],
        "image": os.environ["ISO_IMAGE"],
        "exit_code": int(os.environ["ISO_NEG_RC"]),
        "classification": os.environ["ISO_NEG_CLASS"],
        "log": tail(os.environ["ISO_NEG_LOG"]),
    },
}
pathlib.Path(os.environ["ISO_OBS"]).write_text(json.dumps(rec, indent=2) + "\n")
PY

iso_status="blocked"
if [[ "$ISOLATION_OK" != "yes" ]]; then
  iso_status="blocked"
  iso_detail="the two concurrent instances were not both healthy; isolation was not proven"
elif [[ "$ISO_POS_CLASS" != "reached" ]]; then
  iso_status="blocked"
  iso_detail="positive control did not reach ${CTR_PREV}:8080 on $NET (docker-run rc=$ISO_POS_RC classification=$ISO_POS_CLASS); the helper never demonstrated it can connect, so the negative probe cannot prove isolation"
elif [[ "$ISO_NEG_CLASS" == "reached" ]]; then
  iso_status="fail"
  iso_detail="isolation failed: a container on $NET reached ${CTR_ISO}:8080 (docker-run rc=$ISO_NEG_RC)"
elif [[ "$ISO_NEG_CLASS" != "connect-fail" ]]; then
  iso_status="blocked"
  iso_detail="negative probe against $CTR_ISO did not execute a connect-fail (docker-run rc=$ISO_NEG_RC classification=$ISO_NEG_CLASS); docker 125/127 is not proof of isolation"
else
  iso_status="pass"
  iso_detail="$CTR_PREV on $NET/$VOL_DATA and $CTR_ISO on $ISO_NET/$VOL_ISO were healthy at the same time; positive control reached $CTR_PREV; $NET cannot connect to $CTR_ISO (nc -z rc=$ISO_NEG_RC)"
fi
CASE_OBSERVATIONS_FILE="$ISO_OBS" \
  shell_case "two-instances-coexist" "$iso_status" "$(( $(date -u +%s) - isolation_start ))" \
    "$iso_detail"
if [[ "$iso_status" != "pass" ]]; then
  die "isolation case $iso_status: $iso_detail"
fi
log "isolation proven; removing the second instance"
docker rm -f "$CTR_ISO" >/dev/null 2>&1 || true
docker volume rm -f "$VOL_ISO" >/dev/null 2>&1 || true
docker network rm "$ISO_NET" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Phase: seed the previous release.
# --------------------------------------------------------------------------
seed_start="$(date -u +%s)"
log "seeding the retained-state fixture through $PREV_RELEASE's own public surface"
if ! run_phase "seed" "TestLifecycleSeedPreviousRelease" "$PREV_IMAGE" "http://${CTR_PREV}:8080" "$NET"; then
  phase_rc seed 1
  capture_logs "$CTR_PREV" "$ARTIFACTS/logs/previous.log"
  die "the seed phase failed; see $ARTIFACTS/logs/runner-seed.log and $ARTIFACTS/logs/previous.log"
fi
phase_rc seed 0
capture_logs "$CTR_PREV" "$ARTIFACTS/logs/previous.log"
capture_state "$CTR_PREV" "$ARTIFACTS/observations/previous-state.json"
log "seed complete in $(( $(date -u +%s) - seed_start ))s"

# --------------------------------------------------------------------------
# Stop the previous release with its full grace period, then branch the volume.
# --------------------------------------------------------------------------
STOP_TIMEOUT=60
stop_start="$(date -u +%s)"
log "docker stop -t $STOP_TIMEOUT $CTR_PREV (30s shutdown grace period must be able to run)"
docker stop -t "$STOP_TIMEOUT" "$CTR_PREV" >/dev/null
STOP_SECONDS="$(( $(date -u +%s) - stop_start ))"
capture_logs "$CTR_PREV" "$ARTIFACTS/logs/previous-after-stop.log"
STOPPED_STATE_FILE="$ARTIFACTS/observations/previous-stopped-state.json"
capture_state "$CTR_PREV" "$STOPPED_STATE_FILE"
PREVIOUS_STOPPED_AT="$(docker inspect "$CTR_PREV" --format '{{.State.FinishedAt}}')"
# `docker stop` returns 0 whether the process exited on its own or Docker had
# to SIGKILL it once the timeout elapsed — its exit status alone cannot tell
# these apart, so the verdict is read back from the state docker actually
# observed (see evaluate_graceful_stop).
STOP_VERDICT="$(evaluate_graceful_stop "$STOPPED_STATE_FILE" "$STOP_SECONDS" "$STOP_TIMEOUT" 2>"$ARTIFACTS/logs/previous-stop-verdict.log")"
shell_case "previous-release-stopped-gracefully" "$STOP_VERDICT" "$STOP_SECONDS" \
  "docker stop -t ${STOP_TIMEOUT} returned after ${STOP_SECONDS}s; container finished at $PREVIOUS_STOPPED_AT; state=$(cat "$STOPPED_STATE_FILE" 2>/dev/null || echo unavailable); $(cat "$ARTIFACTS/logs/previous-stop-verdict.log" 2>/dev/null || true)"

copy_volume() {
  local src="$1" dst="$2"
  refuse_existing volume "$dst"
  create_owned_volume "$dst"
  docker run --rm --user 0:0 \
    -v "$src":/from:ro -v "$dst":/to \
    -e OWNER="$SERVER_UID" \
    "$TASK_IMAGE" sh -ec 'cp -a /from/. /to/ && chown "$OWNER" /to'
}

log "branching the stopped volume for the transitions that must not contaminate the main case"
copy_volume "$VOL_DATA" "$VOL_FAILTX"

# --------------------------------------------------------------------------
# Phase: upgrade — the candidate on the SAME volume.
# --------------------------------------------------------------------------
upgrade_start="$(date -u +%s)"
log "starting the candidate on the retained volume $VOL_DATA"
start_server "$CTR_CAND" "$CANDIDATE_IMAGE" "$VOL_DATA" "$NET" "$SHARDS" "$NODE_ADDRESS"
wait_for_log "$CTR_CAND" "migrating database" 120 \
  || log "WARNING: the candidate did not log 'migrating database' within 120s; the upgrade case will report it"
capture_logs "$CTR_CAND" "$ARTIFACTS/logs/candidate.log"
capture_state "$CTR_CAND" "$ARTIFACTS/observations/candidate-state.json"
OBSERVED_IMAGE_ID="$(docker inspect "$CTR_CAND" --format '{{.Image}}')"
CAESIUM_LIFECYCLE_OBS_DIR="$ARTIFACTS/observations" \
CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID="$CANDIDATE_IMAGE_ID" \
CAESIUM_LIFECYCLE_OBSERVED_IMAGE_ID="$OBSERVED_IMAGE_ID" \
CAESIUM_LIFECYCLE_IMAGE_REF="$CANDIDATE_IMAGE" python3 - <<'PY'
import json, os, pathlib
out = pathlib.Path(os.environ["CAESIUM_LIFECYCLE_OBS_DIR"]) / "candidate-identity.json"
out.write_text(json.dumps({
    "expected_image_id": os.environ["CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID"],
    "observed_image_id": os.environ["CAESIUM_LIFECYCLE_OBSERVED_IMAGE_ID"],
    "image_ref": os.environ["CAESIUM_LIFECYCLE_IMAGE_REF"],
}, indent=2) + "\n")
PY

UPGRADE_RC=0
run_phase "upgrade" "TestLifecycleUpgradeToCandidate" "$CANDIDATE_IMAGE" "http://${CTR_CAND}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PREVIOUS_STOPPED_AT="$PREVIOUS_STOPPED_AT" || UPGRADE_RC=$?
phase_rc upgrade "$UPGRADE_RC"
capture_logs "$CTR_CAND" "$ARTIFACTS/logs/candidate.log"
capture_state "$CTR_CAND" "$ARTIFACTS/observations/candidate-state-after.json"
CAND_STATUS_AFTER="$(docker inspect "$CTR_CAND" --format '{{.State.Status}}')"
CAND_EXIT_AFTER="$(docker inspect "$CTR_CAND" --format '{{.State.ExitCode}}')"
CAND_RESTARTS_AFTER="$(docker inspect "$CTR_CAND" --format '{{.RestartCount}}')"
if [[ "$CAND_STATUS_AFTER" == "running" && "$CAND_EXIT_AFTER" == "0" && "$CAND_RESTARTS_AFTER" == "0" ]]; then
  shell_case "candidate-never-exited-nonzero" "pass" "$(( $(date -u +%s) - upgrade_start ))" \
    "after the upgrade phase the candidate container is $CAND_STATUS_AFTER, exit $CAND_EXIT_AFTER, $CAND_RESTARTS_AFTER restarts"
else
  shell_case "candidate-never-exited-nonzero" "fail" "$(( $(date -u +%s) - upgrade_start ))" \
    "after the upgrade phase the candidate container is $CAND_STATUS_AFTER, exit $CAND_EXIT_AFTER, $CAND_RESTARTS_AFTER restarts"
fi
log "upgrade phase finished rc=$UPGRADE_RC in $(( $(date -u +%s) - upgrade_start ))s"

# --------------------------------------------------------------------------
# Phase: supported re-address (PR #536), on the SAME volume.
# --------------------------------------------------------------------------
log "stopping the candidate before the re-address transition"
docker stop -t 60 "$CTR_CAND" >/dev/null
capture_logs "$CTR_CAND" "$ARTIFACTS/logs/candidate-after-stop.log"

log "branching the migrated volume for the recorded-outcome cases"
copy_volume "$VOL_DATA" "$VOL_ROLLBACK"
copy_volume "$VOL_DATA" "$VOL_SHARDS"

readdress_start="$(date -u +%s)"
log "restarting the candidate on $VOL_DATA at $ALT_NODE_ADDRESS (PR #536's supported re-address)"
start_server "$CTR_READDR" "$CANDIDATE_IMAGE" "$VOL_DATA" "$NET" "$SHARDS" "$ALT_NODE_ADDRESS"
READDRESS_RC=0
run_phase "readdress" "TestLifecycleCandidateAddressChange" "$CANDIDATE_IMAGE" "http://${CTR_READDR}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_EXPECT_NODE_ADDRESS="$ALT_NODE_ADDRESS" \
  -e CAESIUM_LIFECYCLE_DATA_DIR=/volume \
  -v "$VOL_DATA":/volume:ro || READDRESS_RC=$?
phase_rc readdress "$READDRESS_RC"
capture_logs "$CTR_READDR" "$ARTIFACTS/logs/candidate-readdress.log"
capture_state "$CTR_READDR" "$ARTIFACTS/observations/candidate-readdress-state.json"
log "readdress phase finished rc=$READDRESS_RC in $(( $(date -u +%s) - readdress_start ))s"
docker stop -t 60 "$CTR_READDR" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Required failing transition: the pinned previous release, re-addressed,
# on its OWN copy of the volume so it cannot disturb the main case.
# --------------------------------------------------------------------------
# record_container_outcome writes what was ACTUALLY observed, and records every
# observation that failed. A swallowed `docker inspect`/`docker logs` error used
# to look identical to a real "exited 0 with no output": the runner now rejects
# any record whose observation is incomplete (validateContainerOutcome), so a
# recorded-outcome case built on one is blocked, never reported.
record_container_outcome() {
  local name="$1" image="$2" volume="$3" envdesc="$4" dest="$5"
  local status exit_code log_tail errors="" log_captured=true complete=true
  local note
  if ! status="$(docker inspect "$name" --format '{{.State.Status}}' 2>&1)" || [[ -z "$status" ]]; then
    errors="${errors}docker inspect .State.Status failed for $name: ${status:-no output}"$'\n'
    status="unknown"
  fi
  if ! exit_code="$(docker inspect "$name" --format '{{.State.ExitCode}}' 2>&1)" \
     || [[ ! "$exit_code" =~ ^-?[0-9]+$ ]]; then
    errors="${errors}docker inspect .State.ExitCode failed for $name: ${exit_code:-no output}"$'\n'
    exit_code="-1"
  fi
  if ! log_tail="$(docker logs --tail 200 "$name" 2>&1)"; then
    errors="${errors}docker logs failed for $name: ${log_tail:-no output}"$'\n'
    log_tail=""
    log_captured=false
  fi
  [[ -z "$errors" ]] || complete=false
  if [[ "$complete" != true ]]; then
    note="$(printf '%s' "$errors" | tr '\n' ';')"
    log "WARNING: the outcome of $name could not be fully observed: $note"
  fi
  CO_NAME="$name" CO_IMAGE="$image" CO_VOLUME="$volume" CO_ENV="$envdesc" \
  CO_STATUS="$status" CO_EXIT="$exit_code" CO_LOG="$log_tail" CO_DEST="$dest" \
  CO_ERRORS="$errors" CO_COMPLETE="$complete" CO_LOG_CAPTURED="$log_captured" python3 - <<'PY'
import datetime, json, os, pathlib
rec = {
    "name": os.environ["CO_NAME"],
    "image": os.environ["CO_IMAGE"],
    "volume": os.environ["CO_VOLUME"],
    "env": os.environ["CO_ENV"],
    "exit_code": int(os.environ["CO_EXIT"]),
    "status": os.environ["CO_STATUS"],
    "started": os.environ["CO_STATUS"] == "running",
    "log_tail": os.environ["CO_LOG"],
    "observed_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "observation_complete": os.environ["CO_COMPLETE"] == "true",
    "observation_errors": [e for e in os.environ["CO_ERRORS"].splitlines() if e.strip()],
    "log_captured": os.environ["CO_LOG_CAPTURED"] == "true",
}
pathlib.Path(os.environ["CO_DEST"]).write_text(json.dumps(rec, indent=2) + "\n")
PY
}

PROBE_FAILTX_RC=0
PROBE_ROLLBACK_RC=0
PROBE_SHARDS_RC=0
log "unsupported transition: $PREV_RELEASE on its own copy of the volume at $ALT_NODE_ADDRESS"
start_server "$CTR_FAILTX" "$PREV_IMAGE" "$VOL_FAILTX" "$NET" "$SHARDS" "$ALT_NODE_ADDRESS"
# The previous release has no address reconciliation: it is expected to exit.
# Give it a bounded window and record whatever it actually did.
for _ in $(seq 1 30); do
  [[ "$(docker inspect "$CTR_FAILTX" --format '{{.State.Status}}')" == "running" ]] || break
  sleep 1
done
capture_logs "$CTR_FAILTX" "$ARTIFACTS/logs/previous-readdress.log"
record_container_outcome "$CTR_FAILTX" "$PREV_IMAGE" "$VOL_FAILTX" \
  "CAESIUM_NODE_ADDRESS=$ALT_NODE_ADDRESS" "$ARTIFACTS/observations/previous-readdress.json"
run_phase "probe" "TestLifecycleProbe" "$CANDIDATE_IMAGE" "http://${CTR_FAILTX}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=previous-readdress \
  -e CAESIUM_LIFECYCLE_PROBE_DEADLINE_SECONDS=20 || PROBE_FAILTX_RC=$?
phase_rc probe-previous-readdress "$PROBE_FAILTX_RC"
docker rm -f "$CTR_FAILTX" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Recorded-outcome cases, each on its own copy of the volume so ordering
# cannot contaminate the main case. No expectation is set for either.
# --------------------------------------------------------------------------
log "recorded outcome: $PREV_RELEASE restarted on a COPY of the candidate-migrated volume (rollback)"
start_server "$CTR_ROLLBACK" "$PREV_IMAGE" "$VOL_ROLLBACK" "$NET" "$SHARDS" "$NODE_ADDRESS"
run_phase "probe" "TestLifecycleProbe" "$CANDIDATE_IMAGE" "http://${CTR_ROLLBACK}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=rollback || PROBE_ROLLBACK_RC=$?
phase_rc probe-rollback "$PROBE_ROLLBACK_RC"
capture_logs "$CTR_ROLLBACK" "$ARTIFACTS/logs/rollback.log"
record_container_outcome "$CTR_ROLLBACK" "$PREV_IMAGE" "$VOL_ROLLBACK" \
  "CAESIUM_DATABASE_SHARDS=$SHARDS" "$ARTIFACTS/observations/rollback.json"
docker rm -f "$CTR_ROLLBACK" >/dev/null 2>&1 || true

log "recorded outcome: the candidate on a COPY of the volume with CAESIUM_DATABASE_SHARDS=$ALT_SHARDS"
start_server "$CTR_SHARDS" "$CANDIDATE_IMAGE" "$VOL_SHARDS" "$NET" "$ALT_SHARDS" "$NODE_ADDRESS"
run_phase "probe" "TestLifecycleProbe" "$CANDIDATE_IMAGE" "http://${CTR_SHARDS}:8080" "$NET" \
  -e CAESIUM_LIFECYCLE_PROBE_NAME=shards || PROBE_SHARDS_RC=$?
phase_rc probe-shards "$PROBE_SHARDS_RC"
capture_logs "$CTR_SHARDS" "$ARTIFACTS/logs/shards.log"
record_container_outcome "$CTR_SHARDS" "$CANDIDATE_IMAGE" "$VOL_SHARDS" \
  "CAESIUM_DATABASE_SHARDS=$ALT_SHARDS" "$ARTIFACTS/observations/shards.json"
docker rm -f "$CTR_SHARDS" >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
# Phase: judge the transitions.
# --------------------------------------------------------------------------
OUTCOMES_RC=0
run_phase "outcomes" "TestLifecycleTransitionOutcomes" "$CANDIDATE_IMAGE" "http://${CTR_CAND}:8080" "$NET" \
  || OUTCOMES_RC=$?
phase_rc outcomes "$OUTCOMES_RC"

# --------------------------------------------------------------------------
# Qualification record.
#
# The complete expected case set. A qualification is a claim about ALL of
# these: a case with no record means the phase that owns it never got far
# enough to record it, which is blocked, not absent. Adding a case here without
# producing it — or producing one without listing it — fails the run, which is
# the point: the record can no longer say "pass" while a whole phase is missing.
# --------------------------------------------------------------------------
EXPECTED_CASES="previous-release-digest-pinned
candidate-image-provenance
runner-compiled-with-integration-tag
observation-validation-self-check
two-instances-coexist
seed-previous-release
previous-release-stopped-gracefully
assert1-candidate-healthy-and-migrated
assert2-schema-migrated-additively
assert3-recorded-identities-readable
assert4-event-replay-from-explicit-cursor
assert5-queued-row-reaches-a-started-run
assert6-export-relints-and-diffs-clean
assert7-serving-build-is-the-candidate
recorded-in-flight-run-after-upgrade
candidate-never-exited-nonzero
supported-candidate-readdress
unsupported-previous-release-readdress
recorded-rollback-previous-release-on-migrated-volume
recorded-shard-count-change"

FINISH_EPOCH="$(date -u +%s)"
CLI_PREV_SHA="$(python3 -c '
import hashlib, sys
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
' "$ARTIFACTS/cli/previous/caesium")"
CLI_CAND_SHA="$(python3 -c '
import hashlib, sys
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
' "$ARTIFACTS/cli/candidate/caesium")"

set +e
CAESIUM_LIFECYCLE_QUAL_ENV="$(server_env_args | paste -sd' ' -)" \
QUAL_ARTIFACTS="$ARTIFACTS" \
QUAL_PAIR="$PAIR" \
QUAL_SHA="$CANDIDATE_SHA" \
QUAL_ID="$ID" \
QUAL_START="$START_EPOCH" \
QUAL_FINISH="$FINISH_EPOCH" \
QUAL_HOST_ARCH="$HOST_ARCH" \
QUAL_PREV_IMAGE="$PREV_IMAGE" \
QUAL_PREV_IMAGE_ID="$PREV_IMAGE_ID" \
QUAL_PREV_DIGESTS="$PREV_REPO_DIGESTS" \
QUAL_PREV_ARCH="$PREV_ARCH" \
QUAL_CAND_IMAGE="$CANDIDATE_IMAGE" \
QUAL_CAND_IMAGE_ID="$CANDIDATE_IMAGE_ID" \
QUAL_CAND_ARCH="$CANDIDATE_ARCH" \
QUAL_BUILDER_IMAGE="$BUILDER_IMAGE" \
QUAL_CLI_PREV_SHA="$CLI_PREV_SHA" \
QUAL_CLI_CAND_SHA="$CLI_CAND_SHA" \
QUAL_SOCKET_MODE="$SOCKET_MODE" \
QUAL_SERVER_UID="$SERVER_UID" \
QUAL_SHARDS="$SHARDS" \
QUAL_EXPECTED_CASES="$EXPECTED_CASES" \
QUAL_PHASES="$PHASE_RCS" \
QUAL_OWNER_TOKEN="$OWNER_TOKEN" \
python3 - <<'PY'
import datetime, json, os, pathlib

art = pathlib.Path(os.environ["QUAL_ARTIFACTS"])
matrix = json.loads((art / "versions.json").read_text())
pair = next(p for p in matrix["pairs"] if p["id"] == os.environ["QUAL_PAIR"])
lifecycle_id = os.environ["QUAL_ID"]

# Every phase's exit status. A phase that failed, or that never ran and so
# never reported one, fails the qualification on its own — independently of
# what the case files happen to say.
phases, malformed_phase_lines = {}, []
for line in os.environ["QUAL_PHASES"].splitlines():
    line = line.strip()
    if not line:
        continue
    name, _, rc = line.partition("=")
    try:
        phases[name] = int(rc)
    except ValueError:
        malformed_phase_lines.append(line)
failed_phases = sorted(n for n, rc in phases.items() if rc != 0)

expected = [n for n in (l.strip() for l in os.environ["QUAL_EXPECTED_CASES"].splitlines()) if n]

# Case records, keyed by name, each validated against THIS invocation's
# lifecycle id so a record written by an earlier run can never be counted.
by_name, stale = {}, []
case_dir = art / "cases"
for path in sorted(case_dir.glob("*.json")):
    try:
        rec = json.loads(path.read_text())
    except Exception as exc:  # a case record that cannot be read is not a pass
        rec = {"name": path.stem, "status": "blocked", "duration_seconds": 0.0,
               "detail": f"case record unreadable: {exc}"}
    name = rec.get("name") or path.stem
    got_id = rec.get("lifecycle_id")
    if got_id != lifecycle_id:
        stale.append(f"{name} (lifecycle_id={got_id!r})")
        rec = dict(rec, status="blocked", duration_seconds=rec.get("duration_seconds", 0.0),
                   detail=f"case record carries lifecycle_id {got_id!r}, not this run's "
                          f"{lifecycle_id!r}: it is not evidence for this qualification")
    by_name[name] = rec

missing = [n for n in expected if n not in by_name]
for name in missing:
    by_name[name] = {
        "name": name, "phase": "never-recorded", "lifecycle_id": lifecycle_id,
        "status": "blocked", "duration_seconds": 0.0,
        "detail": "expected case produced no record: the phase that owns it did not run, "
                  "or did not reach the point of recording it",
    }
unexpected = sorted(n for n in by_name if n not in expected)
cases = [by_name[n] for n in expected] + [by_name[n] for n in unexpected]

# recorded-outcome cases carry no pre-judged expectation, so their OUTCOME does
# not decide the qualification — but they must exist and be conclusive: a
# missing or blocked one is synthesized/marked above and fails here.
failed = [c["name"] for c in cases if c.get("status") not in ("pass", "recorded-outcome")]

delta_path = art / "observations" / "schema-delta.json"
delta = json.loads(delta_path.read_text()) if delta_path.exists() else None

prov_path = art / "observations" / "candidate-provenance.json"
provenance = json.loads(prov_path.read_text()) if prov_path.exists() else None

integrity = []
if provenance is None:
    integrity.append("observations/candidate-provenance.json is missing: nothing binds the qualified "
                     "image to candidate_sha")
if malformed_phase_lines:
    integrity.append(f"unparseable phase return codes: {malformed_phase_lines}")

ok = not failed and not failed_phases and not integrity

record = {
    "schema_version": 1,
    "kind": "caesium-lifecycle-qualification",
    "pair": os.environ["QUAL_PAIR"],
    "candidate_sha": os.environ["QUAL_SHA"],
    "lifecycle_id": os.environ["QUAL_ID"],
    "started_at": datetime.datetime.fromtimestamp(
        int(os.environ["QUAL_START"]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "finished_at": datetime.datetime.fromtimestamp(
        int(os.environ["QUAL_FINISH"]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "duration_seconds": int(os.environ["QUAL_FINISH"]) - int(os.environ["QUAL_START"]),
    "host": {"arch": os.environ["QUAL_HOST_ARCH"]},
    "topology": {"nodes": 1, "database_shards": int(os.environ["QUAL_SHARDS"])},
    "images": {
        "previous": {
            "ref": os.environ["QUAL_PREV_IMAGE"],
            "release": pair["previous"]["release"],
            "image_id": os.environ["QUAL_PREV_IMAGE_ID"],
            "repo_digests": os.environ["QUAL_PREV_DIGESTS"].split(",") if os.environ["QUAL_PREV_DIGESTS"] else [],
            "pinned_digests": pair["previous"]["digests"],
            "architecture": os.environ["QUAL_PREV_ARCH"],
        },
        "candidate": {
            "ref": os.environ["QUAL_CAND_IMAGE"],
            "image_id": os.environ["QUAL_CAND_IMAGE_ID"],
            "architecture": os.environ["QUAL_CAND_ARCH"],
        },
        "runner_builder": os.environ["QUAL_BUILDER_IMAGE"],
    },
    "cli": {
        "previous_sha256": os.environ["QUAL_CLI_PREV_SHA"],
        "candidate_sha256": os.environ["QUAL_CLI_CAND_SHA"],
        "extraction": "docker cp <container>:/bin/caesium, one per side",
        "published_release_assets_sha256": pair["previous"]["release_assets_sha256"],
        "published_asset_smoke": pair["previous"]["cli_smoke"],
    },
    "server_env": os.environ["CAESIUM_LIFECYCLE_QUAL_ENV"],
    "server_user": os.environ["QUAL_SERVER_UID"],
    "docker_socket_access": os.environ["QUAL_SOCKET_MODE"],
    "schema_delta": delta,
    "ownership_token": os.environ["QUAL_OWNER_TOKEN"],
    "candidate_provenance": provenance,
    "expected_cases": expected,
    "phases": phases,
    "cases": cases,
    "result": "pass" if ok else "fail",
    "failed_cases": failed,
    "failed_phases": failed_phases,
    "missing_cases": missing,
    "unexpected_cases": unexpected,
    "stale_case_records": stale,
    "integrity_problems": integrity,
}
if provenance and not provenance.get("verified", False):
    record["result_qualifier"] = (
        "candidate provenance UNVERIFIED and explicitly overridden: the image qualified is "
        "not bound to candidate_sha"
    )
(art / "qualification.json").write_text(json.dumps(record, indent=2) + "\n")

print()
print(f"lifecycle qualification: {record['result']}  ({record['duration_seconds']}s)")
for case in cases:
    print(f"  {case.get('status','?'):16s} {case.get('name','?')}"
          f"  ({case.get('duration_seconds', 0.0):.1f}s)")
if delta:
    def _n(key):
        return len(delta.get(key) or [])
    print(f"  schema: {delta['previous_table_count']} -> {delta['candidate_table_count']} tables; "
          f"pinned additions: tables={_n('pinned_table_additions')} columns={_n('pinned_column_additions')}; "
          f"unpinned additions recorded: tables={_n('unpinned_table_additions')} "
          f"columns={_n('unpinned_column_additions')}; "
          f"dropped: tables={_n('dropped_tables')} columns={_n('dropped_columns')}")
print(f"  phases: {json.dumps(phases, sort_keys=True)}")
if missing:
    print(f"  MISSING expected cases (blocked): {missing}")
if stale:
    print(f"  STALE case records rejected: {stale}")
if unexpected:
    print(f"  unexpected case records: {unexpected}")
if failed_phases:
    print(f"  FAILED phases: {[f'{n}={phases[n]}' for n in failed_phases]}")
for problem in integrity:
    print(f"  INTEGRITY: {problem}")
if record.get("result_qualifier"):
    print(f"  NOTE: {record['result_qualifier']}")
if not ok:
    raise SystemExit(1)
PY
QUAL_RC=$?
set -e

log "qualification record written to $ARTIFACTS/qualification.json"
# Belt and braces: the record itself already folds every phase rc in (QUAL_RC is
# nonzero whenever it does not say "pass"), so the shell's exit status and the
# record can no longer disagree. This check would catch a phase rc that never
# reached the ledger.
RECORD_RESULT="$(python3 -c '
import json, sys
print(json.load(open(sys.argv[1])).get("result", "missing"))
' "$ARTIFACTS/qualification.json" 2>/dev/null || echo unreadable)"
if [[ "$UPGRADE_RC" -ne 0 || "$READDRESS_RC" -ne 0 || "$OUTCOMES_RC" -ne 0 || "$QUAL_RC" -ne 0 \
      || "$RECORD_RESULT" != "pass" ]]; then
  die "lifecycle qualification FAILED (upgrade=$UPGRADE_RC readdress=$READDRESS_RC outcomes=$OUTCOMES_RC record=$QUAL_RC result=$RECORD_RESULT)"
fi
log "lifecycle qualification PASSED for $CANDIDATE_SHA against $PREV_RELEASE (record result=$RECORD_RESULT)"
