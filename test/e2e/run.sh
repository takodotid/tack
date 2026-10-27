#!/usr/bin/env bash
# Runs the end-to-end test: builds the environment, runs the scenarios,
# and deletes the environment unless TACK_E2E_KEEP=1.
set -euo pipefail

E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$E2E_DIR/../.." && pwd)

for tool in docker kind kubectl flux helm go git; do
  command -v "$tool" >/dev/null || { echo "e2e: $tool is not installed (see README.md)" >&2; exit 1; }
done

cleanup() {
  if [[ "${TACK_E2E_KEEP:-0}" != 1 ]]; then
    "$E2E_DIR/env/down.sh"
  fi
}
trap cleanup EXIT

"$E2E_DIR/env/down.sh"
"$E2E_DIR/env/up.sh"
cd "$REPO"
go test -tags e2e -count=1 -v -timeout 90m ./test/e2e/ "$@"
