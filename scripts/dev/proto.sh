#!/usr/bin/env bash
# Regenerate Go gRPC stubs from proto/stashfs/v1/stashfs.proto into
# pkg/remotefs/stashfsv1. The Rust side generates its own code at build time.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"
docker_go bash -euo pipefail -c '
	PROTOC_VERSION=31.1
	if [ ! -x /go/bin/protoc ]; then
		apt-get update -qq >/dev/null && apt-get install -y -qq unzip >/dev/null
		curl -fsSL -o /tmp/protoc.zip \
			"https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip"
		unzip -q -o /tmp/protoc.zip -d /tmp/protoc
		cp /tmp/protoc/bin/protoc /go/bin/protoc
	fi
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	mkdir -p pkg/remotefs/stashfsv1
	protoc -I proto \
		--go_out=pkg/remotefs/stashfsv1 --go_opt=paths=source_relative \
		--go-grpc_out=pkg/remotefs/stashfsv1 --go-grpc_opt=paths=source_relative \
		stashfs/v1/stashfs.proto
	# flatten stashfs/v1/ into the package directory
	mv pkg/remotefs/stashfsv1/stashfs/v1/*.go pkg/remotefs/stashfsv1/
	rm -rf pkg/remotefs/stashfsv1/stashfs
	echo ">> generated:"; ls pkg/remotefs/stashfsv1
'
