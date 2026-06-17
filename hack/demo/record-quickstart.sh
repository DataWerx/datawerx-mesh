#!/usr/bin/env bash
# This script capture a shareable terminal cast of the quickstart.
# It brings up two kind clusters, then call a Service across them by name.
#
# Produces asciinema docs/assets/quickstart.cast. Convert to a GIF with `agg`:
#   agg docs/assets/quickstart.cast docs/assets/quickstart.gif
#
# Requirements: asciinema, plus the quickstart's own tools - docker, kind,
# kubectl, helm, wg, and the `wireguard` kernel module. Run from the repo root.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
OUT="${1:-${REPO_ROOT}/docs/assets/quickstart.cast}"

command -v asciinema >/dev/null || {
  echo "error: asciinema not found — install it (https://asciinema.org/docs/install)" >&2
  exit 1
}
mkdir -p "$(dirname "$OUT")"

# The script asciinema records: the scripted bring-up, a health check, and the
# cross-c1luster curl that is the mesh connection we're trying to create.
read -r -d '' DEMO <<'SCRIPT' || true
set -e
echo "$ hack/e2e/kind-up.sh"
hack/e2e/kind-up.sh
echo
echo "$ hack/demo/quickstart.sh"
hack/demo/quickstart.sh
echo
SCRIPT

echo "Recording quickstart to ${OUT} …"
asciinema rec --overwrite --command "bash -lc '${DEMO}'" "$OUT"
echo "Done. Convert to GIF with:  agg ${OUT} ${OUT%.cast}.gif"
