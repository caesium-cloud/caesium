#!/bin/sh
# Run from the test package directory, just as `go test ./test/` does. The suite
# resolves real CLI/manifest/infra fixtures relative to this directory.
set -eu
cd "$(dirname "$0")/../test"
if [ -n "${CAESIUM_INTEGRATION_TEST_BINARY:-}" ]; then
    if [ ! -x "$CAESIUM_INTEGRATION_TEST_BINARY" ]; then
        echo "Precompiled integration test binary is missing: $CAESIUM_INTEGRATION_TEST_BINARY" >&2
        exit 1
    fi
    exec "$CAESIUM_INTEGRATION_TEST_BINARY" -test.count=1 -test.timeout=30m -test.v "$@"
fi
mkdir -p ../ui/dist
touch ../ui/dist/index.html
exec go test -tags=integration -count=1 -timeout=30m -v "$@" .
