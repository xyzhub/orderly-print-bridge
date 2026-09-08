# Orderly Print Bridge

A single portable Go binary that prints an **Orderly receipt image** to an
**ESC/POS thermal printer** — silently, with **no CUPS, no Avahi, no browser**.

Give it a rendered receipt PNG and a printer target; it converts the image to an
ESC/POS raster (`GS v 0`) and sends the bytes straight to the printer over the
network or USB.

This is **Path 2** of the print-bridge decision (`docs/product/decisions/
2026-07-31-print-bridge-runtime-options.md` in the Orderly repo): one binary per
OS, no host print stack to provision.

---

## What it is (and the MVP boundary)

**This is the print ENGINE, not the platform.** It does exactly one thing:

```
receipt image  →  1-bit raster  →  GS v 0 ESC/POS  →  printer
```

Arabic/RTL shaping and the ZATCA QR are **not** done here — Orderly renders the
receipt to an image server-side (where a real text engine shapes Arabic
correctly), and this bridge just wraps that image in printer commands. Native
ESC/POS codepages cannot shape Arabic; rasterizing the shaped image is the
universal workaround.

The binary now has **two shapes**:

| Shape | Command | What it does |
|---|---|---|
| Print engine (the original) | `--image … --printer …` | one-shot: rasterise an image and send it |
| **Daemon** | `serve` | enroll, then poll Orderly for print jobs, print, acknowledge |

**Built in Phase 4 (this release):** a bounded printer discovery sweep
(`discover`), a checksum-verified `self-update`, the tailnet join from the
enrolment response, a per-printer left margin, and released Linux binaries.

**Still deferred:** mDNS discovery (LD-32 — the `:9100` + `GS I` probe is
enough for v1.1), signed Windows/macOS installers, the Star dialect, Windows USB
via the spooler, and the cash-drawer kick.

---

## Install (Linux box or PC)

One line, from the venue's own Orderly server, which also carries the setup
code:

```bash
curl -fsSL https://<your-orderly>/install.sh | sudo bash -s -- --code ABCDEFGHJK
```

