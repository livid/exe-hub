// Package avatar normalizes profile images: whatever comes in (PNG, JPEG,
// GIF), the largest centered square is cropped out, scaled to 128×128, and
// re-encoded as PNG. The pipeline stays in RGBA end to end with draw.Src,
// so source transparency survives into the output.
package avatar

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
)

const Size = 128

// maxDim guards against decompression bombs before the full decode.
const maxDim = 8192

func Process(b []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("not a decodable image: %w", err)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxDim || cfg.Height > maxDim {
		return nil, errors.New("image dimensions out of range")
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	bnds := img.Bounds()
	side := min(bnds.Dx(), bnds.Dy())
	x0 := bnds.Min.X + (bnds.Dx()-side)/2
	y0 := bnds.Min.Y + (bnds.Dy()-side)/2

	dst := image.NewRGBA(image.Rect(0, 0, Size, Size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, image.Rect(x0, y0, x0+side, y0+side), draw.Src, nil)

	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
