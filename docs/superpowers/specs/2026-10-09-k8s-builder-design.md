# build-worker: compile custom clients on Kubernetes

Status: design approved in conversation 2026-10-09, pending review of this document.
Scope: this repo (worker code, a new builder image, CI) and the `build-worker` subchart in
k8s-deploy. rustdesk-api and the console are unchanged.

## Background

The worker turns a console request into a custom RustDesk client:

1. A **pre-build** checks out a RustDesk release, patches the signing key and compiles it:
   - flutter_rust_bridge codegen;
   - `cargo build --release`;
   - `flutter build linux`.

   The output is cached in S3 per platform, arch and version.
2. A **bundle** adds a signed `custom.txt` and packages a `.deb` or `.zip`.

Since PRs #14 and #15 the worker owns its RustDesk clone (`rustdesk-src-dir`, cloned from
`rustdesk-repo-url`). The deb takes each release's own `res/` files and `Depends`, and a
1.5.0 build was verified end to end on a host.

The published image (`ghcr.io/rxxozqfoe/rustdesk-build-worker`) ships only the Go binary on
wolfi-base. Cargo, Flutter, vcpkg and the native libraries are expected from the host.
So the worker cannot compile anything in Kubernetes, where the k8s-deploy chart runs it
(disabled by default, image `0.0.1`). Three other facts shape the design:

- **vcpkg is tied to the release.** 1.4.9 pins vcpkg baseline `120deac`, 1.5.0 pins `9e593bb`
  with different ports (`res/vcpkg` overlays). A toolchain baked for one release breaks the next.
  The 1.5.0 host build failed until vcpkg was re-cloned at the new baseline and its packages
  rebuilt.
- **glibc decides where the client runs.** A binary only runs on a glibc at least as new as
  the one it was built against. The host build (Ubuntu 24.04, glibc 2.39) does not run on
  Ubuntu 22.04 or Debian 12. Upstream builds on Ubuntu 18.04.
- **Measured cost of a 1.5.0 build:**

  | | First build | With caches |
  |---|---|---|
  | Time | 30 min+ (vcpkg 20–30 min) | ~10 min |
  | Peak memory | ~12 GB (the LTO link) | ~12 GB |
  | Disk (sources, caches, target) | 15–20 GB | 15–20 GB |

## Goals

- With the chart's build-worker enabled, creating a custom client in the console completes the
  pre-build and bundle inside the cluster and yields an installable `.deb`.
- No host directory, host toolchain or manual vcpkg step is needed. A new upstream release
  builds without rebuilding the image as long as it keeps Flutter 3.24.x and a stable-Rust-
  compatible toolchain.
- The client runs on glibc 2.35 and newer: Ubuntu 22.04+, Debian 12+ and other distributions of
  that age.
- Every resource figure is a chart value. The defaults fit the measurements above.

Non-goals: arm64, Windows and macOS builds; more than one worker replica; scaling to zero or
running builds as Jobs (the worker stays a long-running Deployment); compiling RustDesk in CI.

## Design

### 1. Builder image (`Dockerfile.builder`)

The new image is published next to the existing slim one, as tag `<version>-builder`, amd64 only.

