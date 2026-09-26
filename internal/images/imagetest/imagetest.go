// Package imagetest builds image fixtures for tests: JPEGs with EXIF camera
// metadata and photos of a clock-calibration QR code.
package imagetest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	"github.com/tiim/photo-collect/internal/images"
)

// Camera describes the EXIF metadata to embed. Empty fields are omitted.
type Camera struct {
	Make, Model string
	// Taken is the naive wall-clock capture time (DateTimeOriginal); zero omits it.
	Taken time.Time
}

// PlainJPEG returns a small gradient JPEG without EXIF.
func PlainJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	return encode(img, Camera{})
}

// JPEG returns a gradient JPEG carrying cam as EXIF.
func JPEG(w, h int, cam Camera) []byte {
	return WithEXIF(PlainJPEG(w, h), cam)
}

// ClockPhoto returns a JPEG photo of a clock page showing the given reading,
// taken by cam.
func ClockPhoto(reading images.ClockReading, cam Camera) []byte {
	return clockPhoto(reading, cam, 1200, 900, 700)
}

// ClockPhotoLarge is like ClockPhoto but a 12 MP frame, bigger than the QR
// scanner's working size, so that downscaling is exercised. Slow under -race.
func ClockPhotoLarge(reading images.ClockReading, cam Camera) []byte {
	return clockPhoto(reading, cam, 4000, 3000, 2000)
}

// clockPhoto draws a code of code×code pixels in the middle of a w×h frame,
// like a photo of a phone or monitor filling most of the view.
func clockPhoto(reading images.ClockReading, cam Camera, w, h, code int) []byte {
	payload := images.FormatClockPayload(reading.UTC, reading.Wall)
	m, err := qrcode.NewQRCodeWriter().Encode(payload, gozxing.BarcodeFormat_QR_CODE, code, code, nil)
	if err != nil {
		panic(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for y := 0; y < m.GetHeight(); y++ {
		for x := 0; x < m.GetWidth(); x++ {
			if m.Get(x, y) {
				img.Set((w-code)/2+x, (h-code)/2+y, color.Black)
			}
		}
	}
	return encode(img, cam)
}

func encode(img image.Image, cam Camera) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return WithEXIF(b.Bytes(), cam)
}

// WithEXIF inserts an EXIF APP1 segment right after the JPEG SOI marker.
func WithEXIF(jpg []byte, cam Camera) []byte {
	if cam == (Camera{}) {
		return jpg
	}
	tiff := buildTIFF(cam)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(2+6+len(tiff)))
	seg = append(seg, "Exif\x00\x00"...)
	seg = append(seg, tiff...)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

type entry struct {
	tag   uint16
	typ   uint16 // 2 = ASCII, 4 = LONG
	count uint32
	data  []byte // ASCII payload (NUL-terminated) or nil for LONG
	long  uint32
}

func ascii(tag uint16, s string) entry {
	d := append([]byte(s), 0)
	return entry{tag: tag, typ: 2, count: uint32(len(d)), data: d}
}

// buildTIFF lays out a little-endian TIFF: IFD0 (Make, Model, ExifIFD pointer)
// followed by the ExifIFD (DateTimeOriginal).
func buildTIFF(cam Camera) []byte {
	var ifd0 []entry
	if cam.Make != "" {
		ifd0 = append(ifd0, ascii(0x010F, cam.Make))
	}
	if cam.Model != "" {
		ifd0 = append(ifd0, ascii(0x0110, cam.Model))
	}
	var exifIFD []entry
	if !cam.Taken.IsZero() {
		exifIFD = append(exifIFD, ascii(0x9003, cam.Taken.Format("2006:01:02 15:04:05")))
		ifd0 = append(ifd0, entry{tag: 0x8769, typ: 4, count: 1}) // long filled in below
	}

	size := func(es []entry) int { return 2 + 12*len(es) + 4 }
	pos := 8
	ifd0Off := pos
	pos += size(ifd0)
	exifOff := 0
	if len(exifIFD) > 0 {
		exifOff = pos
		pos += size(exifIFD)
	}
	for i := range ifd0 {
		if ifd0[i].tag == 0x8769 {
			ifd0[i].long = uint32(exifOff)
		}
	}

	var data bytes.Buffer
	write := func(es []entry) []byte {
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, uint16(len(es)))
		for _, e := range es {
			binary.Write(&b, binary.LittleEndian, e.tag)
			binary.Write(&b, binary.LittleEndian, e.typ)
			binary.Write(&b, binary.LittleEndian, e.count)
			switch {
			case e.typ == 4:
				binary.Write(&b, binary.LittleEndian, e.long)
			case len(e.data) <= 4:
				v := make([]byte, 4)
				copy(v, e.data)
				b.Write(v)
			default:
				binary.Write(&b, binary.LittleEndian, uint32(pos+data.Len()))
				data.Write(e.data)
			}
		}
		binary.Write(&b, binary.LittleEndian, uint32(0)) // no next IFD
		return b.Bytes()
	}
	ifd0Bytes := write(ifd0)
	var exifBytes []byte
	if len(exifIFD) > 0 {
		exifBytes = write(exifIFD)
	}

	var out bytes.Buffer
	out.WriteString("II*\x00")
	binary.Write(&out, binary.LittleEndian, uint32(ifd0Off))
	out.Write(ifd0Bytes)
	out.Write(exifBytes)
	out.Write(data.Bytes())
	return out.Bytes()
}
