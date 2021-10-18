#!/usr/bin/env bash

set -eux
cd "$(dirname "${BASH_SOURCE[0]}")"/../..

pushd dev/sg
go build -ldflags "-X main.BuildCommit=$BUILD_COMMIT" -mod=mod . -o ../../ci_sg
popd

./ci_sg ci logs --out=$LOKI_URL
