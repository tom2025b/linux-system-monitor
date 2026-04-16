# Containerfile — multi-stage build for linux-system-monitor
#
# Build targets:
#   podman build -t linux-system-monitor .
#     → builds the 'final' stage (default): minimal Alpine + msmtp + inotify-tools
#
#   podman build --target=debug -t linux-system-monitor-debug .
#     → builds the 'debug' stage: same as final + bash, curl, strace

# ── Stage 1: Builder ─────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy module manifests before source code so Podman/Docker can cache the
# dependency download layer. If only main.go changes, this layer is reused.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a fully static binary.
# CGO_ENABLED=0: pure Go — no glibc or C runtime dependency in the final image.
# -ldflags="-w -s": strip DWARF debug info and symbol table to shrink the binary.
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-w -s" -o linux-system-monitor .

# ── Stage 2: Debug (optional) ─────────────────────────────────────────────────
# Build with: podman build --target=debug -t linux-system-monitor-debug .
# Run with:   podman run -it --rm linux-system-monitor-debug sh
FROM alpine:3.19 AS debug

# All runtime packages plus shell tools for interactive troubleshooting.
RUN apk add --no-cache \
    msmtp \
    inotify-tools \
    bash \
    curl \
    strace

COPY --from=builder /build/linux-system-monitor /usr/local/bin/linux-system-monitor
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/linux-system-monitor

# Pre-create runtime directories. Bind mounts will shadow them at run time,
# but they serve as sensible defaults if mounts are omitted.
RUN mkdir -p /alerts /logs /config

ENTRYPOINT ["/entrypoint.sh"]

# ── Stage 3: Final (default) ──────────────────────────────────────────────────
# This is the default build target because it is the last FROM in the file.
FROM alpine:3.19 AS final

# Only the two packages needed at runtime:
# msmtp: SMTP client called by the bind-mounted send-report script.
# inotify-tools: provides inotifywait for the alert watcher in entrypoint.sh.
RUN apk add --no-cache \
    msmtp \
    inotify-tools

COPY --from=builder /build/linux-system-monitor /usr/local/bin/linux-system-monitor
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/linux-system-monitor

# Pre-create runtime directories.
RUN mkdir -p /alerts /logs /config

ENTRYPOINT ["/entrypoint.sh"]
