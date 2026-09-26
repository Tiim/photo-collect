package downloads

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

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
