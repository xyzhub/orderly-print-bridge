package escpos

import (
	"fmt"
	"image"
	"image/color"
)

// Decode reconstructs an image from an ESC/POS stream by scanning for every
// GS v 0 raster command and stacking their bands vertically. It is the inverse
// of Encode's raster step and exists so the raster can be verified visually
// without a physical printer: decode the bytes back to a PNG and confirm the
// Arabic + QR survived the 1-bit conversion.
//
// It understands exactly the subset Encode emits (GS v 0, m=0) plus it skips
// the init/align/feed/cut control bytes. Unknown bytes between commands are
// ignored, which is enough for our own output and for typical raster streams.
func Decode(stream []byte) (image.Image, error) {
	type band struct {
		width, height, bytesPerRow int
		data                       []byte
	}
	var bands []band
	totalH := 0
	maxW := 0

	i := 0
	for i < len(stream) {
		// Look for GS v 0 : 0x1D 0x76 0x30
		if i+8 <= len(stream) && stream[i] == 0x1d && stream[i+1] == 0x76 && stream[i+2] == 0x30 {
			// m := stream[i+3] // 0 = normal; other modes scale, unused here.
			bytesPerRow := int(stream[i+4]) | int(stream[i+5])<<8
			height := int(stream[i+6]) | int(stream[i+7])<<8
			width := bytesPerRow * 8
			dataStart := i + 8
			dataLen := bytesPerRow * height
			if dataStart+dataLen > len(stream) {
				return nil, fmt.Errorf("GS v 0 at offset %d claims %d bytes but stream has %d remaining", i, dataLen, len(stream)-dataStart)
			}
			bands = append(bands, band{
				width:       width,
				height:      height,
				bytesPerRow: bytesPerRow,
				data:        stream[dataStart : dataStart+dataLen],
			})
			totalH += height
			if width > maxW {
				maxW = width
			}
			i = dataStart + dataLen
			continue
		}
		i++
	}

	if len(bands) == 0 {
		return nil, fmt.Errorf("no GS v 0 raster commands found in stream")
	}

	img := image.NewGray(image.Rect(0, 0, maxW, totalH))
	// Start white.
	for p := range img.Pix {
		img.Pix[p] = 0xff
	}
	yOff := 0
	for _, bd := range bands {
		for y := 0; y < bd.height; y++ {
			for bx := 0; bx < bd.bytesPerRow; bx++ {
				b := bd.data[y*bd.bytesPerRow+bx]
				for bit := 0; bit < 8; bit++ {
					if b&(1<<(7-bit)) != 0 { // set bit = black
						x := bx*8 + bit
						img.SetGray(x, yOff+y, color.Gray{Y: 0})
					}
				}
			}
		}
		yOff += bd.height
	}
	return img, nil
}
