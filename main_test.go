package main

import (
	"image"
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