That installs the binary to `/usr/local/bin/orderly-print-bridge`, writes the
`orderly-bridge.service` unit and the nightly `orderly-bridge-update.timer`, and
enrols the device. (The installer route ships with Orderly S13; the assets it
fetches are published by this repo's `release.yml`.)

By hand, from the release:

```bash
TAG=v1.1.0
ARCH=amd64                                   # or arm64
BASE=https://github.com/xyzhub/orderly-print-bridge/releases/download/$TAG
curl -fsSLO $BASE/orderly-print-bridge-linux-$ARCH
curl -fsSLO $BASE/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS     # MUST print: OK
sudo install -m 0755 orderly-print-bridge-linux-$ARCH /usr/local/bin/orderly-print-bridge
```

### Keeping it up to date

```bash
orderly-print-bridge self-update --check   # is there a newer v1?
orderly-print-bridge self-update           # verify the digest, install, restart
```

`self-update` reads the public release feed, **refuses anything outside the v1
major line** (a v2 is a decision, not a download), fetches the asset and the
release's `SHA256SUMS`, verifies the digest, and only then renames the new
binary into place. A digest that does not match aborts and keeps the running
binary. Offline, or GitHub rate-limiting the venue's IP, is a logged skip and
exit 0 — never a failed unit.

The install path of record is the nightly timer `install.sh` writes:

```ini
# /etc/systemd/system/orderly-bridge-update.timer
[Timer]
OnCalendar=daily
RandomizedDelaySec=3h
Persistent=true
```

`serve` also checks every 24 h (± 10% jitter) and, by default, only *reports*
that an update exists in the journal. `serve --auto-update` makes it install and
restart, for installs with no timer.

> **Docker is deprecated.** The `v1.0.0` image stays published as a rollback
> (`systemctl start orderly-bridge` on the box's cached image), but `v1.1.0`
> publishes **no new image** (LD-33): the install path is a binary plus a
> systemd unit.

---

## Build

Requires Go 1.24+ (built and tested with Go 1.26).

```bash
# Build for this machine → ./bin/orderly-print-bridge
go build -o bin/orderly-print-bridge .

# Run the tests
go test ./...
```

### Cross-compile (the whole point of Path 2 — one binary per OS)

Go cross-compiles from any host with zero C toolchain (this project is pure Go,
no cgo):

```bash
# Windows (64-bit Intel/AMD)
GOOS=windows GOARCH=amd64 go build -o dist/orderly-print-bridge-windows-amd64.exe .

# Linux — Raspberry Pi 3/4/5, 64-bit (arm64)
GOOS=linux   GOARCH=arm64 go build -o dist/orderly-print-bridge-linux-arm64 .

# Linux — Pi Zero 2 / older 32-bit ARM
GOOS=linux   GOARCH=arm GOARM=7 go build -o dist/orderly-print-bridge-linux-armv7 .

# Linux — regular x86-64 server/desktop
GOOS=linux   GOARCH=amd64 go build -o dist/orderly-print-bridge-linux-amd64 .

# macOS — Apple Silicon / Intel
GOOS=darwin  GOARCH=arm64 go build -o dist/orderly-print-bridge-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/orderly-print-bridge-darwin-amd64 .
```

Each output is a single self-contained executable — copy it to the target
machine and run it. No runtime, no dependencies to install.

---

## Run

### 1. Print to a network printer (the universal case)

Most 80mm thermal printers with an Ethernet/Wi-Fi port listen for raw ESC/POS on
**TCP port 9100** (JetDirect / RAW).

```bash
./bin/orderly-print-bridge --image sample/receipt.png --printer tcp://192.168.1.50:9100
```

Port 9100 is assumed if you omit it: `--printer tcp://192.168.1.50`.

**Finding the printer's IP:** print a self-test / config page (usually hold the
FEED button while powering on) — the IP is on it. Or check your router's DHCP
client list, or scan the LAN:

```bash
# macOS/Linux — find hosts answering on the raw-print port
nmap -p 9100 --open 192.168.1.0/24
```

### 2. Print to a USB printer

Plug the printer in; Linux exposes a printer-class device as `/dev/usb/lp0`
(the `usblp` driver). All three spellings reach the same code path —
`usb:/dev/usb/lp0` (what `discover` reports and what you paste into Orderly),
`usb:///dev/usb/lp0`, and the bare `/dev/usb/lp0` an older printer row carries.

The device node is **never created**: a `usb:` target that is missing means the
printer is unplugged or off, and creating a file at `/dev/usb/lp0` would swallow
every receipt in silence. The open is bounded at 8 s (a powered-off usblp node
blocks in the kernel forever otherwise) and the write at 30 s, and a permission
error names its fix — the `lp` group.

```bash
# Linux — usually /dev/usb/lp0 (add yourself to the 'lp' group, or use sudo)
./bin/orderly-print-bridge --image sample/receipt.png --printer usb:/dev/usb/lp0

# macOS/BSD — the device shows up under /dev/ (e.g. /dev/cu.usbmodem*)
./bin/orderly-print-bridge --image sample/receipt.png --printer usb:///dev/cu.usbmodemXXXX
```

**Finding the USB device:**

```bash
# Linux
ls -l /dev/usb/lp*          # lp0, lp1, …
# or discover it
dmesg | grep -i printer

# macOS
ls /dev/cu.*                # look for the usbmodem/usbserial entry
```

> **Windows USB** is deferred in this MVP. Windows exposes USB printers through
> the print spooler, not a raw device path, so it needs a spooler call (a small
> follow-up). Windows **network** printers work today via `tcp://…:9100`.

### 3. Dry run — write the ESC/POS bytes to a file (no printer needed)

```bash
./bin/orderly-print-bridge --image sample/receipt.png --out receipt.escpos
```

You can send a captured `.escpos` file to a printer by hand later, e.g.
`cat receipt.escpos > /dev/usb/lp0` on Linux, or `--printer file://…` /
`nc <ip> 9100 < receipt.escpos`.

### 4. Verify a raster without a printer — decode it back to PNG

The binary can decode its own ESC/POS output back to an image so you can eyeball
that the Arabic + QR survived the 1-bit conversion:

```bash
./bin/orderly-print-bridge --decode receipt.escpos --out roundtrip.png
open roundtrip.png
```

---

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--image <path>` | — | receipt PNG/JPEG to print |
| `--printer <target>` | — | `tcp://HOST:9100` · `usb:///dev/usb/lp0` · `file:///path` |
| `--out <path>` | — | dry run: write ESC/POS bytes here instead of printing |
| `--width <dots>` | `576` | raster width. 80mm @ 203dpi = **576**; 58mm = **384** |
| `--dither` | off | Floyd-Steinberg dither instead of a hard threshold |
| `--threshold <0-255>` | `128` | grey cutoff for black (only when not dithering) |
| `--center` | off | center the image on the paper |
| `--full-cut` | off | legacy alias for `--cut-mode full` |
| `--cut-mode <mode>` | `full` | `full` (`GS V 0`) · `partial` (`GS V 66 0`) · `none` (feed only) |
| `--left-margin <n>` | `0` | shift the raster right by *n* dots inside the paper width (0–64) |
| `--decode <path>` | — | decode an ESC/POS file back to a PNG (writes to `--out`) |

### Threshold vs. dither

**Default (threshold) is the right choice for Orderly receipts.** A hard
threshold gives crisp, solid black text and clean, scannable QR modules on a
low-DPI thermal head. Dithering (error diffusion) is only better for
photographic gradients — on a receipt it makes text and the logo look "sandy"
and adds noise to the QR. Verified on the bundled sample: threshold kept the
Arabic and the ZATCA QR sharp; dither degraded both. Reach for `--dither` only
if a receipt carries a real photo.

---

## How it works

1. **Decode** the receipt PNG/JPEG.
2. **Grayscale + resize** to the target dot width with an area-averaging box
   filter (keeps thin Arabic strokes and QR modules legible when downscaling
   from a 1200px render to 576 dots). Width is padded up to a byte boundary.
3. **1-bit** via threshold (default) or Floyd-Steinberg dither.
4. **Emit ESC/POS:** `ESC @` init → optional center → the bitmap as **banded
   `GS v 0`** raster commands (128 rows per band, so cheap printer firmwares
   don't choke on one giant command) → feed → partial cut.
5. **Transport:** raw TCP to `:9100`, or a raw write to a USB/character device,
   or a file.

The `GS v 0` raster bit-image command is supported by ~all 80mm thermal printers
(Epson TM-T20/T88 and the XPrinter/Rongta clones).

---

## The daemon (`serve`)

```bash
# First run on a box: reads the per-flash setup code from /etc/orderly/setup-code,
# reads the DMI serial, enrolls, then polls every 3s.
orderly-print-bridge serve --server https://orderly-staging.fly.dev

# Enroll only (store the token and exit)
orderly-print-bridge enroll --server https://orderly.example --code ABCDEFGHJK

# One poll cycle, for a smoke test
orderly-print-bridge serve --once
```

**Config file** — `{serverUrl, token, deviceId, venueId, printers[]}`, mode
**0600**, at one path per OS:

| OS | Path |
|---|---|
| Linux | `/etc/orderly/bridge.json` |
| macOS | `/Library/Application Support/Orderly/bridge.json` |
| Windows | `%ProgramData%\Orderly\bridge.json` (ACL: SYSTEM + Administrators) |

`ORDERLY_BRIDGE_CONFIG` overrides the path. A world-readable file is rewritten
to 0600 on load. The device token is a redacting type — it cannot reach stdout,
a log line, an error string or an accidental `json.Marshal`; only the config
file and the `Authorization` header ever hold the real value.

**Identity.** The setup code comes from `/etc/orderly/setup-code` (written by
the flash script beside the Tailscale key) plus the hardware serial — Linux
`/sys/class/dmi/id/product_serial` then `board_serial`, Windows
`Win32_BIOS.SerialNumber`, macOS `ioreg`. **A serial that cannot be read is not
an error**: the setup code alone is then the identity. If no code file exists,
the binary serves a setup page on `http://127.0.0.1:47831/` as a last resort.

**Rules the loop will not break**

- **Any byte written makes a failure terminal.** `transport.Send` returns the
  byte count; a stall mid-receipt acks `failed{partial}` and is never
  re-queued. Retrying after a partial write is how one order becomes two legal
  invoices. Only a zero-byte failure is retryable.
- **An artifact whose width ≠ the printer's `widthDots` acks
  `failed{render_failed}` and prints nothing.** It is never resampled.
- **A welcome slip is never sent blind.** Port 9100 is JetDirect, so the first
  answer on the LAN can be an office LaserJet. A slip goes only to a printer
  the server named, or — when it named none — to the single candidate, or to
  the one candidate that answers the ESC/POS identity query `GS I`.
  > **Whether the Bixolon SRP-E300 answers `GS I` is UNKNOWN** as of this
  > build. It has not been tried on a bench. Do not assume it replies.
- **Every job is acked within 60 s** or acked `failed{timeout}`.
- **A 401 stops the loop** and reports "revoked".
- **A discovered printer is never printed to.** The sweep below is a list a
  human reads; routing comes only from the server's assignment.

### Printer discovery

```bash
orderly-print-bridge discover          # what can this machine see?
orderly-print-bridge discover --quiet  # addresses only, one per line

systemctl kill -s USR1 orderly-bridge  # make the daemon sweep right now
```

The daemon sweeps at most **every 10 minutes** (and on `SIGUSR1`) and reports
what it found on the next heartbeat, capped at 50 entries. It is bounded on
every axis, because a discovery feature that saturates a venue's switch during
service is worse than none: **this box's own /24 only** (a /16 is narrowed to
the /24 around us), TCP **9100** only, 32 concurrent dials, a 300 ms dial
timeout, and ~10 s for the whole sweep. Each responder is then asked the ESC/POS
identity query `GS I 1` — 9100 is JetDirect, so an office LaserJet answers too,
and silence is recorded as "not identified", never as "not a printer".

**USB** (Linux only in this release): `/dev/usb/lp*` plus, where sysfs is
readable, `idVendor` / `idProduct` / `manufacturer` / `product` from
`/sys/class/usbmisc/lp<N>/device/..`, reported as
`usb:/dev/usb/lp0 · EPSON TM-T20III · 04b8:0e15`. Inside a container with the
node mapped but no `/sys`, the node is still reported — it is the part you can
print to. Windows USB (via the spooler) stays deferred.

**No mDNS** (LD-32): `_pdl-datastream._tcp` would add a resolver dependency and
an open UDP port for a marginal gain over "who answers on 9100".

### The left margin (`leftMarginDots`)

A per-printer setting on the server (0 by default, capped at 64 dots ≈ 8 mm at
203 dpi), and `--left-margin` on the one-shot CLI. It pads the raster on the
left: the first *n* dot-columns go white and the emitted raster becomes
**`widthDots` + *n* dots wide** (rounded up to a byte boundary).

**Nothing is cropped.** The profile's width is the CONTENT width the manager
set — 512 on a T80C whose head is ~560 — so shifting inside it would silently
take the rightmost columns, and that is totals disappearing off a receipt with
nothing on paper to reveal it. A margin wider than the head is the visible
failure instead: it shows on the test slip, and the manager lowers the margin.
The daemon logs the emitted width on every job with a margin set.

**`GS L` is not emitted.** It is a standard-mode command and no bench reading
yet proves this head honours it in raster mode; an unverified command that does
nothing on one model and shifts twice on another is worse than a shift you can
see in `--decode`. Verify a margin without paper:

```bash
orderly-print-bridge --image sample/receipt.png --width 512 --left-margin 24 --out m.escpos
orderly-print-bridge --decode m.escpos --out m.png   # 536 wide: 24 blank, then all 512
```

### The cut (`cutMode`)

A per-printer setting: **`full`** (the default — the 4-LF feed then `GS V 0`),
`partial` (`GS V 66 0`, the pre-2026-09-08 behaviour), or `none` (feed only, for
a head with no cutter or a tear bar). An absent or unrecognised value means
`full`, because a receipt that is not cut is a receipt the next order prints on
top of. The cut command is a **per-head fact**: the owner's T80C over USB
ignores both partial-cut forms (`GS V 66 0`, `GS V 1`, `ESC i`) while honouring
`GS V 0`, with the byte counts proving the trailer was sent.

### The tailnet join

If the enrolment response carries a `tailscale` block, the daemon runs
`tailscale up --auth-key file:<path> --ssh --accept-dns=false --hostname <name>`
once, **after** enrolment and never blocking it, the poll loop or a print. No
block (every server before Orderly's S13), no `tailscale` binary, or a refused
key are each one log line and a daemon that keeps printing.

**Key hygiene** — the whole point of the design:

- the key goes to `tailscale` through a **0600 file** that is overwritten and
  removed afterwards, never on the command line: `/proc/<pid>/cmdline` is
  world-readable, so an argv key is published to every process on the box;
- it is a redacting type from the JSON decoder onward, so it cannot reach a log
  line, an error string or an accidental `json.Marshal`;
- it is **never written to `bridge.json`**;
- `tailscale`'s own output and error are scrubbed before they are formatted
  (`tailscale up` echoes its flags back on some failures).

`--accept-dns=false` is deliberate: a box must not take DNS from the tailnet,
because its printer lives on the venue's LAN.

## Samples

- `sample/receipt.png` — the branded ZATCA simplified tax invoice (Arabic + QR).
- `sample/receipt_bilingual.png` — the bilingual receipt variant.

Both are genuine Orderly-rendered receipts. Run them straight through:

```bash
./bin/orderly-print-bridge --image sample/receipt.png --printer tcp://<printer-ip>:9100
```

---

## Layout

```
main.go                       one-shot CLI (std flag) + command dispatch
serve.go                      the `serve` / `enroll` commands
internal/api/contract.go      EVERY /api/agent/v1 request+response type (one file,
                              on purpose — reconciling against the merged server
                              handlers is a one-file diff)
internal/api/client.go        the HTTP client for that contract
internal/api/apitest/         httptest fake of the contract, for the e2e tests
internal/config/              bridge.json: three OS paths, 0600 / Windows ACL
internal/enroll/              setup code + hardware serial + the loopback page
internal/bridge/              the poll → artifact → raster → print → ack loop
internal/secret/              a token type that redacts through fmt and json
internal/escpos/raster.go     image → GS v 0 ESC/POS (resize, 1-bit, banding)
internal/escpos/decode.go     ESC/POS → image (round-trip verification)
internal/transport/           tcp:// · usb: · file:// delivery, byte counts, GS I
internal/discover/            the bounded :9100 sweep + Linux USB node scan
internal/tailnet/             `tailscale up` with a key that never touches argv
internal/update/              checksum-verified self-update, pinned to v1
sample/                       real Orderly receipt PNGs
```

## Roadmap (after v1.1)

mDNS discovery, signed Windows/macOS installers, the Star dialect, Windows USB
via the spooler, and the cash-drawer kick (`ESC p`, carried by the job's
`actions[]`).

## Releases

`.github/workflows/release.yml` runs on a `v*` tag (created by a human —
shipping to every client's counter is a decision, not a merge side effect). It
gates on `go vet` + `go test` + a six-target cross-compile, then builds
`linux/amd64` and `linux/arm64` (`CGO_ENABLED=0 -trimpath`, the tag stamped into
`internal/version.Version`), writes one `SHA256SUMS`, and **creates the GitHub
Release** with these assets:

```
orderly-print-bridge-linux-amd64            the installer + self-update asset
orderly-print-bridge-linux-arm64
orderly-print-bridge_<tag>_linux_amd64.tar.gz   binary + README
orderly-print-bridge_<tag>_linux_arm64.tar.gz
SHA256SUMS                                  covers all four
```

**The names are a contract with Orderly's `install.sh`** — renaming one is a 404
on a client's counter that looks like a network failure.

### The box container (deprecated)

```bash
docker run -d --restart=unless-stopped \
  -v /etc/orderly:/etc/orderly \
  -v /sys/class/dmi/id:/sys/class/dmi/id:ro \
  ghcr.io/xyzhub/orderly-print-bridge:v1.0.0 --server https://orderly.example
```

`v1.0.0`'s image stays published as the rollback for the boxes already running
it; **v1.1.0 publishes no image** (LD-33). Note that a container needs the USB
device mapped in (`--device /dev/usb/lp0`) or a USB printer is invisible to it —
that is what "no such file or directory" on the staging box was.

## Contract status

**Reconciled 2026-09-07** against the merged Phase-1 handlers (Orderly branch
`mission/print-bridge-p1` at `9b6d6de3`). Every request/response type lives in
`internal/api/contract.go`, which names the exact handler and util files it
mirrors. What the reconciliation changed: the ack now sends
`{failureReason, lastError}` and no longer the deprecated `error` alias;
`failureReason` is coerced into the server's six stored tokens
(`timeout|offline|partial|no_printer|render_failed|unknown`) with the daemon's
own word kept in `lastError`; enrollment tells apart all six status/code pairs
including the two distinct 409s (`already_claimed` vs `serial_conflict`); the
device token is validated against `odb_` + 43 base64url chars before it is
stored; and the artifact's ladder is honoured — 410 acks `voided`, 409
`lease_lost` abandons the job to the server's re-queue, 501/502 ack
`render_failed` (only 502 retryable).

`welcome` / `test` artifacts answer 501 `kind_not_renderable_yet` until S5's
slip routes merge; the daemon prints their PNGs like any other once they land.
