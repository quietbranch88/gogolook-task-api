# syntax=docker/dockerfile:1.7
# Base images are pinned by multi-arch index digest (resolved 2026-10-11) so a rebuild
# months later produces the same toolchain and runtime. Bump the tag and digest together.
FROM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
# Module download in its own layer, with the module cache as a BuildKit cache mount:
# it is reused across builds and never written into an image layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
# Only what the binary needs: docs and Compose files never enter the build. (Test files
# under internal/ do, so a test change rebuilds; the cache mounts make that cheap.)
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /api /api
# Numeric form of the base image's nonroot user, so orchestrators that enforce runAsNonRoot
# can verify it (a named USER cannot be verified without /etc/passwd lookups).
USER 65532:65532
# Inside a container the API must listen on all interfaces or published ports never reach it.
# Local `go run` keeps the loopback default; DATABASE_URL is still required at run time.
ENV HTTP_ADDR=:8080
EXPOSE 8080
# No HEALTHCHECK: distroless has no shell or curl, so `docker compose up --wait` only waits for
# this container to be running. Readiness is GET /readyz (200 once PostgreSQL answers); the
# binary also refuses to start until the database and schema are reachable.
ENTRYPOINT ["/api"]
