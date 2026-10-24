# Multi-target build for the container part of the microservice deployment.
# Build from the repository root:
#   docker build -f deploy/docker/Dockerfile --target api .
#   docker build -f deploy/docker/Dockerfile --target gateway .
#   docker build -f deploy/docker/Dockerfile --target api-plugins \
#     --build-arg PLUGIN_RUNTIME_IMAGE=<existing stash image with plugin deps> .
# (deploy/compose/docker-compose.yml does this for you.)

# Base image for the api-plugins target: any Stash image whose Python
# environment the plugins already rely on. The binaries are static, so the
# base distribution does not matter.
ARG PLUGIN_RUNTIME_IMAGE=alpine:3.22

# ---------------------------------------------------------------------------
# UI build (shared by api and gateway)
# ---------------------------------------------------------------------------
FROM node:24-alpine AS ui
RUN apk add --no-cache make git && npm install -g pnpm
WORKDIR /stash
COPY ui/v2.5/package.json ui/v2.5/pnpm-lock.yaml ui/v2.5/
RUN cd ui/v2.5 && pnpm install --frozen-lockfile
COPY Makefile ./
COPY graphql graphql/
COPY ui ui/
ARG GITHASH
ARG STASH_VERSION
RUN make generate-ui && BUILD_DATE=$(date +"%Y-%m-%d %H:%M:%S") make ui-only

# ---------------------------------------------------------------------------
# Go build
# ---------------------------------------------------------------------------
FROM golang:1.24-alpine AS backend
RUN apk add --no-cache make alpine-sdk
WORKDIR /stash
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go Makefile gqlgen.yml .gqlgenc.yml ./
COPY graphql graphql/
COPY scripts scripts/
COPY pkg pkg/
COPY cmd cmd/
COPY internal internal/
COPY ui ui/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    make generate-backend generate-login-locale
COPY --from=ui /stash/ui/v2.5/build ui/v2.5/build/
ARG GITHASH
ARG STASH_VERSION
# static binaries: they run on any base image (see api-plugins)
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    make flags-release flags-static-pie stash && \
    go build -trimpath -buildmode=pie -tags "sqlite_omit_load_extension osusergo netgo" \
        -ldflags "-s -w -extldflags=-static-pie" -o /stash/stash-pathfix ./cmd/stash-pathfix

# ---------------------------------------------------------------------------
# api: GraphQL API, business logic, scheduler, plugins and scrapers
# ---------------------------------------------------------------------------
FROM alpine:3.22 AS api
# ffmpeg: local-only steps (preview concat, image thumbnails from piped data)
# python3: plugins and script scrapers (python_path: /usr/bin/python3)
RUN apk add --no-cache ca-certificates tzdata ffmpeg vips-tools \
        python3 py3-pip py3-requests py3-yaml curl
COPY --from=backend /stash/stash /usr/bin/stash
COPY --from=backend /stash/stash-pathfix /usr/bin/stash-pathfix
ENV STASH_CONFIG_FILE=/config/config.yml \
    STASH_GENERATED=/generated/ \
    STASH_CACHE=/cache/ \
    STASH_METADATA=/metadata/ \
    STASH_PORT=9999 \
    PIP_BREAK_SYSTEM_PACKAGES=1
VOLUME ["/config", "/database", "/generated", "/cache", "/blobs", "/metadata"]
EXPOSE 9999
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s \
    CMD curl -fsS http://127.0.0.1:9999/healthz || exit 1
ENTRYPOINT ["stash"]

# ---------------------------------------------------------------------------
# api-plugins: the same binaries on top of an existing plugin runtime image
# (e.g. a classic Stash image with Python environments for plugins), so
# plugins keep exactly the packages they were validated with.
# ---------------------------------------------------------------------------
FROM ${PLUGIN_RUNTIME_IMAGE} AS api-plugins
COPY --from=backend /stash/stash /usr/bin/stash
COPY --from=backend /stash/stash-pathfix /usr/bin/stash-pathfix
ENV STASH_CONFIG_FILE=/config/config.yml
EXPOSE 9999
ENTRYPOINT ["stash"]

# ---------------------------------------------------------------------------
# gateway: nginx serving the UI and fronting api + stash-fsd
# ---------------------------------------------------------------------------
FROM nginx:1.28-alpine AS gateway
COPY --from=ui /stash/ui/v2.5/build /usr/share/stash-ui
COPY deploy/gateway/default.conf.template /etc/nginx/templates/default.conf.template
ENV API_UPSTREAM=api:9999 \
    FSD_HTTP_UPSTREAM=host.docker.internal:7701 \
    MAX_UPLOAD_SIZE=1g
EXPOSE 80
