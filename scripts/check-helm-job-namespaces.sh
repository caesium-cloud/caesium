#!/usr/bin/env bash
# Render checks for the chart's per-namespace RBAC and policy mount
# (identity-and-access C3). Run by the helm-lint CI job.
set -euo pipefail
cd "$(dirname "$0")/.."
chart=./helm/caesium
fail() { echo "check-helm-job-namespaces: $*" >&2; exit 1; }

# Defaults and every CI values file render no job-namespace RBAC and no policy.
for values in "" helm/caesium/ci/test-values*.yaml; do
  [ "$values" = helm/caesium/ci/test-values-k8s-mapped.yaml ] && continue
  rendered=$(helm template caesium "$chart" ${values:+--values "$values"})
  if grep -q -- '-jobs$' <<<"$rendered" || grep -q 'access-policy' <<<"$rendered" || grep -q CAESIUM_AUTH_POLICY_FILE <<<"$rendered"; then
    fail "${values:-default values} render job-namespace RBAC or a policy mount"
  fi
done

mapped=(--namespace caesium-c3 --values helm/caesium/ci/test-values-k8s.yaml --values helm/caesium/ci/test-values-k8s-mapped.yaml)
helm lint "$chart" "${mapped[@]}" >/dev/null
rbac=$(helm template c3 "$chart" "${mapped[@]}" --show-only templates/role.yaml --show-only templates/rolebinding.yaml)
role=$(awk '/^kind: Role$/{doc=1} doc' <<<"$rbac")
for expect in 'name: c3-caesium-jobs' 'namespace: caesium-mapped' 'resources: \["secrets"\]' 'resources: \["persistentvolumeclaims"\]' 'resources: \["pods", "pods/log"\]'; do
  grep -Eq -- "$expect" <<<"$role" || fail "mapped Role lacks: $expect"
done
grep -q 'pods/exec' <<<"$rbac" && fail "pods/exec is never granted"
binding=$(awk '/^kind: RoleBinding$/{n++} n' <<<"$rbac")
grep -q 'name: c3-caesium$' <<<"$binding" || fail "RoleBinding subject is not the chart service account"
grep -q 'namespace: caesium-c3$' <<<"$binding" || fail "RoleBinding subject is not in the release namespace"

sts=$(helm template c3 "$chart" "${mapped[@]}" --show-only templates/statefulset.yaml)
grep -q 'value: "/etc/caesium/access-policy/policy.yaml"' <<<"$sts" || fail "CAESIUM_AUTH_POLICY_FILE is not set from accessPolicy"
grep -A3 'name: access-policy$' <<<"$sts" | grep -q 'readOnly: true' || fail "the policy mount is not read-only"

# Disabled engine still renders the mapped grants; bad names fail the render.
helm template caesium "$chart" --set 'jobNamespaces={team-a}' | grep -q 'name: caesium-jobs' || fail "jobNamespaces without the engine rendered nothing"
if helm template caesium "$chart" --set 'jobNamespaces={Bad_NS}' >/dev/null 2>&1; then
  fail "an invalid namespace name rendered"
fi
echo "check-helm-job-namespaces: ok"
