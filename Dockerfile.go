# syntax=docker/dockerfile:1
#
# Stripped-down Go runtime image for SOBS — the hard-cutover production artifact (replaces the
# Python image at cutover). Multi-stage: a golang builder compiles the cgo-free (purego) binary and
# fetches the pinned native libchdb, then a slim Debian runtime carries just the binary +
# libchdb.so + templates/ + static/. The server self-initializes its chdb schema on first run
# (ensureSchema), so a fresh container with an empty /data volume comes up serving immediately.

FROM golang:1.23-bookworm AS builder
ARG TARGETARCH
# GOPROXY/GOSUMDB default to Go's normal values so unparameterized builds are unaffected; CI on the
# egress-restricted cluster passes --build-arg GOPROXY=<in-cluster Athens> GOSUMDB=off.
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB}
WORKDIR /src
# Resolve modules first so the layer caches across source-only changes.
COPY go/go.mod go/go.sum ./go/
RUN cd go && go mod download
COPY go/ ./go/
# Dynamically linked (CGO_ENABLED=1): purego's dlopen of libchdb needs libc's dynamic linker, which
# a fully static (CGO_ENABLED=0) binary lacks on linux/amd64 — there the server hangs at chdb open.
# The runtime image is glibc-based (debian-slim), so the dynamic binary loads fine.
RUN cd go && CGO_ENABLED=1 go build -trimpath -o /out/sobs ./cmd/sobs

# Pinned chdb-core v26.5.0 — the native build Python chdb 4.1.9 ships (see go/CHDB_PIN.md). The Go
# store must read/write the same on-disk ClickHouse format, so this version is frozen, never latest.
# Arch-matched (amd64 / arm64) so the image is multi-arch like the Python one.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) asset=linux-x86_64-libchdb.tar.gz ;; \
      arm64) asset=linux-aarch64-libchdb.tar.gz ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/libchdb.tar.gz \
      "https://github.com/chdb-io/chdb-core/releases/download/v26.5.0/${asset}"; \
    mkdir -p /out/lib; tar -xzf /tmp/libchdb.tar.gz -C /out/lib; test -f /out/lib/libchdb.so

FROM debian:bookworm-slim
ARG SOBS_BUILD_VERSION=dev
LABEL org.opencontainers.image.title="sobs (go)"
LABEL org.opencontainers.image.description="Simple Observe – Go reimplementation"
LABEL org.opencontainers.image.version="${SOBS_BUILD_VERSION}"
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && addgroup --system --gid 10001 sobs \
    && adduser --system --uid 10001 --ingroup sobs --home /app sobs

COPY --from=builder /out/sobs /app/sobs
COPY --from=builder /out/lib/libchdb.so /app/lib/libchdb.so
COPY templates/ /app/templates/
COPY static/ /app/static/

RUN mkdir -p /data && chown -R sobs:sobs /app /data

ENV CHDB_LIB_PATH=/app/lib/libchdb.so \
    SOBS_DATA_DIR=/data \
    SOBS_HOST=0.0.0.0 \
    SOBS_PORT=4317 \
    SOBS_STATIC_DIR=/app/static \
    SOBS_TEMPLATE_DIR=/app/templates \
    SOBS_BUILD_VERSION=${SOBS_BUILD_VERSION}

USER sobs:sobs
EXPOSE 4317
ENTRYPOINT ["/app/sobs"]
