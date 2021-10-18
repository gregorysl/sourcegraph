#!/usr/bin/env bash

set -eu
cd "$(dirname "${BASH_SOURCE[0]}")"/../..

echo "--- building sg"
pushd dev/sg
go build -o ../../ci_sg -ldflags "-X main.BuildCommit=$BUILDKITE_COMMIT" -mod=mod .
popd

echo "--- uploading logs if build failed"
./ci_sg ci logs --out=$LOKI_URL --state="failed"
