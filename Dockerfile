# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26
ARG DEBIAN_VERSION=12
ARG YTDLP_VERSION=2026.08.19

# build

FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src

# dependencies first: they change far less often than the sources, so this
# layer survives most rebuilds
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/yawn ./cmd/yawn

# external tools

FROM debian:${DEBIAN_VERSION}-slim AS tools

ARG YTDLP_VERSION
ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# the standalone build carries its own python, which keeps the runtime image
# free of an interpreter it would otherwise need
RUN set -eux; \
    mkdir -p /out; \
    case "${TARGETARCH}" in \
      amd64) asset="yt-dlp_linux" ;; \
      arm64) asset="yt-dlp_linux_aarch64" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /out/yt-dlp \
      "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${asset}"; \
    chmod 0755 /out/yt-dlp; \
    /out/yt-dlp --version

# runtime

FROM debian:${DEBIAN_VERSION}-slim

# node rather than deno: yt-dlp accepts both, and node costs tens of megabytes
# where deno costs a hundred. yawn enables whichever it finds, so no config is
# needed for it.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        ffmpeg \
        nodejs \
    && rm -rf /var/lib/apt/lists/*

COPY --from=tools /out/yt-dlp /usr/local/bin/yt-dlp
COPY --from=builder /out/yawn /usr/local/bin/yawn

# never root: the container writes into a mounted volume, and files it leaves
# behind should belong to a plain user
RUN groupadd --system --gid 1000 yawn \
    && useradd --system --uid 1000 --gid yawn --home-dir /data --shell /usr/sbin/nologin yawn \
    && mkdir -p /data \
    && chown yawn:yawn /data

USER yawn
WORKDIR /data

# the config, the queue and the work directory all live in the volume, so a
# container is disposable and the archive state is not
VOLUME ["/data"]

# the loopback port `yawn auth --port` uses; publish it to log in to a store
EXPOSE 8765

ENV YAWN_IN_DOCKER=1

# the queue is the thing worth checking: if it cannot be read, nothing works
HEALTHCHECK --interval=60s --timeout=10s --start-period=5s --retries=3 \
    CMD ["yawn", "status"]

ENTRYPOINT ["yawn"]
CMD ["run"]
