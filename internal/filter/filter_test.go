package filter_test

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/filter"
)

func parse(t *testing.T, q string) filter.Filter {
	t.Helper()
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	f, err := filter.Parse(v)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	return f
}

func TestParseEmptyIsDefault(t *testing.T) {
	for _, q := range []string{"", "tag=&rating_min=&rating_max=&uploader=&from=&to=&has_gps=&sort=&dir=&tag_mode="} {
		f := parse(t, q)
		if !reflect.DeepEqual(f, filter.Default()) || f.Active() {
			t.Errorf("Parse(%q) = %+v", q, f)
		}
		if got := f.Query(); got != "" {
			t.Errorf("default encodes to %q", got)
		}
	}
}

// The request that motivated the feature: tag #forrest and rating >= 3.
func TestParseForestAndRating(t *testing.T) {
	f := parse(t, "tag=forrest&rating_min=3")
	if !reflect.DeepEqual(f.Tags, []string{"forrest"}) || f.RatingMin != 3 || f.TagMode != filter.ModeAll || !f.Active() {
		t.Fatalf("filter = %+v", f)
	}
	p := f.CountParams("f1")
	if p.Tags.String != `["forrest"]` || p.TagsNeeded != 1 || p.RatingMin.Int64 != 3 || !p.RatingMin.Valid || p.RatingMax.Valid {
		t.Fatalf("params = %+v", p)
	}
}

func TestParseFull(t *testing.T) {
	f := parse(t, "tag=Summer&tag=forrest, Summer&tag_mode=any&rating_min=2&rating_max=4&uploader=+anna++b+&from=2026-06-01&to=2026-06-02&has_gps=1&sort=captured&dir=asc")
	want := filter.Filter{
		Tags: []string{"forrest", "summer"}, TagMode: filter.ModeAny, RatingMin: 2, RatingMax: 4, Uploader: "anna b",
		From: "2026-06-01", To: "2026-06-02", HasGPS: true, Sort: filter.SortCaptured, Dir: filter.DirAsc,
	}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("filter = %+v\nwant     %+v", f, want)
	}
	p := f.ListParams("f1", nil, 61)
	if p.TagsNeeded != 1 || p.FromTime.String != "2026-06-01 00:00:00" || p.ToTime.String != "2026-06-02 23:59:59" || !p.HasGps.Valid || p.AfterSeq.Valid {
		t.Fatalf("params = %+v", p)
	}
	if p := parse(t, "tag=a&tag=b").CountParams("f"); p.TagsNeeded != 2 {
		t.Fatalf("AND needs both tags, got %d", p.TagsNeeded)
	}
	// Round trip.
	if g := parse(t, f.Query()); !reflect.DeepEqual(g, f) {
		t.Fatalf("round trip = %+v", g)
	}
}

func TestParseInvalid(t *testing.T) {
	tags := make([]string, filter.MaxTags+1)
	for i := range tags {
		tags[i] = "tag=t" + string(rune('a'+i))
	}
	for name, q := range map[string]string{
		"rating zero":     "rating_min=0",
		"rating six":      "rating_min=6",
		"rating text":     "rating_max=x",
		"rating inverted": "rating_min=4&rating_max=2",
		"bad date":        "from=yesterday",
		"date order":      "from=2026-06-02&to=2026-06-01",
		"bad sort":        "sort=random",
		"bad dir":         "dir=up",
		"bad mode":        "tag_mode=none",
		"bad gps":         "has_gps=maybe",
		"too many tags":   strings.Join(tags, "&"),
		"long uploader":   "uploader=" + strings.Repeat("x", 101),
		"tag with sql":    "tag=" + url.QueryEscape("a\x00b"),
	} {
		v, _ := url.ParseQuery(q)
		if _, err := filter.Parse(v); err == nil {
			t.Errorf("%s: %q accepted", name, q)
		} else if _, ok := err.(*filter.Error); !ok {
			t.Errorf("%s: error %T is not a *filter.Error", name, err)
		}
	}
}

func TestCursor(t *testing.T) {
	f := parse(t, "sort=rating")
	v := f.Values(filter.Cursor{Key: "003", Seq: 42})
	c, err := filter.ParseCursor(v)
	if err != nil || c == nil || c.Key != "003" || c.Seq != 42 {
		t.Fatalf("cursor = %+v, %v", c, err)
	}
	if c, err := filter.ParseCursor(url.Values{}); c != nil || err != nil {
		t.Fatalf("no cursor = %+v, %v", c, err)
	}
	if _, err := filter.ParseCursor(url.Values{"after_seq": {"x"}}); err == nil {
		t.Fatal("bad cursor accepted")
	}
	p := f.ListParams("f", c, 10)
	if !p.AfterSeq.Valid || p.AfterSeq.Int64 != 42 || p.AfterKey.String != "003" {
		t.Fatalf("params = %+v", p)
	}
}
