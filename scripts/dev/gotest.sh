#!/usr/bin/env bash
# Run Go tests inside the toolchain container.
#   scripts/dev/gotest.sh ./pkg/...                 (unit tests)
#   scripts/dev/gotest.sh -tags integration ./pkg/sqlite/...
# Integration tests that need PostgreSQL read STASH_TEST_PG_DSN; use
# scripts/dev/pg-test.sh to start a throwaway database and run them.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"
docker_go bash -euo pipefail -c '
	mkdir -p ui/v2.5/build && touch ui/v2.5/build/index.html
	[ -f internal/api/generated_exec.go ] || go generate ./cmd/stash
	go test "$@"
' bash "$@"
