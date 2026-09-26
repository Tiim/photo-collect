package http

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/tiim/photo-collect/internal/images"
)

const captureLayout = "2006-01-02 15:04:05"

// Capture-time helpers for templates. The corrected time is the camera's EXIF
// time plus the offset learnt from a photo of the clock page.

func (t TileView) HasCaptureTime() bool { return t.ExifTime.Valid }

func (t TileView) Corrected() bool { return t.TimeOffsetSeconds.Valid }

// CapturedAt is the corrected capture time (the camera's own if uncorrected).
func (t TileView) CapturedAt() string {
	c, ok := images.CorrectedTime(t.ExifTime, t.TimeOffsetSeconds)
	if !ok {
		return ""
	}
	return c.Format(captureLayout)
}

// CameraTime is what the camera's own clock said.
func (t TileView) CameraTime() string {
	c, ok := images.CorrectedTime(t.ExifTime, sql.NullInt64{})
	if !ok {
		return ""
	}
	return c.Format(captureLayout)
}

// OffsetText renders the applied correction, e.g. "+2h 05m 00s".
func (t TileView) OffsetText() string {
	if !t.TimeOffsetSeconds.Valid {
		return ""
	}
	return formatOffset(t.TimeOffsetSeconds.Int64)
}

// DeviceRow summarises one camera of one uploader within a folder.
type DeviceRow struct {
	Uploader, Camera string
	Images           int64
	Calibrated       bool
	Status           string
}

func (s *Server) devicesView(ctx context.Context, folderID string) ([]DeviceRow, error) {
	rows, err := s.db.Q.ListFolderDevices(ctx, folderID)
	if err != nil {
		return nil, err
	}
	out := make([]DeviceRow, 0, len(rows))
	for _, r := range rows {
		// Device keys are "nickname|make|model[|serial]".
		parts := strings.Split(r.DeviceKey.String, "|")
		camera := r.DeviceKey.String
		if len(parts) >= 3 {
			camera = strings.TrimSpace(parts[1] + " " + parts[2])
		}
		d := DeviceRow{Uploader: r.UploaderNickname, Camera: camera, Images: r.ImageCount, Calibrated: r.CorrectedCount > 0}
		switch {
		case r.CalibrationCount == 0:
			d.Status = "no clock photo yet"
		case r.CorrectedCount == 0:
			d.Status = "clock photo found, but the photos carry no capture time"
		case r.MinOffset == r.MaxOffset:
			d.Status = "corrected by " + formatOffset(r.MinOffset)
		default:
			d.Status = "corrected by " + formatOffset(r.MinOffset) + " to " + formatOffset(r.MaxOffset)
		}
		out = append(out, d)
	}
	return out, nil
}

func formatOffset(sec int64) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	h, m, s := sec/3600, sec%3600/60, sec%60
	switch {
	case h > 0:
		return fmt.Sprintf("%s%dh %02dm %02ds", sign, h, m, s)
	case m > 0:
		return fmt.Sprintf("%s%dm %02ds", sign, m, s)
	}
	return fmt.Sprintf("%s%ds", sign, s)
}
