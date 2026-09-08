package main

import (
	"bytes"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xyz/orderly-print-bridge/internal/escpos"
)

// The one-shot CLI is what the paper tests drive by hand
// (`--image … --printer … --width 512`). Adding the daemon must not change it.
func TestCLIImageToPrinterTargetStillWorks(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "sink.escpos")
	if err := runPrint([]string{"--image", "sample/receipt.png", "--printer", "file://" + sink, "--width", "512"}); err != nil {
		t.Fatalf("print: %v", err)
	}
	raw, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	img, err := escpos.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != 512 {
		t.Fatalf("the 512-dot render came out %d dots wide", got)
	}
}

// The --decode round trip is the oracle the daemon's tests lean on; verify the
// CLI half of it end to end at client #1's width.
func TestCLIDryRunAndDecodeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	escposPath := filepath.Join(dir, "receipt.escpos")
	pngPath := filepath.Join(dir, "roundtrip.png")

	if err := runPrint([]string{"--image", "sample/receipt.png", "--width", "512", "--out", escposPath}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if err := runPrint([]string{"--decode", escposPath, "--out", pngPath}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	f, err := os.Open(pngPath)
	if err != nil {
		t.Fatalf("open roundtrip: %v", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode roundtrip png: %v", err)
	}
	if cfg.Width != 512 {
		t.Fatalf("round-trip PNG is %d dots wide, want 512", cfg.Width)
	}
}

func TestCLIRejectsBadInput(t *testing.T) {
	if err := runPrint([]string{"--decode", "x.escpos"}); err == nil {
		t.Fatal("--decode without --out must fail")
	}
	if err := runPrint([]string{"--image", "sample/receipt.png", "--threshold", "999"}); err == nil {
		t.Fatal("an out-of-range threshold must fail")
	}
	if err := runPrint([]string{"--image", "sample/receipt.png"}); err == nil {
		t.Fatal("no --printer and no --out must fail")
	}
}

// Subcommand dispatch must not swallow the flag-first CLI.
func TestCommandDispatch(t *testing.T) {
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })

	os.Args = []string{"orderly-print-bridge", "version"}
	if err := run(); err != nil {
		t.Fatalf("version: %v", err)
	}

	os.Args = []string{"orderly-print-bridge", "nonsense"}
	err := run()
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("want an unknown-command error, got %v", err)
	}

	sink := filepath.Join(t.TempDir(), "sink.escpos")
	os.Args = []string{"orderly-print-bridge", "--image", "sample/receipt.png", "--width", "384", "--out", sink}
	if err := run(); err != nil {
		t.Fatalf("flag-first invocation broke: %v", err)
	}
	if _, err := os.Stat(sink); err != nil {
		t.Fatalf("flag-first invocation wrote nothing: %v", err)
	}
}

// The margin's own --decode round trip, at client #1's width: the repo's proof
// of the byte stream is the decoder, so the paper claim ("a gap on the left")
// is checked here without a printer. 512 + 24 = 536 columns, first 24 blank,
// nothing cropped (owner ruling 2026-09-08).
func TestCLILeftMarginShiftsWithoutWidening(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "margin.escpos")
	if err := runPrint([]string{
		"--image", "sample/receipt.png", "--width", "512",
		"--left-margin", "24", "--out", sink,
	}); err != nil {
		t.Fatalf("print with margin: %v", err)
	}
	raw, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	img, err := escpos.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != 536 {
		t.Fatalf("512 + a 24-dot margin must emit 536 columns, got %d", got)
	}
	// Every one of the first 24 columns must be white on every row.
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := 0; x < 24; x++ {
			if color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y < 128 {
				t.Fatalf("ink at (%d,%d): the first 24 columns must be blank", x, y)
			}
		}
	}
	if err := runPrint([]string{"--image", "sample/receipt.png", "--left-margin", "999", "--out", sink}); err == nil {
		t.Fatal("a margin past the cap must be refused, not silently clamped at the CLI")
	}
}

// The cut trailer is what the owner's T80C finding turned into a setting: a
// receipt that is not cut is a receipt the next order prints on top of.
func TestCLICutModes(t *testing.T) {
	dir := t.TempDir()
	trailer := func(args ...string) []byte {
		t.Helper()
		sink := filepath.Join(dir, "cut.escpos")
		if err := runPrint(append([]string{"--image", "sample/receipt.png", "--width", "384", "--out", sink}, args...)); err != nil {
			t.Fatalf("print %v: %v", args, err)
		}
		raw, err := os.ReadFile(sink)
		if err != nil {
			t.Fatal(err)
		}
		return raw[len(raw)-8:]
	}
	if got := trailer(); !bytes.HasSuffix(got, []byte{0x1d, 0x56, 0x00}) {
		t.Fatalf("default trailer % x, want a full cut GS V 0", got)
	}
	if got := trailer("--cut-mode", "partial"); !bytes.HasSuffix(got, []byte{0x1d, 0x56, 0x42, 0x00}) {
		t.Fatalf("partial trailer % x", got)
	}
	if got := trailer("--cut-mode", "none"); !bytes.HasSuffix(got, []byte{0x0a, 0x0a, 0x0a, 0x0a}) {
		t.Fatalf("none trailer % x, want the feed alone", got)
	}
	if got := trailer("--full-cut"); !bytes.HasSuffix(got, []byte{0x1d, 0x56, 0x00}) {
		t.Fatalf("--full-cut trailer % x", got)
	}
	if err := runPrint([]string{"--image", "sample/receipt.png", "--cut-mode", "sideways", "--out", filepath.Join(dir, "x")}); err == nil {
		t.Fatal("an unknown --cut-mode must be refused at the CLI")
	}
}
