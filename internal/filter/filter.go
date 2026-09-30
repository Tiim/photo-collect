// Package filter parses the gallery filter from a query string, encodes it
// back for links and paging, and maps it onto the sqlc filter queries.
//
//	?tag=forrest&tag=summer&tag_mode=all&rating_min=3&uploader=anna&from=2026-06-01&to=2026-06-02&sort=captured
package filter

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/domain"
)

// MaxTags bounds the number of tags in one filter.
const MaxTags = 10

const (
	SortUploaded = "uploaded"
	SortCaptured = "captured"
	SortRating   = "rating"

	DirAsc  = "asc"
	DirDesc = "desc"

	ModeAll = "all"
	ModeAny = "any"

	dateLayout = "2006-01-02"
)

// Error is a validation problem that may be shown to the user.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func bad(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Filter is a validated gallery filter. The zero value is not valid; use
// Parse (or Default) so Sort, Dir and TagMode are set.
type Filter struct {
	Tags      []string // normalised, sorted, unique
	TagMode   string   // ModeAll (default) or ModeAny
	RatingMin int      // 1-5, 0 = unset
	RatingMax int      // 1-5, 0 = unset
	Uploader  string
	From, To  string // YYYY-MM-DD on the corrected capture time, "" = unset
	HasGPS    bool
	Sort      string
	Dir       string
}

// Default returns the unfiltered view: newest upload first.
func Default() Filter { return Filter{TagMode: ModeAll, Sort: SortUploaded, Dir: DirDesc} }

// Active reports whether any filter (not just the sort order) is set.
func (f Filter) Active() bool {
	return len(f.Tags) > 0 || f.RatingMin > 0 || f.RatingMax > 0 || f.Uploader != "" ||
		f.From != "" || f.To != "" || f.HasGPS
}

// Parse validates the filter query parameters. Empty values count as unset, so
// an unedited filter form submits cleanly.
func Parse(v url.Values) (Filter, error) {
	f := Default()

	seen := map[string]bool{}
	for _, raw := range v["tag"] {
		for _, part := range strings.Split(raw, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			name, err := domain.NormalizeTag(part)
			if err != nil {
				return f, bad("Invalid tag: %s", err)
			}
			if !seen[name] {
				seen[name] = true
				f.Tags = append(f.Tags, name)
			}
		}
	}
	if len(f.Tags) > MaxTags {
		return f, bad("At most %d tags can be combined", MaxTags)
	}
	sort.Strings(f.Tags)

	switch m := v.Get("tag_mode"); m {
	case "", ModeAll:
	case ModeAny:
		f.TagMode = ModeAny
	default:
		return f, bad("Unknown tag mode %q", m)
	}

	var err error
	if f.RatingMin, err = rating(v, "rating_min"); err != nil {
		return f, err
	}
	if f.RatingMax, err = rating(v, "rating_max"); err != nil {
		return f, err
	}
	if f.RatingMin > 0 && f.RatingMax > 0 && f.RatingMin > f.RatingMax {
		return f, bad("The minimum rating is above the maximum rating")
	}

	f.Uploader = strings.Join(strings.Fields(v.Get("uploader")), " ")
	if len([]rune(f.Uploader)) > 100 {
		return f, bad("Uploader name is too long")
	}

	if f.From, err = date(v, "from"); err != nil {
		return f, err
	}
	if f.To, err = date(v, "to"); err != nil {
		return f, err
	}
	if f.From != "" && f.To != "" && f.From > f.To {
		return f, bad("The start date is after the end date")
	}

	switch g := v.Get("has_gps"); g {
	case "", "0", "false", "off":
	case "1", "true", "on":
		f.HasGPS = true
	default:
		return f, bad("Invalid value for has_gps")
	}

	switch s := v.Get("sort"); s {
	case "":
	case SortUploaded, SortCaptured, SortRating:
		f.Sort = s
	default:
		return f, bad("Unknown sort order %q", s)
	}
	switch d := v.Get("dir"); d {
	case "":
	case DirAsc, DirDesc:
		f.Dir = d
	default:
		return f, bad("Unknown sort direction %q", d)
	}
	return f, nil
}

func rating(v url.Values, key string) (int, error) {
	s := strings.TrimSpace(v.Get(key))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 5 {
		return 0, bad("Rating filters must be between 1 and 5")
	}
	return n, nil
}

func date(v url.Values, key string) (string, error) {
	s := strings.TrimSpace(v.Get(key))
	if s == "" {
		return "", nil
	}
	if _, err := time.Parse(dateLayout, s); err != nil {
		return "", bad("Dates must look like 2026-06-01")
	}
	return s, nil
}

