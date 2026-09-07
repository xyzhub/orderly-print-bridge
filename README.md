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

**Deferred to Phase 4 (NOT built here):** printer discovery (the port-9100 /
mDNS / USB sweep — v1 takes a printer address configured on the server), the
PC download path with the claim code in the filename, signed installers and a
self-updater, the Star dialect, Windows USB, and the cash-drawer kick.

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

Plug the printer in; the OS exposes it as a raw character device.

```bash
# Linux — usually /dev/usb/lp0 (add yourself to the 'lp' group, or use sudo)
./bin/orderly-print-bridge --image sample/receipt.png --printer usb:///dev/usb/lp0

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
| `--full-cut` | off | full cut (`GS V 0`) instead of partial cut (`GS V 66 0`) |
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
internal/transport/           tcp:// · usb:// · file:// delivery, byte counts, GS I
sample/                       real Orderly receipt PNGs
```

## Roadmap (Phase 4, after the client witness)

Printer discovery (port-9100 sweep + mDNS + USB), the PC download path with the
claim code carried in the filename, signed Windows/macOS installers and a
self-updater, the Star dialect, Windows USB via the spooler, and the
cash-drawer kick (`ESC p`, carried by the job's `actions[]`).

## Contract status

The daemon was built against the **written** Orderly contract (the endpoint
table and schema sketch in `docs/product/decisions/2026-09-07-print-bridge-memos.md`,
the S3/S4 session briefs, and counsel findings 4, 6 and 9), because the server
side had not merged yet. Every request/response type lives in
`internal/api/contract.go` with its sources named at the top: reconcile that
one file against `server/api/agent/v1/**` once Phase 1 is on staging.
