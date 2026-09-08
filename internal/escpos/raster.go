// Package escpos converts a receipt image into an ESC/POS raster byte stream
// (the modern "GS v 0" bit-image command supported by ~all 80mm thermal
// printers — Epson TM-T20/T88 and the XPrinter/Rongta clones) and back again.
//
// Arabic glyph-shaping and RTL are NOT done here: native ESC/POS codepages
// cannot shape Arabic. The universal workaround is to rasterize already-shaped
// text to an image (Orderly renders it server-side) and ship that image as an
// ESC/POS raster. This package is that last mile: image -> 1-bit -> GS v 0.
package escpos

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
)

// ESC/POS control sequences.
var (
	cmdInit     = []byte{0x1b, 0x40}             // ESC @      initialize printer
	cmdAlignCtr = []byte{0x1b, 0x61, 0x01}       // ESC a 1    center
	cmdAlignLft = []byte{0x1b, 0x61, 0x00}       // ESC a 0    left
	cmdFeed     = []byte{0x0a, 0x0a, 0x0a, 0x0a} // paper feed before cut
	cmdPartCut  = []byte{0x1d, 0x56, 0x42, 0x00} // GS V 66 0  partial cut (feed+cut)
	cmdFullCut  = []byte{0x1d, 0x56, 0x00}       // GS V 0     full cut
)

// Cut modes for Options.CutMode.
//
// Observed on the owner's T80C over USB, 2026-09-08: the head IGNORES BOTH
// partial-cut forms (`GS V 66 0` — this package's original hard-coded trailer —
// and `GS V 1`, and `ESC i`) while honouring `GS V 0`. The byte counts proved
// the trailer was sent, so this is the printer's behaviour, not a lost write. A
// cut command is therefore a PER-PRINTER FACT, not a constant, and the default
// is now the form that was seen to work.
//
// `GS V 65 3` cuts on that head too, but it feeds three extra units first and
// the owner read the result as wasted paper (2026-09-08 15:37) — the 4-LF feed
// this package already emits is enough to clear the head.
const (
	// CutFull emits the existing feed followed by GS V 0. The DEFAULT.
	CutFull = "full"
	// CutPartial emits GS V 66 0 (the pre-2026-09-08 behaviour). Keep it for
	// heads that leave the receipt hanging by a tab, which many venues prefer.
	CutPartial = "partial"
	// CutNone emits no cut command at all — for a head with no cutter, or one
	// with a tear bar, where a cut command is at best ignored and at worst an
	// error beep.
	CutNone = "none"
)

// CmdIdentity is the ESC/POS identity query `GS I 1` (transmit printer model
// ID). It is how a candidate on port 9100 is asked to prove it is a thermal
// printer before a welcome slip is sent to it — an office LaserJet answers
// nothing and prints a page of garbage instead (counsel finding 6).
//
// Not every ESC/POS printer implements it. Silence therefore means "not
// identified", never "not a printer".
var CmdIdentity = []byte{0x1d, 0x49, 0x01}

// Options controls the image -> ESC/POS conversion.
type Options struct {
	// Width is the target raster width in dots (80mm @ 203dpi = 576).
	Width int
	// Dither, when true, uses Floyd-Steinberg error diffusion instead of a
	// hard threshold. Threshold is crisper for text/QR; dither preserves
	// photographic gradients (logos) at the cost of a "sandy" look on a
	// low-DPI thermal head.
	Dither bool
	// Threshold is the grey cutoff (0-255) below which a pixel is black.
	// Only used when Dither is false. 128 is the neutral default.
	Threshold uint8
	// Center emits ESC a 1 so the image is centered on the paper.
	Center bool
	// CutMode is "full" (default), "partial" or "none" — see the constants
	// above. An unknown or empty value means "full": a printer profile that
	// arrives from a future server with a mode this build does not know must
	// still cut the paper.
	CutMode string
	// FullCut is the CLI's legacy `--full-cut`: it emits the bare GS V 0. It
	// applies only when CutMode is empty, so a server-supplied mode always
	// wins. Kept because the paper tests are driven by that flag.
	FullCut bool
	// NoResample rejects, instead of rescaling, a source image whose width is
	// not already Width. The server renders the artifact at the printer's
	// widthDots; if what arrives is a different width, something upstream is
	// wrong and quietly rescaling it is a silent quality regression the venue
	// only discovers on paper. The daemon always sets this; the CLI does not,
	// because a human passing --image --width is asking for a resize.
	NoResample bool
	// BandHeight splits the raster into horizontal bands of this many rows,
	// one GS v 0 command each. Cheap printer firmwares choke on a single
	// multi-thousand-row command; banding keeps each command small. 0 selects
	// the default (128).
	BandHeight int
	// LeftMarginDots pads the raster on the left with this many blank dots.
	// #1079: on a ~560-dot head a receipt rendered from column 0 hugs the
	// paper's left edge.
	//
	// The emitted raster is WIDER than Width — Width + LeftMarginDots, rounded
	// up to a byte boundary — and NO CONTENT IS DROPPED (owner ruling,
	// 2026-09-08; it supersedes master-plan task 49's "shift inside the width"
	// wording). The reason is what the two failures cost: the profile's Width is
	// the CONTENT width the manager set (512 on a T80C whose head is ~560), so
	// shifting inside it would silently crop the rightmost columns — that is
	// totals disappearing off a receipt, with nothing on paper to reveal it. An
	// overflow is the visible failure instead: the manager SEES it on the test
	// slip and lowers the margin (S14's stepper caps at 64 and says so).
	//
	// GS L (set-left-margin) is NOT emitted. It is a standard-mode command and
	// no bench reading yet proves this head honours it in raster mode; an
	// unverified command that silently does nothing on one model and shifts
	// twice on another is worse than a shift we can see in --decode. See the
	// README.
	//
	// Bounded to [0, MaxLeftMarginDots]; 0 is byte-identical to the build
	// before this field existed.
	LeftMarginDots int
}

