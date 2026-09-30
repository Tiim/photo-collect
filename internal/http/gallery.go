package http

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/internal/filter"
)

// maxMatching bounds "all matching" operations (export, trash) and the map.
const maxMatching = 50000

// GridView is one page of tiles. NextQuery is the query string that fetches
// the next page, empty on the last page.
type GridView struct {
	FolderID  string
	Images    []TileView
	NextQuery string
}

// FilterView feeds the filter bar.
type FilterView struct {
	FolderID  string
	F         filter.Filter
	Query     string // "?..." of the applied filter, "" when unfiltered
	Uploaders []string
}

// GalleryView is the swappable part of the folder page: toolbar and grid.
type GalleryView struct {
	FolderID string
	Filter   FilterView
	Grid     GridView
	Matching int64 // images matching the filter
	Total    int64 // images in the folder
	Map      MapCard
}

func query(v url.Values) string {
	if q := v.Encode(); q != "" {
		return "?" + q
	}
	return ""
}

// parseFilter reads the filter from the URL query or writes a 400.
func parseFilter(w http.ResponseWriter, r *http.Request) (filter.Filter, bool) {
	f, err := filter.Parse(r.URL.Query())
	if err != nil {
		var fe *filter.Error
		if errors.As(err, &fe) {
			http.Error(w, fe.Msg, http.StatusBadRequest)
		} else {
			http.Error(w, "Bad request", http.StatusBadRequest)
		}
		return f, false
	}
	return f, true
}

// imageOf drops the sort key from a filtered row. TestImageRowMatches keeps it
// in step with the generated types.
func imageOf(r sqlc.ListImagesFilteredRow) sqlc.Image {
	return sqlc.Image{
		Seq: r.Seq, ID: r.ID, FolderID: r.FolderID, OriginalFilename: r.OriginalFilename, MimeType: r.MimeType,
		SizeBytes: r.SizeBytes, Width: r.Width, Height: r.Height, Sha256: r.Sha256, UploaderNickname: r.UploaderNickname,
		Rating: r.Rating, ThumbnailReady: r.ThumbnailReady, PreviewReady: r.PreviewReady, CreatedAt: r.CreatedAt,
		DeviceKey: r.DeviceKey, ExifTime: r.ExifTime, QrScanned: r.QrScanned, IsCalibration: r.IsCalibration,
		CalibRefTime: r.CalibRefTime, TimeOffsetSeconds: r.TimeOffsetSeconds, Phash: r.Phash,
		PhashAttemptedAt: r.PhashAttemptedAt, DerivativeVersion: r.DerivativeVersion, DeletedAt: r.DeletedAt,
		DeletedBy: r.DeletedBy, GpsLat: r.GpsLat, GpsLon: r.GpsLon, GpsAttemptedAt: r.GpsAttemptedAt,
	}
}

// gridView returns the page of f after the cursor (the first page for nil).
func (s *Server) gridView(ctx context.Context, folderID string, f filter.Filter, after *filter.Cursor) (GridView, error) {
	rows, err := s.db.Q.ListImagesFiltered(ctx, f.ListParams(folderID, after, pageSize+1))
	if err != nil {
		return GridView{}, err
	}
	g := GridView{FolderID: folderID}
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		last := rows[len(rows)-1]
		key, _ := last.SortKey.(string)
		g.NextQuery = f.Values(filter.Cursor{Key: key, Seq: last.Seq}).Encode()
	}
	for _, row := range rows {
		g.Images = append(g.Images, tile(imageOf(row)))
	}
	return g, nil
}

func (s *Server) filterView(ctx context.Context, folderID string, f filter.Filter) (FilterView, error) {
	ups, err := s.db.Q.ListFolderUploaders(ctx, folderID)
	return FilterView{FolderID: folderID, F: f, Query: query(f.Encode()), Uploaders: ups}, err
}

func (s *Server) galleryView(ctx context.Context, folder sqlc.Folder, f filter.Filter) (GalleryView, error) {
	folderID := folder.ID
	g := GalleryView{FolderID: folderID}
	var err error
	if g.Map, err = s.mapCard(ctx, folder, f); err != nil {
		return g, err
	}
	if g.Filter, err = s.filterView(ctx, folderID, f); err != nil {
		return g, err
	}
	if g.Grid, err = s.gridView(ctx, folderID, f, nil); err != nil {
		return g, err
	}
	if g.Matching, err = s.db.Q.CountImagesFiltered(ctx, f.CountParams(folderID)); err != nil {
		return g, err
	}
	g.Total, err = s.db.Q.CountImagesInFolder(ctx, folderID)
	return g, err
}

// folderGallery returns the toolbar and grid for a filter (htmx swaps it into
// the folder page) and tells htmx which URL to show in the address bar.
func (s *Server) folderGallery(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	flt, ok := parseFilter(w, r)
	if !ok {
		return
	}
	g, err := s.galleryView(r.Context(), f, flt)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("HX-Push-Url", "/folders/"+f.ID+g.Filter.Query)
	g.Map.OOB = true
	s.fragment(w, r, "gallery_response", struct {
		Gallery GalleryView
		Counts  *FolderCounts
	}{g, nil})
}

// folderImages returns the next page of tiles (the paging sentinel's target).
func (s *Server) folderImages(w http.ResponseWriter, r *http.Request) {
	f, ok := s.folder(w, r)
	if !ok {
		return
	}
	flt, ok := parseFilter(w, r)
	if !ok {
		return
	}
	cur, err := filter.ParseCursor(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g, err := s.gridView(r.Context(), f.ID, flt, cur)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fragment(w, r, "image_grid", g)
}

// matchingIDs resolves the filter to image IDs for "all matching" actions.
func (s *Server) matchingIDs(ctx context.Context, folderID string, f filter.Filter) ([]string, error) {
	refs, err := s.db.Q.ListFilteredRefs(ctx, f.RefsParams(folderID, maxMatching))
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids, nil
}
