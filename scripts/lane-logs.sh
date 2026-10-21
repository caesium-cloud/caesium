#!/usr/bin/env bash
# Print container logs with Caesium credentials redacted (plan item G1).
#
#   usage: scripts/lane-logs.sh <container-cli> <container>...
#
# A Caesium server prints its bootstrap admin API key (csk_...) to stdout at
# startup, before OIDC discovery or any readiness check can fail, and session
# tokens (css_...) can appear in debug output. Every lane log dump goes through
# this filter so the plaintext never reaches CI output, whether or not the
# recipe got far enough to capture and ::add-mask:: the key. Best effort: a
# missing container is reported, never fatal, so a failure path keeps its
# original exit status. scripts/test_ci.py feeds it a fake bootstrap banner.
set -u
cli="${1:?usage: lane-logs.sh <container-cli> <container>...}"
shift
for container in "$@"; do
    echo "----- ${container} logs (credentials redacted) -----"
    "$cli" logs "$container" 2>&1 | sed -E 's/(csk|css)_[A-Za-z0-9_-]+/[REDACTED_CREDENTIAL]/g' || true
done
exit 0
