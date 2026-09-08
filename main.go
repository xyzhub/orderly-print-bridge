// orderly-print-bridge — print an Orderly receipt image to an ESC/POS thermal
// printer, silently, with no CUPS/Avahi/browser dependency.
//
// Two shapes in one binary:
//
//   - the one-shot CLI (--image / --printer / --width / --decode) that the
//     paper tests drive by hand, unchanged;
//   - `serve`, the daemon: enroll by setup code + hardware serial, then poll
//     Orderly for print jobs, raster them and acknowledge (internal/bridge).
package main

import (
	"flag"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for --image inputs
	"image/png"
	"os"
	"strings"

	"github.com/xyz/orderly-print-bridge/internal/escpos"
	"github.com/xyz/orderly-print-bridge/internal/transport"
	"github.com/xyz/orderly-print-bridge/internal/version"
)

const usage = `orderly-print-bridge — image -> ESC/POS raster -> thermal printer

USAGE
  orderly-print-bridge --image receipt.png --printer tcp://192.168.1.50:9100
  orderly-print-bridge --image receipt.png --out receipt.escpos      (dry run)
  orderly-print-bridge --decode receipt.escpos --out roundtrip.png   (verify)

COMMANDS
  serve    run the daemon: enroll if needed, then poll Orderly for print jobs
  enroll   claim a setup code and store the device token, then exit
  version  print the agent version
  (no command = the one-shot print/decode CLI below)

FLAGS
  --image   <path>    receipt PNG/JPEG to print
  --printer <target>  tcp://HOST:9100 | usb:///dev/usb/lp0 | file:///path
  --out     <path>    dry run: write ESC/POS bytes here instead of printing
                      (also the PNG destination in --decode mode)
  --width   <dots>    raster width; 80mm@203dpi = 576 (default), 58mm = 384
  --dither            Floyd-Steinberg dither instead of a hard threshold
  --threshold <0-255> grey cutoff for black when not dithering (default 128)
  --center            center the image on the paper
  --full-cut          full cut (GS V 0) instead of partial cut (GS V 66 0)
  --decode  <path>    decode an ESC/POS file back to PNG (writes to --out)
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "serve":
			return runServe(args[1:])
		case "enroll":
			return runEnroll(args[1:])
		case "version":
			fmt.Println(version.UserAgent())
			return nil
		case "print":
			return runPrint(args[1:])
		case "help":
			fmt.Print(usage)
			return nil
		default:
			return fmt.Errorf("unknown command %q (try: serve, enroll, version, or the --image flags)", args[0])
		}
	}
	return runPrint(args)
}

// runPrint is the original one-shot CLI, unchanged in behaviour: it is what
// the paper tests drive (--image --printer --width).
func runPrint(args []string) error {
	fs := flag.NewFlagSet("orderly-print-bridge", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	var (
		imagePath = fs.String("image", "", "receipt image (PNG/JPEG) to print")
		printer   = fs.String("printer", "", "printer target (tcp://, usb://, file://)")
		out       = fs.String("out", "", "dry-run output file for ESC/POS bytes (PNG in --decode mode)")
		width     = fs.Int("width", 576, "raster width in dots")
		dither    = fs.Bool("dither", false, "Floyd-Steinberg dither instead of threshold")
		threshold = fs.Int("threshold", 128, "grey cutoff (0-255) for black when not dithering")
		center    = fs.Bool("center", false, "center the image on the paper")
		fullCut   = fs.Bool("full-cut", false, "full cut instead of partial cut")
		decode    = fs.String("decode", "", "decode an ESC/POS file back to PNG (writes to --out)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Verification mode: ESC/POS file -> PNG.
	if *decode != "" {
		if *out == "" {
			return fmt.Errorf("--decode requires --out <file.png>")
		}
		return decodeToPNG(*decode, *out)
	}

	if *imagePath == "" {
		fs.Usage()
		return fmt.Errorf("--image is required (or use --decode)")
	}
	if *threshold < 0 || *threshold > 255 {
		return fmt.Errorf("--threshold must be 0-255, got %d", *threshold)
	}

	img, err := loadImage(*imagePath)
	if err != nil {
		return err
	}

	opts := escpos.Options{
		Width:      *width,
		Dither:     *dither,
		Threshold:  uint8(*threshold),
		Center:     *center,
		FullCut:    *fullCut,
		BandHeight: 128,
	}
	stream, err := escpos.Encode(img, opts)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	// Dry run: write bytes to a file.
	if *out != "" {
		if err := os.WriteFile(*out, stream, 0o644); err != nil {
			return fmt.Errorf("write --out %s: %w", *out, err)
		}
		fmt.Printf("wrote %d bytes of ESC/POS to %s (dry run, not printed)\n", len(stream), *out)
		return nil
	}

	if *printer == "" {
		return fmt.Errorf("need --printer <target> to print, or --out <file> for a dry run")
	}
	n, err := transport.Send(*printer, stream)
	if err != nil {
		return fmt.Errorf("send to %s (%d of %d bytes sent): %w", *printer, n, len(stream), err)
	}
	fmt.Printf("sent %d bytes to %s\n", n, *printer)
	return nil
}

func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open image %s: %w", path, err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode image %s: %w", path, err)
	}
	return img, nil
}

func decodeToPNG(escposPath, pngPath string) error {
	stream, err := os.ReadFile(escposPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", escposPath, err)
	}
	img, err := escpos.Decode(stream)
	if err != nil {
		return fmt.Errorf("decode ESC/POS: %w", err)
	}
	f, err := os.Create(pngPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", pngPath, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("encode PNG: %w", err)
	}
	b := img.Bounds()
	fmt.Printf("decoded %s -> %s (%dx%d)\n", escposPath, pngPath, b.Dx(), b.Dy())
	return nil
}