- **Base:** `ubuntu:22.04`, pinned by digest (glibc 2.35). `apt-get upgrade` at build time.
- **apt packages**, the set the host build needed:
  - build-essential, clang, libclang-dev, cmake, ninja-build, nasm, yasm, pkg-config;
  - autoconf, automake, libtool (needed by vcpkg's mfx-dispatch);
  - git, curl, ca-certificates, python3, zip, unzip, xz-utils, dpkg-dev;
  - libgtk-3-dev, libayatana-appindicator3-dev (Flutter's tray plugin);
  - libxcb-randr0-dev, libxcb-shape0-dev, libxcb-xfixes0-dev, libxdo-dev, libxfixes-dev;
  - libasound2-dev, libpulse-dev, libgstreamer1.0-dev, libgstreamer-plugins-base1.0-dev;
  - libpam0g-dev (for releases before 1.5.0).
- **Toolchains under `/opt`**, read-only, each version a Dockerfile `ARG`:
  - Rust via rustup, default 1.98.1 (verified with 1.5.0), `RUSTUP_HOME=/opt/rustup`;
  - Flutter 3.24.5 from the official tarball with a sha256 check, then
    `flutter precache --linux` and analytics off;
  - `flutter_rust_bridge_codegen` 1.80.1 via `cargo install --locked`, into `/opt/cargo/bin`.
- **The worker binary** comes from the same Go build stage as the slim image.
- **Runtime:** uid/gid 65532, as in the slim image. `CARGO_HOME`, `PUB_CACHE` and `HOME`
  point under `/data`. All writes go to `/data` (the PVC) or `/tmp`.

Nothing release-specific is in the image: no RustDesk source and no vcpkg ports or packages.
Section 2 covers those. The expected size is about 5 GB.

### 2. Worker manages vcpkg (`internal/builder`, `internal/config`)

New config:

| Key | Default | Meaning |
|---|---|---|
| `build.vcpkg-dir` | `/var/lib/build-worker/vcpkg` | The worker's own vcpkg clone, created if missing |
| `build.vcpkg-repo-url` | `https://github.com/microsoft/vcpkg.git` | Where it is cloned from |
| `build.jobs` | `0` | Build parallelism; sets `CARGO_BUILD_JOBS` and `VCPKG_MAX_CONCURRENCY` when non-zero |

A new pre-build step, "Preparing vcpkg dependencies", runs after the RustDesk checkout and
before codegen:

1. Read the baseline commit from the checkout's `vcpkg.json`
   (`vcpkg-configuration.default-registry.baseline`). A missing baseline fails the job.
2. Make sure the vcpkg clone has that commit:
   - clone `vcpkg-repo-url` into `vcpkg-dir` when missing;
   - `git fetch` when the commit is not there;
   - then `git checkout --force <baseline>` and `bootstrap-vcpkg.sh -disableMetrics`.

   Upstream CI does the same: the vcpkg tool and the port scripts match the release's baseline.
3. In the RustDesk checkout, run
   `vcpkg install --x-install-root=<vcpkg-dir>/installed --clean-after-build` with
   `VCPKG_DEFAULT_BINARY_CACHE=<vcpkg-dir>/binary-cache`.

   Switching between releases restores packages already built from the binary cache instead
   of compiling them again. The output goes to the build log.
4. Every later build command runs with `VCPKG_ROOT=<vcpkg-dir>`. The old "VCPKG_ROOT is not set"
   check is removed.

Two existing patterns are reused:

- git commands that talk to a remote run through the same redaction as the RustDesk clone, so
  credentials in `vcpkg-repo-url` never reach logs or job errors;
- the vcpkg git operations hold `gitMu`.

Each failing step fails the job with the step named.

A host deployment uses the same code: it sets `vcpkg-dir` to a directory of its choice. The
existing host clone at `/home/user/build-worker-data/vcpkg` works as is.

### 3. Chart (`k8s-deploy/charts/build-worker`)

- **Image:** the default tag becomes the builder tag (`0.1.0-builder`).
- **`persistence`**, on by default:
  - `size: 40Gi`, `accessModes: [ReadWriteOnce]`, `storageClass: ""` (the cluster
    default), or `existingClaim`;
  - mounted at `/data`; it holds the RustDesk clone, vcpkg, the cargo and pub caches and
    the build logs.
- **`/tmp`** is an `emptyDir` with `sizeLimit: 5Gi`, for bundle staging and pre-build tarballs.
- **Deployment:**
  - `strategy: Recreate`, because a RWO volume cannot be shared by old and new pods;
  - `replicas` stays 1, and its comment says more workers need storage of their own;
  - `securityContext` runs as and sets `fsGroup` to 65532.
- **Resources:** requests `cpu: 1`, `memory: 4Gi`; limit `memory: 16Gi`. A new `build.jobs` value
  defaults to 0.
- **Config (secret.yaml):**
  - `worktree-dir` goes away;
  - writes `rustdesk-src-dir: /data/rustdesk`, `vcpkg-dir: /data/vcpkg` and `log-dir: /data/logs`;
  - both repo URLs and `jobs` come from values.
- **NetworkPolicy:**
  - The worker reaches many hosts: github.com, crates.io, pub.dev, and every vcpkg port's own
    download site. NetworkPolicy cannot filter by name.
  - A new `networkPolicy.internetEgress` (`enabled: false`) adds egress to `0.0.0.0/0` except
    10/8, 172.16/12 and 192.168/16, on TCP 443 and 80. The CIDRs and ports are values.
  - The default stays closed, per the chart's deny-by-default rule.
  - A proxy is the other route: `HTTPS_PROXY` in `extraEnv`, plus `extraEgress` to the proxy.
  - `NOTES.txt` warns when the worker is enabled under NetworkPolicy and neither
    `internetEgress` nor `extraEgress` is set.
- **Versions:** the umbrella chart goes to 0.3.0 and the subchart is bumped. The README gets an
  "enabling the build worker" section covering resources, storage and egress.

### 4. CI and release (this repo)

- **First, the existing Trivy red.** The slim image fails its gate on pre-existing findings:
  wolfi-base, `golang.org/x/{crypto,net,text}` and Go stdlib 1.26.4. Bump the modules, the
  Go toolchain and the wolfi-base digest (Renovate PR #4). No image can ship until this is
  green.
- **Validation (`_validate.yml`):**
  - hadolint and the FROM digest-pin gate cover `Dockerfile.builder` too.
  - The cosign base-image check covers only the slim image. `ubuntu:22.04` is a Docker Official
    Image without a Sigstore signature, so the builder relies on its digest pin. The script
    lists this exception with the reason.
  - The builder image is built and Trivy-scanned on PRs only when `Dockerfile.builder` or its
    inputs change (path filter), because a build takes ~15 minutes.
  - Fixable HIGH/CRITICAL findings are fixed. An unfixable finding inside a vendored toolchain
    (Flutter, Rust) goes into `.trivyignore` with a reason and an expiry date, only after the
    maintainer approves that entry.
- **Publish (`publish.yml`, on `v*.*.*` tags):**
  - A `build-builder` job builds the amd64 builder image and pushes `<version>-builder`. It has
    the same SBOM, provenance, cosign signature and verify steps as the slim image.
  - A smoke step runs `cargo --version`, `flutter --version`, `flutter_rust_bridge_codegen
    --version` and `build-worker --help` inside the published image.
- **Version bumps:** Flutter, Rust and flutter_rust_bridge are `ARG`s, bumped by hand. Renovate
  keeps base-image digests current.

## Error handling

- vcpkg clone/fetch/bootstrap/install failures fail the pre-build job with the step named. The
  build log carries the tool output, with repository credentials redacted.
- An out-of-memory kill during the LTO link shows as a failed job and a restarted pod. The README
  names `build.jobs` and the memory limit as the knobs.
- Without internet egress, the first clone fails, and the job error says the clone failed. The
  NOTES warning points at the cause before that happens.

## Testing

No `go test` / `cargo test` runs (repository rule).

- **Worker code:** a throwaway driver program, not committed, runs the builder package on this
  host twice:
  - against the existing vcpkg clone, to check the cache path;
  - from an empty `vcpkg-dir`, to check clone → checkout → bootstrap → install end to end.
- **Image:** on this host, `docker run` the builder image with a data volume standing in for the
  PVC, and run a full 1.5.0 pre-build and bundle from empty directories. Then check:
  - the deb's required glibc, with `objdump -T`: at most 2.35;
  - its `Depends`;
  - the patched key.

  Disk: about 25 GB more (60 GB free). Memory and disk are watched by the same watchdog used for
  the host build.
- **Chart:**
  - `ci/validate.sh`, with new render scenarios for persistence on/off/existingClaim,
    internetEgress, and the NOTES warning;
  - a kind install that checks the pod starts, mounts the PVC as uid 65532 and can write to it.

  No full compile on kind.

## Rollout

1. PR: CI green (Trivy fixes).
2. PR: vcpkg management in the worker.
3. PR: builder image + CI.
4. Tag `v0.1.0` → slim `0.1.0` and `0.1.0-builder`.
5. k8s-deploy PR: subchart changes, chart 0.3.0, published after merge.
