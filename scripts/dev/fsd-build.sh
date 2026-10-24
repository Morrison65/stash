#!/usr/bin/env bash
# Test and cross-compile stash-fsd (windows/stash-fsd) inside Docker.
#
#   scripts/dev/fsd-build.sh               cargo test (Linux) + release build of
#                                          stash-fsd.exe -> windows/stash-fsd/dist/
#   scripts/dev/fsd-build.sh <cargo args>  run an arbitrary cargo command in the
#                                          toolchain container, e.g.
#                                          scripts/dev/fsd-build.sh test paths::
#
# The builder image (stash-fsd-builder:latest) is built from
# windows/stash-fsd/Dockerfile.build if it does not exist yet; set
# FSD_REBUILD_IMAGE=1 to force a rebuild. Cargo caches live in named volumes.
# The container's exit code is propagated.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

FSD_DIR="$REPO_ROOT/windows/stash-fsd"

if [ -n "${FSD_REBUILD_IMAGE:-}" ] || ! docker image inspect "$RUST_IMAGE" >/dev/null 2>&1; then
	echo ">> building $RUST_IMAGE"
	if command -v cygpath >/dev/null 2>&1; then
		ctx="$(cygpath -w "$FSD_DIR")"
	else
		ctx="$FSD_DIR"
	fi
	docker build -t "$RUST_IMAGE" -f "$ctx/Dockerfile.build" "$ctx"
fi

docker_rust() {
	docker run --rm "${DOCKER_TTY[@]}" \
		-v "$HOST_REPO:/src" \
		-v stashms-cargo-registry:/usr/local/cargo/registry \
		-v stashms-cargo-git:/usr/local/cargo/git \
		-v stashms-fsd-target:/target \
		-e CARGO_TARGET_DIR=/target \
		-e CARGO_TERM_COLOR="${CARGO_TERM_COLOR:-auto}" \
		-w /src/windows/stash-fsd \
		"$RUST_IMAGE" "$@"
}

if [ "$#" -gt 0 ]; then
	docker_rust cargo "$@"
	exit $?
fi

docker_rust bash -euo pipefail -c '
	echo ">> cargo test (linux)"
	cargo test
	echo ">> cargo build --release --target x86_64-pc-windows-gnu"
	cargo build --release --target x86_64-pc-windows-gnu
	exe=/target/x86_64-pc-windows-gnu/release/stash-fsd.exe
	mkdir -p dist
	cp "$exe" dist/stash-fsd.exe
	echo ">> DLL dependencies:"
	x86_64-w64-mingw32-objdump -p dist/stash-fsd.exe | grep "DLL Name" || true
	ls -l dist/stash-fsd.exe
	echo ">> OK"
'
