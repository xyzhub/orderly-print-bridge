package escpos

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// checkerboard builds a small test image for round-trip checks.
func checkerboard(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%2 == 0 {
				g.SetGray(x, y, color.Gray{Y: 0}) // black
			} else {
				g.SetGray(x, y, color.Gray{Y: 255}) // white
			}
		}
	}
	return g
}

func TestEncodeEmitsInitAndCut(t *testing.T) {
	img := checkerboard(64, 8)
	out, err := Encode(img, Options{Width: 64, Threshold: 128, BandHeight: 128})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte{0x1b, 0x40}) {
		t.Error("stream should start with ESC @ init")
	}
	if !bytes.Contains(out, []byte{0x1d, 0x76, 0x30, 0x00}) {
		t.Error("stream should contain a GS v 0 raster command")
	}
	// Changed 2026-09-08: the default trailer is a full cut, GS V 0. The
	// owner's T80C ignores BOTH partial-cut forms; see the CutMode doc.
	if !bytes.HasSuffix(out, []byte{0x1d, 0x56, 0x00}) {
		t.Error("stream should end with a full cut GS V 0 by default")
	}
}

func TestWidthRoundsToByteBoundary(t *testing.T) {
	img := checkerboard(100, 4)
	out, err := Encode(img, Options{Width: 100, Threshold: 128}) // 100 -> 104
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	if w := got.Bounds().Dx(); w != 104 {
		t.Errorf("width should round up to 104 (byte boundary), got %d", w)
	}
}

func TestBandingCoversFullHeight(t *testing.T) {
	// 300 rows with band=128 -> 3 bands (128+128+44), all reassembled.
	img := checkerboard(64, 300)
	out, err := Encode(img, Options{Width: 64, Threshold: 128, BandHeight: 128})
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(out, []byte{0x1d, 0x76, 0x30, 0x00}); n != 3 {
		t.Errorf("expected 3 GS v 0 bands, got %d", n)
	}
	got, err := Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	if h := got.Bounds().Dy(); h != 300 {
		t.Errorf("decoded height should be 300, got %d", h)
	}
}

func TestRoundTripPreservesPattern(t *testing.T) {
	// Encode a same-width checkerboard (no resize) and decode; the black/white
	// pattern must survive threshold + packing + unpacking exactly.
	img := checkerboard(32, 16)
	out, err := Encode(img, Options{Width: 32, Threshold: 128, BandHeight: 128})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	gg, ok := got.(*image.Gray)
	if !ok {
		t.Fatalf("decoded image is %T, want *image.Gray", got)
	}
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			wantBlack := (x+y)%2 == 0
			gotBlack := gg.GrayAt(x, y).Y == 0
			if wantBlack != gotBlack {
				t.Fatalf("pixel (%d,%d): wantBlack=%v gotBlack=%v", x, y, wantBlack, gotBlack)
			}
		}
	}
}
