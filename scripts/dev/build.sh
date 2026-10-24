#!/usr/bin/env bash
# Generate GraphQL code (if needed) and compile + vet every Go package.
# Exits non-zero on any failure.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"
docker_go bash -euo pipefail -c '
	mkdir -p ui/v2.5/build && touch ui/v2.5/build/index.html
	if [ ! -f internal/api/generated_exec.go ] || [ -n "${FORCE_GENERATE:-}" ]; then
		echo ">> go generate ./cmd/stash"
		go generate ./cmd/stash
	fi
	echo ">> go build ./..."
	go build ./...
	echo ">> go vet ./..."
	go vet ./...
	echo ">> OK"
'
