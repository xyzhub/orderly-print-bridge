package escpos

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// blackImage returns a fully black w x h image: every column is ink, so any
// white column in the decoded output can only have come from the margin.
func blackImage(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = 0
	}
	return g
}

// The load-bearing property of the margin (owner ruling 2026-09-08, superseding
// master-plan task 49): the raster is Width + margin wide, the first N columns
// are white, and NO CONTENT IS DROPPED — the profile width is the content
// width, so cropping it would take totals off a receipt with nothing on paper
// to show for it.
func TestLeftMarginWidensAndWhitensFirstColumns(t *testing.T) {
	const (
		width  = 512
		height = 32
		margin = 24
	)
	opts := DefaultOptions()
	opts.Width = width
	opts.NoResample = true
	opts.LeftMarginDots = margin

	stream, err := Encode(blackImage(width, height), opts)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	img, err := Decode(stream)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != width+margin {
		t.Fatalf("raster is %d dots wide, want %d (width + margin)", got, width+margin)
	}
	for x := 0; x < margin; x++ {
		if isInk(img, x, 0) {
			t.Fatalf("column %d is ink; the first %d columns must be blank", x, margin)
		}
	}
	if !isInk(img, margin, 0) {
		t.Fatalf("column %d is blank; the image should start exactly at the margin", margin)
	}
	// Nothing was cropped: all `width` source columns are still ink, out to the
	// last one, which now sits at width+margin-1.
	for y := 0; y < height; y++ {
		for x := margin; x < width+margin; x++ {
			if !isInk(img, x, y) {
				t.Fatalf("blank dot at (%d,%d): the margin dropped content", x, y)
			}
		}
	}
}

// A zero margin must be byte-identical to the build before the field existed.
func TestZeroMarginIsByteIdentical(t *testing.T) {
	opts := DefaultOptions()
	opts.Width = 384
	opts.NoResample = true
	src := blackImage(384, 8)

	before, err := Encode(src, opts)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	opts.LeftMarginDots = 0
	after, err := Encode(src, opts)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("LeftMarginDots=0 changed the byte stream")
	}
}

func TestMarginIsClamped(t *testing.T) {
	if got := clampMargin(-5); got != 0 {
		t.Fatalf("a negative margin must clamp to 0, got %d", got)
	}
	if got := clampMargin(500); got != MaxLeftMarginDots {
		t.Fatalf("a margin past the cap must clamp to %d, got %d", MaxLeftMarginDots, got)
	}
}

// A margin that does not land on a byte boundary still emits whole bytes per
// row — the raster command counts bytes, and a half-byte is a garbled row.
func TestOffByteMarginRoundsUpToAByteBoundary(t *testing.T) {
	opts := DefaultOptions()
	opts.Width = 512
	opts.NoResample = true
	opts.LeftMarginDots = 20
	out, err := Encode(blackImage(512, 8), opts)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	img, err := Decode(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != 536 {
		t.Fatalf("512 + 20 must round up to 536 dots, got %d", got)
	}
	for x := 0; x < 20; x++ {
		if isInk(img, x, 0) {
			t.Fatalf("column %d must be blank", x)
		}
	}
	if !isInk(img, 531, 0) {
		t.Fatal("the last content column (531) lost its ink")
	}
}

func isInk(img image.Image, x, y int) bool {
	return color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y < 128
}

// The exact trailer per cut mode (owner's T80C finding, 2026-09-08). The bytes
// are the contract: this head ignores both partial-cut forms, so a venue whose
// receipts stopped being cut is a venue on the wrong mode.
func TestCutModeTrailerBytes(t *testing.T) {
	feed := []byte{0x0a, 0x0a, 0x0a, 0x0a}
	cases := []struct {
		mode    string
		fullCut bool
		want    []byte
	}{
		{mode: "", want: append(append([]byte{}, feed...), 0x1d, 0x56, 0x00)},               // default = full
		{mode: CutFull, want: append(append([]byte{}, feed...), 0x1d, 0x56, 0x00)},          // GS V 0
		{mode: CutPartial, want: append(append([]byte{}, feed...), 0x1d, 0x56, 0x42, 0x00)}, // GS V 66 0
		{mode: CutNone, want: feed}, // feed only
		{mode: "", fullCut: true, want: append(append([]byte{}, feed...), 0x1d, 0x56, 0x00)}, // legacy --full-cut
		{mode: "sideways", want: append(append([]byte{}, feed...), 0x1d, 0x56, 0x00)},        // unknown = cut anyway
	}
	for _, tc := range cases {
		opts := DefaultOptions()
		opts.Width = 64
		opts.NoResample = true
		opts.CutMode = tc.mode
		opts.FullCut = tc.fullCut
		out, err := Encode(blackImage(64, 8), opts)
		if err != nil {
			t.Fatalf("cutMode %q: %v", tc.mode, err)
		}
		if !bytes.HasSuffix(out, tc.want) {
			t.Fatalf("cutMode %q (fullCut=%t): trailer is % x, want it to end % x",
				tc.mode, tc.fullCut, out[len(out)-8:], tc.want)
		}
		if tc.mode == CutNone && bytes.Contains(out, []byte{0x1d, 0x56}) {
			t.Fatalf("cutMode none emitted a GS V cut command")
		}
	}
}
