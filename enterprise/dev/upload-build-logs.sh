#!/usr/bin/env bash

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"/../..

echo "--- :cog: building sg"

(
  set -x
  pushd dev/sg
  go build -o ../../ci_sg -ldflags "-X main.BuildCommit=$BUILDKITE_COMMIT" -mod=mod .
  popd
)

echo "--- :arrow_up: uploading logs if build failed"

./ci_sg ci logs --out=$LOKI_URL --state="failed"
