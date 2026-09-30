package downloads

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestBuildXMPGPS(t *testing.T) {
	x := BuildXMP(XMPData{HasGPS: true, Lat: 47.3769, Lon: -8.5417})
	for _, want := range []string{`exif:GPSLatitude="47,22.614000N"`, `exif:GPSLongitude="8,32.502000W"`} {
		if !strings.Contains(string(x), want) {
			t.Errorf("XMP lacks %q:\n%s", want, x)
		}
	}
	if err := xml.Unmarshal(x[strings.Index(string(x), "<x:xmpmeta"):strings.Index(string(x), "<?xpacket end")], new(struct{})); err != nil {
		t.Errorf("XMP is not well-formed: %v", err)
	}
	if s := BuildXMP(XMPData{Rating: 1}); strings.Contains(string(s), "GPS") {
		t.Errorf("image without a position got GPS tags:\n%s", s)
	}
}

func TestBuildXMPCorrectedTime(t *testing.T) {
	with := BuildXMP(XMPData{Rating: 3, Captured: time.Date(2026, 9, 26, 12, 5, 0, 0, time.UTC)})
	for _, want := range []string{`xmp:CreateDate="2026-09-26T12:05:00"`, `exif:DateTimeOriginal="2026-09-26T12:05:00"`, `xmlns:exif=`, `xmp:Rating="3"`} {
		if !strings.Contains(string(with), want) {
			t.Errorf("XMP lacks %q:\n%s", want, with)
		}
	}
	if err := xml.Unmarshal(with[strings.Index(string(with), "<x:xmpmeta"):strings.Index(string(with), "<?xpacket end")], new(struct{})); err != nil {
		t.Errorf("XMP is not well-formed: %v", err)
	}

	without := BuildXMP(XMPData{Rating: 3})
	if strings.Contains(string(without), "DateTimeOriginal") || strings.Contains(string(without), "CreateDate") {
		t.Errorf("uncorrected image must not get a time:\n%s", without)
	}
}
