package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/filter"
)

// MapCard is the map at the top of the folder page. It is shown (JSONURL set)
// when maps are on and at least one photo matching the filter has a position;
// map.js then fetches JSONURL. OOB re-renders the card after a filter change.
type MapCard struct {
	JSONURL string
	OOB     bool
}

func (s *Server) mapCard(ctx context.Context, folder sqlc.Folder, f filter.Filter) (MapCard, error) {
	f.HasGPS = true
	n, err := s.db.Q.CountImagesFiltered(ctx, f.CountParams(folder.ID))
	if err != nil || n == 0 {
		return MapCard{}, err
	}
	return MapCard{JSONURL: "/folders/" + folder.ID + "/map.json" + query(f.Encode())}, nil
}

type mapPoint struct {
	ID  string  `json:"id"`
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// folderMapJSON lists the geotagged photos matching the filter: id and position only.
func (s *Server) folderMapJSON(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	flt, ok := s.parseFilter(w, r)
	if !ok {
		return
	}
	flt.HasGPS = true
	refs, err := s.db.Q.ListFilteredRefs(r.Context(), flt.RefsParams(f.ID, maxMatching))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	points := make([]mapPoint, 0, len(refs))
	for _, ref := range refs {
		if ref.GpsLat.Valid && ref.GpsLon.Valid {
			points = append(points, mapPoint{ID: ref.ID, Lat: ref.GpsLat.Float64, Lon: ref.GpsLon.Float64})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Points []mapPoint `json:"points"`
	}{points})
}
