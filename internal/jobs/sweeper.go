package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/storage"
)

const (
	// SweepGrace is how old an unreferenced object must be before the sweeper
	// removes it, so uploads that stored their object but have not committed
	// the database row yet are never touched.
	SweepGrace = 24 * time.Hour
	// SweepInterval is how often the sweeper is scheduled.
	SweepInterval = 24 * time.Hour

	// The sweeper refuses to run when it would delete more than this share of
	// all objects (and at least sweepGuardMin of them): that pattern means the
	// database and the storage do not belong together.
	sweepGuardPercent = 20
	sweepGuardMin     = 100
)

// parseImageKey splits folders/<folder>/images/<image>/<kind>.
func parseImageKey(key string) (folderID, imageID string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 5 || parts[0] != "folders" || parts[2] != "images" {
		return "", "", false
	}
	switch parts[4] {
	case "original", "preview", "thumbnail":
		return parts[1], parts[3], true
	}
	return "", "", false
}

// EnqueueSweep queues a sweep unless one is already waiting or running.
func (q *Queue) EnqueueSweep(ctx context.Context) error {
	n, err := q.db.Q.CountUnfinishedJobsByType(ctx, TypeSweepOrphans)
	if err != nil || n > 0 {
		return err
	}
	return q.Enqueue(ctx, q.db.Q, TypeSweepOrphans, struct{}{})
}

// RunSweepSchedule enqueues a sweep now and then every SweepInterval until ctx is cancelled.
func (q *Queue) RunSweepSchedule(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		if err := q.EnqueueSweep(ctx); err != nil && ctx.Err() == nil {
			q.log.Error("schedule orphan sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepOrphans deletes stored image objects that no image row refers to.
// Trashed images keep their rows, so their objects are never candidates, and
// objects of folders being deleted are left to the delete_folder job.
func (h *Handlers) sweepOrphans(ctx context.Context, _ json.RawMessage) error {
	// Read the database first: an upload stores its object before it inserts
	// the row, so anything listed afterwards that is missing here is either
	// an orphan or younger than the grace period.
	refs, err := h.DB.Q.ListAllImageRefs(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		known[r.ID] = struct{}{}
	}
	deleting, err := h.DB.Q.ListDeletedFolderIDs(ctx)
	if err != nil {
		return err
	}
	deletingSet := make(map[string]struct{}, len(deleting))
	for _, id := range deleting {
		deletingSet[id] = struct{}{}
	}

	objs, err := h.Store.List(ctx, "folders/")
	if err != nil {
		return fmt.Errorf("list objects: %w", err)
	}

	now := time.Now()
	var candidates []storage.ObjectInfo
	present := map[string]struct{}{} // image ids that have an original
	var noAge int
	for _, o := range objs {
		folderID, imageID, ok := parseImageKey(o.Key)
		if !ok {
			continue
		}
		if strings.HasSuffix(o.Key, "/original") {
			present[imageID] = struct{}{}
		}
		if _, ok := known[imageID]; ok {
			continue
		}
		if _, ok := deletingSet[folderID]; ok {
			continue
		}
		if o.Modified.IsZero() {
			noAge++
			continue
		}
		if now.Sub(o.Modified) < SweepGrace {
			continue
		}
		candidates = append(candidates, o)
	}
	if noAge > 0 {
		h.Log.Warn("orphan sweep: storage does not report object age, skipping objects", "count", noAge)
	}

	// Reverse check: rows whose original is gone cannot be repaired.
	var missing []string
	for _, r := range refs {
		if _, ok := present[r.ID]; !ok {
			missing = append(missing, r.ID)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		sample := missing[:min(len(missing), 20)]
		h.Log.Warn("orphan sweep: images without a stored original", "count", len(missing), "sample_image_ids", sample)
	}

	if len(candidates) >= sweepGuardMin && len(candidates)*100 > len(objs)*sweepGuardPercent {
		err := fmt.Errorf("orphan sweep aborted: %d of %d objects have no image row (is the database pointed at the right storage?)", len(candidates), len(objs))
		h.Log.Error(err.Error())
		return Permanent(err)
	}

	var deleted, failed int
	for _, o := range candidates {
		if err := h.Store.Delete(ctx, o.Key); err != nil {
			failed++
			h.Log.Error("orphan sweep: delete failed", "key", o.Key, "err", err)
			continue
		}
		deleted++
		h.Log.Info("orphan sweep: deleted object", "key", o.Key, "bytes", o.Size, "modified", o.Modified.UTC().Format(time.RFC3339))
	}
	h.Log.Info("orphan sweep finished", "listed", len(objs), "deleted", deleted, "failed", failed, "images_missing_original", len(missing))
	if failed > 0 {
		return fmt.Errorf("%d of %d orphaned objects could not be deleted", failed, len(candidates))
	}
	return nil
}
