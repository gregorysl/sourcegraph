#!/usr/bin/env bash

set -eux
cd "$(dirname "${BASH_SOURCE[0]}")"/../..

pushd dev/sg
go build -o ../../ci_sg -ldflags "-X main.BuildCommit=$BUILDKITE_COMMIT" -mod=mod .
popd

./ci_sg ci logs --out=$LOKI_URL --state=""