// MaxLeftMarginDots bounds Options.LeftMarginDots (~8mm at 203dpi).
const MaxLeftMarginDots = 64

// DefaultOptions returns sane defaults for an 80mm thermal receipt.
func DefaultOptions() Options {
	return Options{Width: 576, Dither: false, Threshold: 128, Center: false, BandHeight: 128}
}

// WidthMismatchError reports a source image whose width does not match the
// requested raster width while NoResample is set.
type WidthMismatchError struct {
	Got  int
	Want int
}

func (e *WidthMismatchError) Error() string {
	return fmt.Sprintf("image is %d dots wide but this printer prints %d dots; refusing to resample", e.Got, e.Want)
}

// Encode converts a decoded image into a complete, ready-to-print ESC/POS
// stream: init, optional center, the banded GS v 0 raster, a feed, and a cut.
func Encode(src image.Image, opts Options) ([]byte, error) {
	if opts.Width <= 0 {
		return nil, fmt.Errorf("width must be positive, got %d", opts.Width)
	}
	if opts.Width%8 != 0 {
		// Round up to a byte boundary so 8 px pack cleanly per byte.
		opts.Width = (opts.Width + 7) / 8 * 8
	}
	band := opts.BandHeight
	if band <= 0 {
		band = 128
	}

	// 1. grayscale + resize to the target width (aspect-preserving).
	gray := toGray(src)
	sb := gray.Bounds()
	srcW, srcH := sb.Dx(), sb.Dy()
	if srcW == 0 || srcH == 0 {
		return nil, fmt.Errorf("source image is empty")
	}
	dstW := opts.Width
	if opts.NoResample && srcW != dstW {
		return nil, &WidthMismatchError{Got: srcW, Want: dstW}
	}
	dstH := srcH * dstW / srcW
	if dstH < 1 {
		dstH = 1
	}
	resized := resizeGrayBox(gray, dstW, dstH)

	// 2. 1-bit conversion: dither OR threshold. mono[y*W+x] == true means black.
	mono := to1bit(resized, opts.Dither, opts.Threshold)

	// 2b. the left margin: pad the 1-bit mask on the left so the new columns are
	// exactly white and no grey is invented at the seam. This WIDENS the
	// raster; nothing is cropped.
	outW := dstW
	if margin := clampMargin(opts.LeftMarginDots); margin > 0 {
		mono, outW = padLeft(mono, dstW, dstH, margin)
	}

	// 3. emit the stream.
	var buf bytes.Buffer
	buf.Write(cmdInit)
	if opts.Center {
		buf.Write(cmdAlignCtr)
	} else {
		buf.Write(cmdAlignLft)
	}

	bytesPerRow := outW / 8
	for y0 := 0; y0 < dstH; y0 += band {
		rows := band
		if y0+rows > dstH {
			rows = dstH - y0
		}
		writeRasterCommand(&buf, mono, outW, bytesPerRow, y0, rows)
	}

	buf.Write(cmdFeed)
	buf.Write(cutCommand(opts))
	return buf.Bytes(), nil
}

// cutCommand picks the trailer. Order matters: an explicit CutMode from the
// server beats the CLI's legacy boolean, and anything unrecognised falls back to
// a full cut rather than to no cut — an uncut receipt is a receipt the next
// order prints on top of.
func cutCommand(opts Options) []byte {
	switch opts.CutMode {
	case CutPartial:
		return cmdPartCut
	case CutNone:
		// Feed only. The feed is written by the caller, so a tear-bar head
		// still advances the receipt clear of the mechanism.
		return nil
	}
	// "full", the legacy --full-cut boolean, an empty mode and anything a
	// future server invents all land here: cut the paper.
	return cmdFullCut
}

