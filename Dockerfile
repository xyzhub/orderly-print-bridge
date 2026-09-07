# orderly-print-bridge — the box container.
#
# Published as ghcr.io/xyzhub/orderly-print-bridge:v1, which is what the
# `orderly-box-image` first-boot unit pulls.
#
# ── Why the runtime stage is `scratch` ──────────────────────────────────────
# The container holds a device token that prints at, and reads receipts (PII)
# for, one venue. It runs on hardware a client physically holds, on their LAN.
# There is nothing to gain from a shell inside it: the HOST is Debian 12 with
# full Tailscale SSH, so an operator debugging a box reads `docker logs` from a
# real shell one level up. A shell in here would only be attack surface.
#
# ── Why it runs as root ─────────────────────────────────────────────────────
# Linux DMI serials (`/sys/class/dmi/id/product_serial`) are mode 0400 root-only
# in the kernel, and the config file is 0600 under /etc/orderly. A non-root user
# would silently lose the hardware identity and fall back to the setup code
# alone. Run it with:
#
#   docker run -d --restart=unless-stopped \
#     -v /etc/orderly:/etc/orderly \
#     -v /sys/class/dmi/id:/sys/class/dmi/id:ro \
#     ghcr.io/xyzhub/orderly-print-bridge:v1 --server https://orderly.example
#
# The host's /etc/orderly carries BOTH the per-flash `setup-code` (written by
# the flash script) and the `bridge.json` the daemon writes after enrolling —
# so a container restart never re-enrolls, and re-flashing the host resets it.
#
# Printing is direct to the printer's IP on the LAN; no host networking needed
# unless the printer is discovered by broadcast (Phase 4).

# --- build ------------------------------------------------------------------
# BUILDPLATFORM keeps the compile native and cross-compiles with Go's own
# toolchain, instead of emulating arm64 under QEMU for a ~10x slower build.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# No third-party dependencies (go.mod has zero requires), so there is no
# module-download layer to cache — the source is the whole input.
COPY . .

# CGO_ENABLED=0 is what makes the binary runnable on `scratch`: no libc, no
# dynamic loader. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/orderly-print-bridge . \
 && mkdir -p /out/etc/orderly && chmod 0700 /out/etc/orderly

# Fail the image build, not the client's counter, if the daemon is broken.
# Tests only run when the build is native; cross-compiled targets are gated by
# the release workflow's separate test job.
RUN if [ "${TARGETARCH}" = "$(go env GOHOSTARCH)" ]; then go vet ./... && go test ./...; fi

# --- runtime ----------------------------------------------------------------
FROM scratch

# HTTPS to Orderly needs a trust store; scratch has none.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# The config directory, pre-created at 0700 so a first run on a fresh host does
# not depend on umask.
COPY --from=build /out/etc/orderly /etc/orderly
COPY --from=build /out/orderly-print-bridge /orderly-print-bridge

LABEL org.opencontainers.image.title="Orderly Print Bridge" \
      org.opencontainers.image.description="Polls Orderly for print jobs and prints them to an ESC/POS thermal printer" \
      org.opencontainers.image.source="https://github.com/xyzhub/orderly-print-bridge" \
      org.opencontainers.image.licenses="UNLICENSED"

# `serve` is the daemon; extra flags (--server, --config, --poll) append.
ENTRYPOINT ["/orderly-print-bridge", "serve"]
