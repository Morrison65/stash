#!/usr/bin/env bash
# Shared helpers for the containerised dev toolchain.
# Nothing is installed on the host: Go, Rust and Node all run in Docker with
# named volumes for module/build caches.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Git Bash on Windows rewrites /paths in arguments; disable that and convert
# the repo root into a form docker understands.
export MSYS_NO_PATHCONV=1
if command -v cygpath >/dev/null 2>&1; then
	HOST_REPO="$(cygpath -w "$REPO_ROOT")"
elif [ -n "${WSL_DISTRO_NAME:-}" ] && ! docker info >/dev/null 2>&1 && command -v docker.exe >/dev/null 2>&1; then
	# Docker Desktop integration may be disabled. The Windows CLI can still
	# reach the same daemon; its bind sources must be Windows paths.
	HOST_REPO="$(wslpath -w "$REPO_ROOT")"
	DOCKER_WINDOWS_CLI=1
	docker() { command docker.exe "$@"; }
else
	HOST_REPO="$REPO_ROOT"
fi

GO_IMAGE="${GO_IMAGE:-golang:1.25-bookworm}"
RUST_IMAGE="${RUST_IMAGE:-stash-fsd-builder:latest}"
NODE_IMAGE="${NODE_IMAGE:-node:24-alpine}"

# Use a TTY only when we have one so that exit codes and output piping work
# in non-interactive shells.
DOCKER_TTY=()
if [ -t 1 ]; then DOCKER_TTY=(-t); fi

# DEV_MOUNT=<host dir> mounts a directory read-only at /mnt/dev, for local
# test data that must not be in the repository (pkg/renamer golden test).
DEV_MOUNT_ARGS=()
if [ -n "${DEV_MOUNT:-}" ]; then
	if [ "${DOCKER_WINDOWS_CLI:-0}" = 1 ]; then
		DEV_MOUNT="$(wslpath -w "$DEV_MOUNT")"
	fi
	DEV_MOUNT_ARGS=(-v "$DEV_MOUNT:/mnt/dev:ro")
fi

docker_go() {
	docker run --rm "${DOCKER_TTY[@]}" \
		--network "${DEV_NETWORK:-bridge}" \
		-v "$HOST_REPO:/src" \
		"${DEV_MOUNT_ARGS[@]}" \
		-v stash-gomod:/go/pkg/mod \
		-v stash-gocache:/root/.cache/go-build \
		-e GOFLAGS="${GOFLAGS:-}" \
		-e STASH_TEST_PG_DSN="${STASH_TEST_PG_DSN:-}" \
		-e RENAMER_GOLDEN_DIR="${RENAMER_GOLDEN_DIR:-}" \
		-w /src \
		"$GO_IMAGE" "$@"
}
