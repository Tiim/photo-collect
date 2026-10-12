package images_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/images/imagetest"
)

func TestInspectVideo(t *testing.T) {
	iphone := imagetest.Video("qt  ", []string{"qt  "}, 100)
	android := imagetest.Video("mp42", []string{"isom", "mp42"}, 100)
	noMoov := iphone[:len(iphone)-100-8-16] // ftyp only
	truncated := iphone[:len(iphone)-10]    // mdat claims more than is there

	// A 64-bit mdat size (size field 1) as cameras write for files over 4 GB.
	large := imagetest.Video("isom", []string{"isom"}, 0)
	large = large[:len(large)-8]
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[0:4], 1)
	copy(hdr[4:8], "mdat")
	binary.BigEndian.PutUint64(hdr[8:16], 16+5)
	large = append(append(large, hdr[:]...), "12345"...)

	tests := []struct {
		name string
		data []byte
		mime string
		want error
	}{
		{name: "iphone mov", data: iphone, mime: "video/quicktime"},
		{name: "android mp4", data: android, mime: "video/mp4"},
		{name: "isom mp4", data: imagetest.Video("isom", []string{"isom", "iso2", "avc1", "mp41"}, 10), mime: "video/mp4"},
		{name: "64-bit box size", data: large, mime: "video/mp4"},
		{name: "no moov", data: noMoov, want: images.ErrCorrupt},
		{name: "truncated", data: truncated, want: images.ErrCorrupt},
		// HEIF files are images, whatever else their brands say.
		{name: "heic is not a video", data: imagetest.Video("heic", []string{"mif1", "heic"}, 10), want: images.ErrCorrupt},
		{name: "mp4 brand with heif compat", data: imagetest.Video("isom", []string{"mif1"}, 10), want: images.ErrCorrupt},
		{name: "heif sequence", data: imagetest.Video("msf1", []string{"iso8", "msf1"}, 10), want: images.ErrAnimated},
		{name: "heif sequence as compat", data: imagetest.Video("isom", []string{"isom", "msf1"}, 10), want: images.ErrAnimated},
		{name: "hevc sequence", data: imagetest.Video("mp42", []string{"hevs"}, 10), want: images.ErrAnimated},
		{name: "unknown brand", data: imagetest.Video("abcd", []string{"abcd"}, 10), want: images.ErrNotAnImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := images.Inspect(bytes.NewReader(tt.data), 1) // pixel limit does not apply
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v, want %v", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.MIME != tt.mime || info.Width != 0 || info.Height != 0 || !images.IsVideo(info.MIME) {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestVideoCannotBeDecoded(t *testing.T) {
	for _, mime := range []string{"video/mp4", "video/quicktime"} {
		if _, err := images.FormatForMIME(mime); !errors.Is(err, images.ErrUnsupported) {
			t.Errorf("%s: err = %v", mime, err)
		}
	}
	if images.IsVideo("image/heic") {
		t.Error("image/heic is not a video")
	}
}