// Encode returns the query parameters that Parse turns back into f. Defaults
// are left out, so the unfiltered view has an empty query.
func (f Filter) Encode() url.Values {
	v := url.Values{}
	if len(f.Tags) > 0 {
		v["tag"] = append([]string(nil), f.Tags...)
		if f.TagMode == ModeAny {
			v.Set("tag_mode", ModeAny)
		}
	}
	if f.RatingMin > 0 {
		v.Set("rating_min", strconv.Itoa(f.RatingMin))
	}
	if f.RatingMax > 0 {
		v.Set("rating_max", strconv.Itoa(f.RatingMax))
	}
	if f.Uploader != "" {
		v.Set("uploader", f.Uploader)
	}
	if f.From != "" {
		v.Set("from", f.From)
	}
	if f.To != "" {
		v.Set("to", f.To)
	}
	if f.HasGPS {
		v.Set("has_gps", "1")
	}
	if f.Sort != SortUploaded {
		v.Set("sort", f.Sort)
	}
	if f.Dir != DirDesc {
		v.Set("dir", f.Dir)
	}
	return v
}

// Query is Encode as a query string without the leading "?".
func (f Filter) Query() string { return f.Encode().Encode() }

// TagsCSV is the tag list as shown in the filter form's text input.
func (f Filter) TagsCSV() string { return strings.Join(f.Tags, ", ") }

// Cursor marks the last row of a page: its sort key and seq.
type Cursor struct {
	Key string
	Seq int64
}

func (f Filter) tagsJSON() (sql.NullString, int64) {
	if len(f.Tags) == 0 {
		return sql.NullString{}, 0
	}
	b, err := json.Marshal(f.Tags)
	if err != nil {
		panic(err) // a []string always marshals
	}
	need := int64(len(f.Tags))
	if f.TagMode == ModeAny {
		need = 1
	}
	return sql.NullString{String: string(b), Valid: true}, need
}

func nullInt(n int) sql.NullInt64 { return sql.NullInt64{Int64: int64(n), Valid: n > 0} }

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func (f Filter) bounds() (from, to sql.NullString) {
	if f.From != "" {
		from = nullStr(f.From + " 00:00:00")
	}
	if f.To != "" {
		to = nullStr(f.To + " 23:59:59")
	}
	return from, to
}

func (f Filter) hasGPS() sql.NullInt64 { return sql.NullInt64{Int64: 1, Valid: f.HasGPS} }

// ListParams builds the arguments of ListImagesFiltered. after is nil for the first page.
func (f Filter) ListParams(folderID string, after *Cursor, pageSize int64) sqlc.ListImagesFilteredParams {
	tags, need := f.tagsJSON()
	from, to := f.bounds()
	p := sqlc.ListImagesFilteredParams{
		Sort: f.Sort, Dir: f.Dir, FolderID: folderID, PageSize: pageSize,
		RatingMin: nullInt(f.RatingMin), RatingMax: nullInt(f.RatingMax), Uploader: nullStr(f.Uploader),
		FromTime: from, ToTime: to, HasGps: f.hasGPS(), Tags: tags, TagsNeeded: need,
	}
	if after != nil {
		p.AfterKey = sql.NullString{String: after.Key, Valid: true}
		p.AfterSeq = sql.NullInt64{Int64: after.Seq, Valid: true}
	}
	return p
}

// CountParams builds the arguments of CountImagesFiltered.
func (f Filter) CountParams(folderID string) sqlc.CountImagesFilteredParams {
	p := f.ListParams(folderID, nil, 0)
	return sqlc.CountImagesFilteredParams{
		FolderID: p.FolderID, RatingMin: p.RatingMin, RatingMax: p.RatingMax, Uploader: p.Uploader,
		FromTime: p.FromTime, ToTime: p.ToTime, HasGps: p.HasGps, Tags: p.Tags, TagsNeeded: p.TagsNeeded,
	}
}

// RefsParams builds the arguments of ListFilteredRefs, returning at most maxRows matches.
func (f Filter) RefsParams(folderID string, maxRows int64) sqlc.ListFilteredRefsParams {
	p := f.ListParams(folderID, nil, 0)
	return sqlc.ListFilteredRefsParams{
		FolderID: p.FolderID, RatingMin: p.RatingMin, RatingMax: p.RatingMax, Uploader: p.Uploader,
		FromTime: p.FromTime, ToTime: p.ToTime, HasGps: p.HasGps, Tags: p.Tags, TagsNeeded: p.TagsNeeded,
		MaxRows: maxRows,
	}
}

// ParseCursor reads the after_key and after_seq paging parameters. ok is false
// for the first page.
func ParseCursor(v url.Values) (c *Cursor, err error) {
	seq := v.Get("after_seq")
	if seq == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 0 {
		return nil, errors.New("invalid cursor")
	}
	return &Cursor{Key: v.Get("after_key"), Seq: n}, nil
}

// Values returns the query parameters for the page after c.
func (f Filter) Values(c Cursor) url.Values {
	v := f.Encode()
	v.Set("after_key", c.Key)
	v.Set("after_seq", strconv.FormatInt(c.Seq, 10))
	return v
}