// writeRasterCommand emits one GS v 0 command for rows [y0, y0+rows) of mono.
func writeRasterCommand(buf *bytes.Buffer, mono []bool, width, bytesPerRow, y0, rows int) {
	xL := byte(bytesPerRow & 0xff)
	xH := byte((bytesPerRow >> 8) & 0xff)
	yL := byte(rows & 0xff)
	yH := byte((rows >> 8) & 0xff)
	buf.Write([]byte{0x1d, 0x76, 0x30, 0x00, xL, xH, yL, yH}) // GS v 0 m=0

	for y := y0; y < y0+rows; y++ {
		rowBase := y * width
		for bx := 0; bx < bytesPerRow; bx++ {
			var b byte
			pxBase := rowBase + bx*8
			for bit := 0; bit < 8; bit++ {
				// MSB is the leftmost pixel; set bit = black.
				if mono[pxBase+bit] {
					b |= 1 << (7 - bit)
				}
			}
			buf.WriteByte(b)
		}
	}
}

// clampMargin keeps a server-supplied margin inside [0, MaxLeftMarginDots].
func clampMargin(n int) int {
	if n <= 0 {
		return 0
	}
	if n > MaxLeftMarginDots {
		return MaxLeftMarginDots
	}
	return n
}

// padLeft returns a new 1-bit mask that is n dots wider on the left, with the
// original content intact at x+n. The result is rounded up to a byte boundary
// (the raster command counts BYTES per row), so a margin of 20 on a 512-dot
// image emits 536 columns with 4 blank ones on the right rather than a
// half-byte the firmware would misread.
func padLeft(mono []bool, width, height, n int) ([]bool, int) {
	outW := (width + n + 7) / 8 * 8
	out := make([]bool, outW*height)
	for y := 0; y < height; y++ {
		copy(out[y*outW+n:y*outW+n+width], mono[y*width:(y+1)*width])
	}
	return out, outW
}

// toGray converts any image to 8-bit grayscale (luminance).
func toGray(src image.Image) *image.Gray {
	if g, ok := src.(*image.Gray); ok {
		return g
	}
	b := src.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			g.Set(x, y, color.GrayModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)))
		}
	}
	return g
}

// resizeGrayBox resamples src to dstW x dstH using an area-averaging box
// filter. Box averaging is the right filter for downscaling text/QR: it
// integrates each destination pixel over its full source footprint, which
// keeps thin strokes and QR modules legible where nearest-neighbour would
// drop them. No external image library required.
// With Options.NoResample set (every daemon path) srcW == dstW, so dstH ==
// srcH and this function takes the copy branch below without ever resampling.
func resizeGrayBox(src *image.Gray, dstW, dstH int) *image.Gray {
	sb := src.Bounds()
	srcW, srcH := sb.Dx(), sb.Dy()
	dst := image.NewGray(image.Rect(0, 0, dstW, dstH))
	if dstW == srcW && dstH == srcH {
		copy(dst.Pix, src.Pix)
		return dst
	}
	for dy := 0; dy < dstH; dy++ {
		sy0 := dy * srcH / dstH
		sy1 := (dy + 1) * srcH / dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			sx0 := dx * srcW / dstW
			sx1 := (dx + 1) * srcW / dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sum, n int
			for sy := sy0; sy < sy1; sy++ {
				rowOff := sy * src.Stride
				for sx := sx0; sx < sx1; sx++ {
					sum += int(src.Pix[rowOff+sx])
					n++
				}
			}
			dst.Pix[dy*dst.Stride+dx] = byte(sum / n)
		}
	}
	return dst
}

// to1bit reduces a grayscale image to a black/white mask. The returned slice
// is row-major, length W*H; true means black (ink).
func to1bit(g *image.Gray, dither bool, threshold uint8) []bool {
	w := g.Bounds().Dx()
	h := g.Bounds().Dy()
	mono := make([]bool, w*h)
	if !dither {
		for y := 0; y < h; y++ {
			rowOff := y * g.Stride
			for x := 0; x < w; x++ {
				mono[y*w+x] = g.Pix[rowOff+x] < threshold
			}
		}
		return mono
	}

	// Floyd-Steinberg error diffusion over a float working buffer.
	buf := make([]float64, w*h)
	for y := 0; y < h; y++ {
		rowOff := y * g.Stride
		for x := 0; x < w; x++ {
			buf[y*w+x] = float64(g.Pix[rowOff+x])
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			old := buf[y*w+x]
			var newv float64
			if old < 128 {
				newv = 0 // black
				mono[y*w+x] = true
			} else {
				newv = 255 // white
			}
			err := old - newv
			if x+1 < w {
				buf[y*w+x+1] += err * 7 / 16
			}
			if y+1 < h {
				if x > 0 {
					buf[(y+1)*w+x-1] += err * 3 / 16
				}
				buf[(y+1)*w+x] += err * 5 / 16
				if x+1 < w {
					buf[(y+1)*w+x+1] += err * 1 / 16
				}
			}
		}
	}
	return mono
}
