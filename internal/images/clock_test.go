package images_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/images/imagetest"
)

func wall(s string) time.Time {
	t, err := time.ParseInLocation(images.WallTimeLayout, s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestClockPayloadRoundTrip(t *testing.T) {
	in := images.ClockReading{UTC: time.Unix(1_800_000_000, 0).UTC(), Wall: wall("2027-01-15T09:30:05")}
	got, ok := images.ParseClockPayload(images.FormatClockPayload(in.UTC, in.Wall))
	if !ok || !got.UTC.Equal(in.UTC) || !got.Wall.Equal(in.Wall) {
		t.Fatalf("round trip = %+v, %v", got, ok)
	}
	for _, bad := range []string{"", "https://example.com", "PC1|", "PC1|x|2027-01-15T09:30:05", "PC1|1800000000|nope", "PC1|1800000000"} {
		if _, ok := images.ParseClockPayload(bad); ok {
			t.Errorf("ParseClockPayload(%q) accepted", bad)
		}
	}
}

func TestReadMeta(t *testing.T) {
	taken := wall("2026-09-26T14:03:09")
	data := imagetest.JPEG(64, 48, imagetest.Camera{Make: "Apple", Model: "iPhone 14", Taken: taken})
	m := images.ReadMeta(bytes.NewReader(data))
	if m.Make != "Apple" || m.Model != "iPhone 14" || !m.TakenAt.Equal(taken) {
		t.Fatalf("meta = %+v", m)
	}
	if k := m.DeviceKey("  Tim  Bach "); k != "tim bach|Apple|iPhone 14" {
		t.Fatalf("device key = %q", k)
	}
	m.BodySerial = "123"
	if k := m.DeviceKey("Tim"); k != "tim|Apple|iPhone 14|123" {
		t.Fatalf("device key = %q", k)
	}
}

func TestReadMetaMissing(t *testing.T) {
	for name, data := range map[string][]byte{
		"no exif": imagetest.PlainJPEG(32, 32),
		"garbage": []byte("definitely not an image"),
		"empty":   nil,
	} {
		m := images.ReadMeta(bytes.NewReader(data))
		if m != (images.Meta{}) {
			t.Errorf("%s: meta = %+v, want empty", name, m)
		}
		if k := m.DeviceKey("tim"); k != "" {
			t.Errorf("%s: device key = %q, want empty", name, k)
		}
	}
	// Camera without a capture time still identifies the device.
	m := images.ReadMeta(bytes.NewReader(imagetest.JPEG(32, 32, imagetest.Camera{Make: "Google", Model: "Pixel 8"})))
	if m.DeviceKey("a") == "" || !m.TakenAt.IsZero() {
		t.Errorf("meta = %+v", m)
	}
}

func TestScanClock(t *testing.T) {
	p := images.NewGoProcessor(50, 100, 1)
	want := images.ClockReading{UTC: time.Unix(1_800_000_000, 0).UTC(), Wall: wall("2027-01-15T10:30:05")}

	for name, photo := range map[string][]byte{
		"small":                imagetest.ClockPhoto(want, imagetest.Camera{}),
		"12MP (is downscaled)": imagetest.ClockPhotoLarge(want, imagetest.Camera{}),
	} {
		got, err := p.ScanClock(context.Background(), bytes.NewReader(photo), "image/jpeg")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || !got.UTC.Equal(want.UTC) || !got.Wall.Equal(want.Wall) {
			t.Fatalf("%s: scan = %+v, want %+v", name, got, want)
		}
	}

	var err error
	var got *images.ClockReading

	got, err = p.ScanClock(context.Background(), bytes.NewReader(imagetest.PlainJPEG(200, 200)), "image/jpeg")
	if err != nil || got != nil {
		t.Fatalf("plain image: scan = %+v, %v", got, err)
	}
}
