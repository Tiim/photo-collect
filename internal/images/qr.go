package images

import (
	"context"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// ClockPayloadPrefix starts every clock QR payload:
//
//	PC1|<utc unix seconds>|<local wall time YYYY-MM-DDTHH:MM:SS>
const ClockPayloadPrefix = "PC1|"

// ClockReading is what a photographed clock page said.
type ClockReading struct {
	UTC time.Time
	// Wall is the wall-clock time of the page's viewer as a naive time (UTC location).
	Wall time.Time
}

// FormatClockPayload builds the QR payload; the clock page's script builds the
// same string in the browser.
func FormatClockPayload(utc, wall time.Time) string {
	return fmt.Sprintf("%s%d|%s", ClockPayloadPrefix, utc.Unix(), wall.Format(WallTimeLayout))
}

// ParseClockPayload parses a payload produced by FormatClockPayload.
func ParseClockPayload(s string) (ClockReading, bool) {
	rest, ok := strings.CutPrefix(s, ClockPayloadPrefix)
	if !ok {
		return ClockReading{}, false
	}
	unix, wall, ok := strings.Cut(rest, "|")
	if !ok {
		return ClockReading{}, false
	}
	sec, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return ClockReading{}, false
	}
	w, err := time.ParseInLocation(WallTimeLayout, wall, time.UTC)
	if err != nil {
		return ClockReading{}, false
	}
	return ClockReading{UTC: time.Unix(sec, 0).UTC(), Wall: w}, true
}

// qrScanSize bounds the longest side of the image handed to the QR reader.
// Calibration shots are mostly the clock page, so the code stays large.
const qrScanSize = 1600

// ScanClock decodes the original and looks for a clock QR code. It returns
// (nil, nil) if the image has no (valid) clock code.
func (p *GoProcessor) ScanClock(ctx context.Context, original io.Reader, mime string) (*ClockReading, error) {
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
	img, err := f.Decode(original)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return scanClockImage(scaleToFit(img, qrScanSize)), nil
}

func scanClockImage(img image.Image) *ClockReading {
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return nil
	}
	reader := qrcode.NewQRCodeReader()
	for _, hints := range []map[gozxing.DecodeHintType]any{nil, {gozxing.DecodeHintType_TRY_HARDER: true}} {
		res, err := reader.Decode(bmp, hints)
		if err != nil {
			continue
		}
		if r, ok := ParseClockPayload(res.GetText()); ok {
			return &r
		}
		return nil // some other QR code
	}
	return nil
}
