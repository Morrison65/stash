#!/usr/bin/env bash
# Run an arbitrary command inside the Go toolchain container.
#   scripts/dev/go.sh go build ./...
#   scripts/dev/go.sh make generate-backend
# The container's exit code is propagated.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"
docker_go "$@"
