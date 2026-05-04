#!/usr/bin/env bash
# Configure the cluster's containerd to bypass Docker Desktop's catch-all
# pull-through mirror for our local dev registry, so kubelet can pull
# `host.docker.internal:<port>/...` images directly.
#
# Docker Desktop Kubernetes ships a `_default` hosts.toml that points all
# pulls at an internal registry-mirror. Without a per-host override, our
# local registry is unreachable. We write a hosts.toml under
# `/etc/containerd/certs.d/host.docker.internal:<port>/` on the node;
# containerd 1.5+ picks the per-host file up dynamically.
set -euo pipefail

port="${1:-5050}"
pod="caesium-registry-bypass"
node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"

# Drop any leftover pod from a previous run.
kubectl delete pod "$pod" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
spec:
  hostNetwork: true
  hostPID: true
  restartPolicy: Never
  nodeName: ${node}
  containers:
  - name: bypass
    image: busybox
    command:
    - sh
    - -c
    - |
      set -e
      target="/host/etc/containerd/certs.d/host.docker.internal:${port}"
      mkdir -p "\$target"
      cat > "\$target/hosts.toml" <<TOML
      server = "http://host.docker.internal:${port}"

      [host."http://host.docker.internal:${port}"]
      capabilities = ["pull", "resolve"]
      skip_verify = true
      TOML
      echo "Wrote \$target/hosts.toml"
    securityContext:
      privileged: true
    volumeMounts:
    - name: host
      mountPath: /host
  volumes:
  - name: host
    hostPath:
      path: /
EOF

# Wait until the pod terminates (success or failure).
for _ in $(seq 1 30); do
    phase="$(kubectl get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in
        Succeeded|Failed) break ;;
    esac
    sleep 1
done

logs="$(kubectl logs "$pod" 2>/dev/null || true)"
kubectl delete pod "$pod" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true

if [ "${phase:-}" != "Succeeded" ]; then
    echo "Failed to configure containerd bypass (pod phase: ${phase:-unknown})." >&2
    [ -n "$logs" ] && echo "$logs" >&2
    exit 1
fi

echo "  $(echo "$logs" | tail -n 1)"
