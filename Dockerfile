# syntax=docker/dockerfile:1.24@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89

# ---- Build stage ----
# Pure-Go service: no cgo (no sqlite / C bindings), so we build a fully
# static binary with CGO_ENABLED=0. No build-base / musl-dev needed.
FROM golang:1.26-alpine@sha256:f23e8b227fb4493eabe03bede4d5a32d04092da71962f1fb79b5f7d1e6c2a17f AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache module downloads on the dependency manifests alone, so source-only
# edits don't bust the layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped, trimmed binary. CGO_ENABLED=0 guarantees no dynamic
# libc dependency, so the binary runs on any glibc/musl runtime.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
        -ldflags="-s -w" \
        -o /out/build-worker ./cmd/worker

# ---- Runtime stage ----
# wolfi-base (Chainguard) — a small, sigstore-signed base that ships a
# shell (/bin/sh) and core utilities. A shell is REQUIRED here: the worker
# shells out to an external RustDesk build toolchain (cargo, flutter,
# vcpkg, git, dpkg-deb, ...) at run time, so distroless is not usable.
#
# This image ships only the worker BINARY. The heavy build toolchain is
# NOT baked in; it is expected to be provided by the runtime environment
# (mounted onto the host, or layered into a derived image). See the PR
# description for the runtime-toolchain caveat.
FROM cgr.dev/chainguard/wolfi-base@sha256:cb2fc9d795bf874e660dbc25958cfa699b55e789235896ec7c354c57d3e3a704

WORKDIR /app

# Default config; override at runtime with a mounted config + --config.
COPY --from=builder /out/build-worker /app/build-worker
COPY config.yaml /app/config.yaml

# wolfi-base ships a "nonroot" account (uid/gid 65532). Drop privileges.
USER 65532:65532

ENTRYPOINT ["/app/build-worker"]
CMD ["--config", "/app/config.yaml"]
