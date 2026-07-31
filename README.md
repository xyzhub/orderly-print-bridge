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

**Deferred to a later mission (NOT in this MVP):** the Orderly-side job feed,
polling, device pairing, auth tokens, retry/ack, and printer-error surfacing.
Today the bridge prints an image you hand it. The poll→print→ack daemon wraps
this engine later.

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
main.go                       CLI (std flag) + orchestration
internal/escpos/raster.go     image → GS v 0 ESC/POS (resize, 1-bit, banding)
internal/escpos/decode.go     ESC/POS → image (round-trip verification)
internal/transport/           tcp:// · usb:// · file:// delivery
sample/                       real Orderly receipt PNGs
```

## Roadmap (next mission, not this MVP)

Wrap this engine in the Orderly-driven **poll → print → ack** daemon: device
pairing + token auth, the `PrintJob` feed, atomic claim (no duplicate prints),
retry when offline, and printer-error surfacing (paper-out/offline). The server
contract for that is already settled; this binary is the print step it calls.
