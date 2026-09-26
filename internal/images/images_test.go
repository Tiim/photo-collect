package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	return img
}

func jpegBytes(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngBytes(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, testImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func chunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

func TestInspect(t *testing.T) {
	// Make an APNG by inserting acTL after IHDR (sig 8 + IHDR chunk 25 bytes).
	p := pngBytes(t, 4, 4)
	apng := append(append(append([]byte{}, p[:33]...), chunk("acTL", make([]byte, 8))...), p[33:]...)

	// Animated WebP: RIFF header + VP8X with animation flag.
	webpAnim := append([]byte("RIFF\x1e\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"), 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0)

	// HEIC ftyp header only (major brand heic) and a sequence one (msf1).
	ftyp := func(major string) []byte {
		b := make([]byte, 24)
		binary.BigEndian.PutUint32(b[0:4], 24)
		copy(b[4:], "ftyp")
		copy(b[8:], major)
		copy(b[16:], "mif1")
		return b
	}

	tests := []struct {
		name   string
		data   []byte
		want   error
		mime   string
		w, h   int
		anyErr bool
	}{
		{name: "jpeg", data: jpegBytes(t, 10, 7), mime: "image/jpeg", w: 10, h: 7},
		{name: "png", data: pngBytes(t, 5, 6), mime: "image/png", w: 5, h: 6},
		{name: "apng", data: apng, want: ErrAnimated},
		{name: "animated webp", data: webpAnim, want: ErrAnimated},
		{name: "heif sequence", data: ftyp("msf1"), want: ErrAnimated},
		{name: "text", data: []byte("hello world, definitely not an image"), want: ErrNotAnImage},
		{name: "empty", data: nil, want: ErrNotAnImage},
		{name: "gif", data: []byte("GIF89a\x01\x00\x01\x00"), want: ErrNotAnImage},
		{name: "truncated jpeg", data: jpegBytes(t, 10, 7)[:6], want: ErrCorrupt},
		{name: "heic garbage", data: ftyp("heic"), want: ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := Inspect(bytes.NewReader(tt.data))
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v, want %v", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.MIME != tt.mime || info.Width != tt.w || info.Height != tt.h {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestDerive(t *testing.T) {
	p := NewGoProcessor(50, 100, 1)
	for name, data := range map[string][]byte{"image/jpeg": jpegBytes(t, 400, 200), "image/png": pngBytes(t, 400, 200)} {
		d, err := p.Derive(context.Background(), bytes.NewReader(data), name)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(d.Thumbnail))
		if err != nil || cfg.Width != 50 || cfg.Height != 25 {
			t.Fatalf("%s thumbnail: %+v %v", name, cfg, err)
		}
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(d.Preview))
		if err != nil || cfg.Width != 100 || cfg.Height != 50 {
			t.Fatalf("%s preview: %+v %v", name, cfg, err)
		}
	}
}

func TestExifOrientation(t *testing.T) {
	// Big-endian TIFF with one IFD entry: Orientation = 6.
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0}
	if got := exifOrientation(tiff); got != 6 {
		t.Fatalf("got %d", got)
	}
	img := applyOrientation(testImage(4, 2), 6)
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 4 {
		t.Fatalf("rotated bounds %v", b)
	}
}
