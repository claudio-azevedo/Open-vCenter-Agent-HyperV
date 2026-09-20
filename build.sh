#!/usr/bin/env bash
#
# Build the ovc-agent Windows binary (for macOS / Linux / WSL).
#
#   ./build.sh                 -> ./ovc-agent.exe  (windows/amd64)
#   ./build.sh dist/agent.exe  -> custom output path
#   GOARCH=arm64 ./build.sh    -> override target arch
#
set -euo pipefail
cd "$(dirname "$0")"

GOOS="${GOOS:-windows}"
GOARCH="${GOARCH:-amd64}"
OUT="${1:-ovc-agent.exe}"

version="$(sed -nE 's/.*AgentVersion = "([^"]+)".*/\1/p' internal/tasks/agent_status.go)"

echo "Building ovc-agent v${version}  (${GOOS}/${GOARCH})  ->  ${OUT}"
GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o "$OUT" ./cmd/agent

echo "Done: $(ls -lh "$OUT" | awk '{printf "%s  %s\n", $NF, $5}')"
