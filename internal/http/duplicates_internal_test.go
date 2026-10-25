package http

import (
	"reflect"
	"testing"

	"github.com/tiim/photo-collect/internal/database/sqlc"
)

func TestClusterDuplicates(t *testing.T) {
	pair := func(id, a, b string) sqlc.ListPendingDuplicatesRow {
		return sqlc.ListPendingDuplicatesRow{ID: id, ImageIDA: a, ImageIDB: b, AFilename: a + ".jpg", BFilename: b + ".jpg"}
	}
	// A~B and C~D start as two groups; B~C later joins them into one.
	// E~F stays on its own.
	rows := []sqlc.ListPendingDuplicatesRow{
		pair("p1", "a", "b"),
		pair("p2", "c", "d"),
		pair("p3", "e", "f"),
		pair("p4", "b", "c"),
		pair("p5", "a", "d"),
	}
	got := clusterDuplicates(rows)
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2: %+v", len(got), got)
	}
	ids := func(c DuplicateCluster) (out []string) {
		for _, img := range c.Images {
			out = append(out, img.ID)
		}
		return out
	}
	if got[0].ID != "p1" || !reflect.DeepEqual(ids(got[0]), []string{"a", "b", "c", "d"}) ||
		!reflect.DeepEqual(got[0].PairIDs, []string{"p1", "p2", "p4", "p5"}) {
		t.Errorf("first cluster = %+v", got[0])
	}
	if got[1].ID != "p3" || !reflect.DeepEqual(ids(got[1]), []string{"e", "f"}) {
		t.Errorf("second cluster = %+v", got[1])
	}
	if got[0].Images[2].Filename != "c.jpg" {
		t.Errorf("filename = %q", got[0].Images[2].Filename)
	}
	if clusterDuplicates(nil) != nil {
		t.Error("no pairs should give no clusters")
	}
}
