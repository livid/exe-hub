package avatar

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestProcessCropsResizesAndKeepsAlpha(t *testing.T) {
	// 40×80 source: fully transparent except an opaque red band across the
	// vertical middle — inside the centered 40×40 crop.
	src := image.NewRGBA(image.Rect(0, 0, 40, 80))
	for y := 36; y < 44; y++ {
		for x := 0; x < 40; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var in bytes.Buffer
	if err := png.Encode(&in, src); err != nil {
		t.Fatal(err)
	}

	out, err := Process(in.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not PNG: %v", err)
	}
	if img.Bounds().Dx() != Size || img.Bounds().Dy() != Size {
		t.Fatalf("output %v, want %d×%d", img.Bounds(), Size, Size)
	}
	// corners were transparent in the source and must stay transparent
	_, _, _, a := img.At(2, 2).RGBA()
	if a != 0 {
		t.Fatalf("corner alpha %d, want 0 — transparency lost", a)
	}
	// the middle band was opaque red and must stay opaque
	r, _, _, a2 := img.At(Size/2, Size/2).RGBA()
	if a2 == 0 || r == 0 {
		t.Fatalf("center pixel r=%d a=%d, want opaque red", r, a2)
	}
}

func TestProcessRejectsGarbage(t *testing.T) {
	if _, err := Process([]byte("not an image at all")); err == nil {
		t.Fatal("garbage accepted")
	}
}
