// Package images validates uploaded image files and generates derivatives.
package images

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/gen2brain/heic"
	"golang.org/x/image/webp"
)

// Info describes a validated image.
type Info struct {
	MIME   string
	Ext    string
	Width  int
	Height int
}

// Validation errors; messages are safe to show to uploaders.
var (
	ErrNotAnImage    = errors.New("file is not a supported image")
	ErrAnimated      = errors.New("animated images are not supported")
	ErrCorrupt       = errors.New("image file is damaged or unreadable")
	ErrUnsupported   = errors.New("unsupported image format")
	ErrTooManyPixels = errors.New("image resolution is too large")
)

// Format is a supported image format. Adding a format means adding an entry
// to formats below.
type Format struct {
	MIME string
	Ext  string
	// Match reports whether head (the first bytes of the file) belongs to this format.
	Match func(head []byte) bool
	// Inspect validates the whole file and returns its dimensions. It must
	// return ErrAnimated for animated content.
	Inspect func(r io.ReadSeeker) (w, h int, err error)
	// Decode fully decodes the image (used for derivative generation).
	Decode func(r io.Reader) (image.Image, error)
}

var formats = []Format{jpegFormat, pngFormat, webpFormat, heicFormat}

const headSize = 64

// Inspect determines the format from the file content (never from its name),
// validates it and returns its metadata. Images with more than maxPixels
// pixels are rejected with ErrTooManyPixels based on the header alone, before
// any full decode. r is left at an unspecified position.
func Inspect(r io.ReadSeeker, maxPixels int64) (Info, error) {
	f, err := detect(r)
	if err != nil {
		return Info{}, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Info{}, err
	}
	w, h, err := f.Inspect(r)
	if err != nil {
		if errors.Is(err, ErrAnimated) {
			return Info{}, err
		}
		return Info{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if w > 0 && h > 0 && int64(w)*int64(h) > maxPixels {
		return Info{}, fmt.Errorf("%w: %dx%d", ErrTooManyPixels, w, h)
	}
	if w <= 0 || h <= 0 || w > 20000 || h > 20000 {
		return Info{}, fmt.Errorf("%w: unreasonable dimensions %dx%d", ErrCorrupt, w, h)
	}
	return Info{MIME: f.MIME, Ext: f.Ext, Width: w, Height: h}, nil
}

func detect(r io.ReadSeeker) (*Format, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	head := make([]byte, headSize)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]
	for i := range formats {
		if formats[i].Match(head) {
			return &formats[i], nil
		}
	}
	return nil, ErrNotAnImage
}

// FormatForMIME returns the format registered for mime.
func FormatForMIME(mime string) (*Format, error) {
	for i := range formats {
		if formats[i].MIME == mime {
			return &formats[i], nil
		}
	}
	return nil, ErrUnsupported
}

// ---- JPEG ----

var jpegFormat = Format{
	MIME:  "image/jpeg",
	Ext:   ".jpg",
	Match: func(h []byte) bool { return bytes.HasPrefix(h, []byte{0xFF, 0xD8, 0xFF}) },
	Inspect: func(r io.ReadSeeker) (int, int, error) {
		cfg, err := jpeg.DecodeConfig(r)
		return cfg.Width, cfg.Height, err
	},
	Decode: func(r io.Reader) (image.Image, error) { return jpeg.Decode(r) },
}

// ---- PNG ----

var pngSig = []byte("\x89PNG\r\n\x1a\n")

var pngFormat = Format{
	MIME:  "image/png",
	Ext:   ".png",
	Match: func(h []byte) bool { return bytes.HasPrefix(h, pngSig) },
	Inspect: func(r io.ReadSeeker) (int, int, error) {
		cfg, err := png.DecodeConfig(r)
		if err != nil {
			return 0, 0, err
		}
		animated, err := pngIsAnimated(r)
		if err != nil {
			return 0, 0, err
		}
		if animated {
			return 0, 0, ErrAnimated
		}
		return cfg.Width, cfg.Height, nil
	},
	Decode: func(r io.Reader) (image.Image, error) { return png.Decode(r) },
}

// pngIsAnimated walks PNG chunks looking for an acTL chunk (APNG), which must
// precede the first IDAT.
func pngIsAnimated(r io.ReadSeeker) (bool, error) {
	if _, err := r.Seek(int64(len(pngSig)), io.SeekStart); err != nil {
		return false, err
	}
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return false, err
		}
		length := int64(binary.BigEndian.Uint32(hdr[0:4]))
		switch string(hdr[4:8]) {
		case "acTL":
			return true, nil
		case "IDAT", "IEND":
			return false, nil
		}
		if _, err := r.Seek(length+4, io.SeekCurrent); err != nil { // data + CRC
			return false, err
		}
	}
}

// ---- WebP ----

var webpFormat = Format{
	MIME: "image/webp",
	Ext:  ".webp",
	Match: func(h []byte) bool {
		return len(h) >= 12 && bytes.Equal(h[0:4], []byte("RIFF")) && bytes.Equal(h[8:12], []byte("WEBP"))
	},
	Inspect: func(r io.ReadSeeker) (int, int, error) {
		var hdr [21]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return 0, 0, err
		}
		// Extended format: "VP8X" chunk at offset 12, flags byte at 20; bit 1 (0x02) = animation.
		if string(hdr[12:16]) == "VP8X" && hdr[20]&0x02 != 0 {
			return 0, 0, ErrAnimated
		}
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, 0, err
		}
		cfg, err := webp.DecodeConfig(r)
		return cfg.Width, cfg.Height, err
	},
	Decode: func(r io.Reader) (image.Image, error) { return webp.Decode(r) },
}

// ---- HEIC / HEIF ----

var heicBrands = map[string]bool{
	"heic": true, "heix": true, "heim": true, "heis": true, "hevc": true, "hevx": true, "mif1": true,
}

// heifSequenceBrands mark image sequences / animations, which we reject.
var heifSequenceBrands = map[string]bool{"msf1": true, "hevm": true, "hevs": true}

func ftypBrands(h []byte) (major string, compat []string, ok bool) {
	if len(h) < 16 || string(h[4:8]) != "ftyp" {
		return "", nil, false
	}
	size := int(binary.BigEndian.Uint32(h[0:4]))
	if size < 16 || size > len(h) {
		size = len(h)
	}
	major = string(h[8:12])
	for off := 16; off+4 <= size; off += 4 {
		compat = append(compat, string(h[off:off+4]))
	}
	return major, compat, true
}

var heicFormat = Format{
	MIME: "image/heic",
	Ext:  ".heic",
	Match: func(h []byte) bool {
		major, compat, ok := ftypBrands(h)
		if !ok {
			return false
		}
		if heicBrands[major] || heifSequenceBrands[major] {
			return true
		}
		for _, c := range compat {
			if heicBrands[c] {
				return true
			}
		}
		return false
	},
	Inspect: func(r io.ReadSeeker) (int, int, error) {
		head := make([]byte, headSize)
		n, _ := io.ReadFull(r, head)
		major, compat, _ := ftypBrands(head[:n])
		if heifSequenceBrands[major] {
			return 0, 0, ErrAnimated
		}
		for _, c := range compat {
			if heifSequenceBrands[c] {
				return 0, 0, ErrAnimated
			}
		}
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, 0, err
		}
		cfg, err := heic.DecodeConfig(r)
		return cfg.Width, cfg.Height, err
	},
	Decode: func(r io.Reader) (image.Image, error) { return heic.Decode(r) },
}
