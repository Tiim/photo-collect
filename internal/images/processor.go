package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"io"

	"golang.org/x/image/draw"
)

// Derived holds generated derivative images, both JPEG encoded.
type Derived struct {
	Thumbnail []byte
	Preview   []byte
}

// Processor generates derivative images from originals. It exists so the rest
// of the application does not depend on a particular image library.
type Processor interface {
	Derive(ctx context.Context, original io.Reader, mime string) (*Derived, error)
	// ScanClock looks for a clock-calibration QR code; nil if there is none.
	ScanClock(ctx context.Context, original io.Reader, mime string) (*ClockReading, error)
}

// GoProcessor is a pure-Go Processor. It bounds the number of concurrent
// decodes because a decoded photo can take hundreds of MB of RAM.
type GoProcessor struct {
	thumbSize, previewSize int
	sem                    chan struct{}
}

func NewGoProcessor(thumbSize, previewSize, maxConcurrent int) *GoProcessor {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &GoProcessor{thumbSize: thumbSize, previewSize: previewSize, sem: make(chan struct{}, maxConcurrent)}
}

func (p *GoProcessor) Derive(ctx context.Context, original io.Reader, mime string) (*Derived, error) {
	f, err := FormatForMIME(mime)
	if err != nil {
		return nil, err
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	data, err := io.ReadAll(original)
	if err != nil {
		return nil, err
	}
	orientation := 1
	if f.MIME == "image/jpeg" {
		orientation = jpegOrientation(data)
	}
	img, err := f.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	data = nil
	img = applyOrientation(img, orientation)

	var d Derived
	if d.Preview, err = encodeScaled(img, p.previewSize, 85); err != nil {
		return nil, err
	}
	if d.Thumbnail, err = encodeScaled(img, p.thumbSize, 80); err != nil {
		return nil, err
	}
	return &d, nil
}

// scaleToFit resamples img to fit within max×max (never upscaling), flattened
// onto white. It always returns a fresh RGBA image.
func scaleToFit(img image.Image, max int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > max || h > max {
		if w >= h {
			h, w = h*max/w, max
		} else {
			w, h = w*max/h, max
		}
		if w < 1 {
			w = 1
		}
		if h < 1 {
			h = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// Flatten transparency onto white so JPEG output looks right.
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// encodeScaled fits img within max×max (never upscaling) and encodes it as JPEG.
func encodeScaled(img image.Image, max, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaleToFit(img, max), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// jpegOrientation extracts the EXIF orientation (1-8) from a JPEG, or 1.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xDA || marker == 0xD9 { // start of scan / end of image
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return 1
		}
		if marker == 0xE1 && segLen >= 8 && string(data[i+4:i+10]) == "Exif\x00\x00" {
			return exifOrientation(data[i+10 : i+2+segLen])
		}
		i += 2 + segLen
	}
	return 1
}

func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	n := int(bo.Uint16(tiff[ifd : ifd+2]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + k*12
		if e+12 > len(tiff) {
			return 1
		}
		if bo.Uint16(tiff[e:e+2]) == 0x0112 { // Orientation
			v := int(bo.Uint16(tiff[e+8 : e+10]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// applyOrientation rotates/flips img according to EXIF orientation values 1-8.
func applyOrientation(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	var dst *image.RGBA
	if o >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
