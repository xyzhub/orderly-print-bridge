package escpos

import (
	"errors"
	"image"
	"image/color"
	"testing"
)

func gray(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = 0xff
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w/2; x++ {
			g.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	return g
}

// The resample branch must be unreachable from the daemon: an artifact that is
// not already the printer's width is a failure, not a resize. A silently
// rescaled receipt is a quality regression the venue finds on paper.
func TestNoResampleRejectsAMismatchedWidth(t *testing.T) {
	_, err := Encode(gray(576, 200), Options{Width: 512, Threshold: 128, BandHeight: 128, NoResample: true})
	if err == nil {
		t.Fatal("a 576-dot image at a 512-dot printer must be an error, not a resize")
	}
	var mismatch *WidthMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want *WidthMismatchError, got %T: %v", err, err)
	}
	if mismatch.Got != 576 || mismatch.Want != 512 {
		t.Fatalf("the error must name both widths, got %+v", mismatch)
	}
}

func TestNoResampleAcceptsTheExactWidth(t *testing.T) {
	stream, err := Encode(gray(512, 128), Options{Width: 512, Threshold: 128, BandHeight: 128, NoResample: true})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	img, err := Decode(stream)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != 512 || img.Bounds().Dy() != 128 {
		t.Fatalf("round trip changed the geometry: %v", img.Bounds())
	}
}

// The CLI keeps resizing — a human passing --image and --width is asking for
// one. Only the daemon sets NoResample.
func TestWithoutNoResampleTheCLIStillResizes(t *testing.T) {
	stream, err := Encode(gray(1200, 400), DefaultOptions())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	img, err := Decode(stream)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != 576 {
		t.Fatalf("the CLI path should have resized to 576, got %d", img.Bounds().Dx())
	}
}

// All three supported paper widths are byte-aligned, so NoResample never trips
// on Encode's own round-up.
func TestSupportedWidthsAreByteAligned(t *testing.T) {
	for _, w := range []int{384, 512, 576} {
		if w%8 != 0 {
			t.Fatalf("%d is not a byte boundary", w)
		}
		if _, err := Encode(gray(w, 32), Options{Width: w, Threshold: 128, BandHeight: 128, NoResample: true}); err != nil {
			t.Fatalf("width %d: %v", w, err)
		}
	}
}
