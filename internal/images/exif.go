package images

import (
	"database/sql"
	"io"
	"strings"
	"time"

	"github.com/evanoberholster/imagemeta"
)

// WallTimeLayout is the format of naive wall-clock times stored in the database.
const WallTimeLayout = "2006-01-02T15:04:05"

// Meta is the camera metadata used for clock calibration.
type Meta struct {
	Make, Model, BodySerial string
	// TakenAt is the EXIF capture time as a naive wall-clock time (its fields
	// are the clock reading, the location is always UTC). Zero if unknown.
	TakenAt time.Time
}

// ReadMeta extracts camera metadata. Missing, unsupported or damaged EXIF is
// not an error: the result is simply empty and the image cannot be corrected.
func ReadMeta(r io.ReadSeeker) Meta {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Meta{}
	}
	ex, err := imagemeta.Decode(r)
	if err != nil {
		return Meta{}
	}
	m := Meta{
		Make:       strings.TrimSpace(ex.IFD0.Make),
		Model:      strings.TrimSpace(ex.IFD0.Model),
		BodySerial: strings.TrimSpace(ex.ExifIFD.BodySerialNumber),
	}
	t := ex.ExifIFD.DateTimeOriginal
	if t.IsZero() {
		t = ex.ExifIFD.CreateDate
	}
	if !t.IsZero() && t.Year() > 1970 {
		// Keep the clock reading, ignore whatever zone the library attached.
		m.TakenAt = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	}
	return m
}

// CorrectedTime applies a calibration offset (seconds, may be NULL) to a stored
// naive EXIF time. ok is false if the image has no capture time.
func CorrectedTime(exifTime sql.NullString, offsetSeconds sql.NullInt64) (t time.Time, ok bool) {
	if !exifTime.Valid {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(WallTimeLayout, exifTime.String, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	if offsetSeconds.Valid {
		t = t.Add(time.Duration(offsetSeconds.Int64) * time.Second)
	}
	return t, true
}

// DeviceKey identifies the device an image came from: the uploader plus the
// camera make, model and body serial. Empty if the camera is unknown, in which
// case the image cannot be matched to a calibration shot.
func (m Meta) DeviceKey(nickname string) string {
	if m.Make == "" && m.Model == "" {
		return ""
	}
	parts := []string{strings.ToLower(strings.Join(strings.Fields(nickname), " ")), m.Make, m.Model}
	if m.BodySerial != "" {
		parts = append(parts, m.BodySerial)
	}
	return strings.Join(parts, "|")
}
