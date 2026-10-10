# syntax=docker/dockerfile:1.22@sha256:4a43a54dd1fedceb30ba47e76cfcf2b47304f4161c0caeac2db1c61804ea3c91

# Builder image: the worker plus the toolchain it shells out to when it
# compiles a RustDesk release. Nothing release-specific is baked in: the
# worker keeps the RustDesk clone, vcpkg and its packages, and the cargo and
# pub caches under /data (a volume). See
# docs/superpowers/specs/2026-10-09-k8s-builder-design.md.
#
# Ubuntu 22.04 sets the client's glibc floor (2.35): a binary built here runs
# on Ubuntu 22.04+, Debian 12+ and distributions of that age.

# ---- Worker binary (same build as Dockerfile) ----
FROM golang:1.26-alpine@sha256:c95332c2af86b6d89b91bd0500f4b9529ccbd090a0d1855c6d1ceaa142ae8615 AS worker

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/build-worker ./cmd/worker

# ---- Toolchain ----
FROM ubuntu:22.04@sha256:5ec03bb3441e8b0bf3b4f9cd4629a1ae763010dc3035bb8da3ae6cf026486401

ARG RUSTUP_VERSION=1.28.2
ARG RUSTUP_SHA256=20a06e644b0d9bd2fbdbfd52d42540bdde820ea7df86e92e533c073da0cdd43c
ARG RUST_VERSION=1.98.1
ARG FLUTTER_VERSION=3.24.5
ARG FLUTTER_SHA256=a7c82f551a9eae018e078f6bb186171e5a77920d35a3d75a61d9a593d0a9e4ae
ARG FRB_VERSION=1.80.1

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV DEBIAN_FRONTEND=noninteractive

# Build dependencies of a RustDesk Flutter release on Linux and of its vcpkg
# ports (autotools for mfx-dispatch). Versions come from the pinned base
# image's archive; upgrade picks up security fixes.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      build-essential clang libclang-dev libstdc++-12-dev cmake ninja-build nasm yasm pkg-config \
      autoconf automake libtool \
      git curl ca-certificates python3 zip unzip xz-utils liblzma-dev dpkg-dev file \
      libssl-dev libdbus-1-dev zlib1g-dev \
      libgtk-3-dev libayatana-appindicator3-dev \
      libxcb-randr0-dev libxcb-shape0-dev libxcb-xfixes0-dev libxdo-dev libxfixes-dev \
      libasound2-dev libpulse-dev libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev \
      libpam0g-dev \
 && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*'

ENV RUSTUP_HOME=/opt/rustup \
    CARGO_HOME=/opt/cargo \
    PATH=/opt/cargo/bin:/opt/flutter/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

RUN curl -fsSLo /tmp/rustup-init "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/x86_64-unknown-linux-gnu/rustup-init" \
 && echo "${RUSTUP_SHA256}  /tmp/rustup-init" | sha256sum -c - \
 && chmod +x /tmp/rustup-init \
 && /tmp/rustup-init -y --no-modify-path --profile minimal --default-toolchain "${RUST_VERSION}" \
 && rm /tmp/rustup-init \
 && cargo install --locked --version "${FRB_VERSION}" --features uuid flutter_rust_bridge_codegen \
 && rm -rf /opt/cargo/registry /opt/cargo/git

# precache leaves a root-only pub cache the runtime user cannot read (it uses
# PUB_CACHE under /data); dropping it also drops sample credentials in package
# docs that secret scanners flag.
RUN curl -fsSLo /tmp/flutter.tar.xz "https://storage.googleapis.com/flutter_infra_release/releases/stable/linux/flutter_linux_${FLUTTER_VERSION}-stable.tar.xz" \
 && echo "${FLUTTER_SHA256}  /tmp/flutter.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/flutter.tar.xz -C /opt \
 && rm /tmp/flutter.tar.xz \
 && flutter config --no-analytics \
 && flutter precache --linux \
 && rm -rf /root/.pub-cache

# flutter_rust_bridge_codegen formats the Rust it generates with rustfmt,
# which the minimal profile leaves out.
RUN rustup component add rustfmt

COPY --from=worker /out/build-worker /app/build-worker
COPY config.yaml /app/config.yaml

# The Flutter tool locks and writes its own bin/cache on every run, so the
# runtime user owns /opt/flutter; Rust and the codegen stay root-owned.
RUN groupadd -g 65532 nonroot \
 && useradd -u 65532 -g 65532 -d /data -M -s /usr/sbin/nologin nonroot \
 && chown -R 65532:65532 /opt/flutter \
 && mkdir -p /data \
 && chown 65532:65532 /data

ENV HOME=/data \
    CARGO_HOME=/data/cargo \
    PUB_CACHE=/data/pub-cache \
    FLUTTER_SUPPRESS_ANALYTICS=true

USER 65532:65532
WORKDIR /app
ENTRYPOINT ["/app/build-worker"]
CMD ["--config", "/app/config.yaml"]
