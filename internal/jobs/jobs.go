// Package jobs implements a durable, SQLite-backed background job queue.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/database/sqlc"
)

// Job types.
const (
	TypeDeriveImage          = "derive_image"
	TypeAnalyzeImage         = "analyze_image"
	TypeDeleteFolder         = "delete_folder"
	TypeBuildExport          = "build_export"
	TypeScanFolderDuplicates = "scan_folder_duplicates"
)

// Job payloads.
type (
	DerivePayload struct {
		ImageID string `json:"image_id"`
	}
	AnalyzePayload struct {
		ImageID string `json:"image_id"`
	}
	DeleteFolderPayload struct {
		FolderID string `json:"folder_id"`
	}
	ExportPayload struct {
		ExportID string `json:"export_id"`
	}
	ScanFolderDuplicatesPayload struct {
		FolderID string `json:"folder_id"`
	}
)

const (
	maxAttempts  = 5
	pollInterval = 2 * time.Second
	// A running job untouched for this long is assumed to belong to a crashed worker.
	leaseTimeout = 30 * time.Minute

	// DuplicateHashThreshold is the maximum dHash Hamming distance (0-64)
	// for two images to be flagged as near-duplicates. Kept conservative to
	// favor fewer false positives.
	DuplicateHashThreshold = 6
	// duplicateScanDebounce is how long after the last relevant event
	// (upload, hash computed) a folder's duplicate scan waits before running,
	// so a burst of uploads produces one scan instead of one per photo.
	duplicateScanDebounce = 30 * time.Second
)

// Permanent wraps err so the queue fails the job at once instead of retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Handler processes a job. It must be idempotent: jobs may run more than once.
type Handler func(ctx context.Context, payload json.RawMessage) error

type Queue struct {
	db       *database.DB
	handlers map[string]Handler
	wake     chan struct{}
	log      *slog.Logger
}

func New(db *database.DB, log *slog.Logger) *Queue {
	return &Queue{db: db, handlers: map[string]Handler{}, wake: make(chan struct{}, 1), log: log}
}

// Handle registers the handler for a job type. Call before Run.
func (q *Queue) Handle(typ string, h Handler) { q.handlers[typ] = h }

// Enqueue adds a job using the given queries handle (which may be a
// transaction, so the job is enqueued atomically with related changes).
func (q *Queue) Enqueue(ctx context.Context, qs *sqlc.Queries, typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = qs.EnqueueJob(ctx, sqlc.EnqueueJobParams{Type: typ, Payload: string(b), RunAt: database.Time(time.Now())})
	if err != nil {
		return err
	}
	q.Notify()
	return nil
}

// ScheduleFolderScan debounces a duplicate-detection scan for folderID: if
// one is already pending, its run time is pushed further into the future;
// otherwise a new job is queued. Called both when a new upload lands and
// again once that upload's perceptual hash finishes computing, so the scan
// always runs a fixed delay after the last such event.
func (q *Queue) ScheduleFolderScan(ctx context.Context, qs *sqlc.Queries, folderID string) error {
	payload, err := json.Marshal(ScanFolderDuplicatesPayload{FolderID: folderID})
	if err != nil {
		return err
	}
	runAt := database.Time(time.Now().Add(duplicateScanDebounce))
	n, err := qs.RescheduleJobByPayload(ctx, sqlc.RescheduleJobByPayloadParams{
		RunAt: runAt, Type: TypeScanFolderDuplicates, Payload: string(payload),
	})
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = qs.EnqueueJob(ctx, sqlc.EnqueueJobParams{Type: TypeScanFolderDuplicates, Payload: string(payload), RunAt: runAt})
	return err
}

// Notify wakes idle workers. Safe to call at any time.
func (q *Queue) Notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run starts n workers and blocks until ctx is cancelled and they have stopped.
func (q *Queue) Run(ctx context.Context, n int) {
	// Single-node deployment: anything still "running" at startup is orphaned.
	if rows, err := q.db.Q.RequeueStaleJobs(ctx, sql.NullString{String: database.Time(time.Now()), Valid: true}); err != nil {
		q.log.Error("requeue orphaned jobs", "err", err)
	} else if rows > 0 {
		q.log.Info("requeued orphaned jobs", "count", rows)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.worker(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.maintenance(ctx)
	}()
	wg.Wait()
}

func (q *Queue) worker(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := q.db.Q.ClaimJob(ctx, sql.NullString{String: database.Time(time.Now()), Valid: true})
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			case <-time.After(pollInterval):
			}
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				q.log.Error("claim job", "err", err)
				sleep(ctx, pollInterval)
			}
			continue
		}
		// More work may be pending; let another idle worker look.
		q.Notify()
		q.run(ctx, job)
	}
}

func (q *Queue) run(ctx context.Context, job sqlc.Job) {
	log := q.log.With("job_id", job.ID, "job_type", job.Type, "attempt", job.Attempts)
	h, ok := q.handlers[job.Type]
	var err error
	if !ok {
		err = fmt.Errorf("no handler for job type %q", job.Type)
	} else {
		err = safeCall(ctx, h, json.RawMessage(job.Payload))
	}
	// Persist the outcome even if we are shutting down.
	dbctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := database.Time(time.Now())

	switch {
	case err == nil:
		if e := q.db.Q.CompleteJob(dbctx, sqlc.CompleteJobParams{FinishedAt: sql.NullString{String: now, Valid: true}, ID: job.ID}); e != nil {
			log.Error("complete job", "err", e)
		}
	case ctx.Err() != nil:
		// Shutdown interrupted the job; put it back without counting the attempt as a failure.
		_ = q.db.Q.RetryJobLater(dbctx, sqlc.RetryJobLaterParams{RunAt: now, Error: sql.NullString{String: "interrupted by shutdown", Valid: true}, ID: job.ID})
	case job.Attempts >= maxAttempts || isPermanent(err):
		log.Error("job failed permanently", "err", err)
		if e := q.db.Q.FailJob(dbctx, sqlc.FailJobParams{FinishedAt: sql.NullString{String: now, Valid: true}, Error: sql.NullString{String: err.Error(), Valid: true}, ID: job.ID}); e != nil {
			log.Error("mark job failed", "err", e)
		}
	default:
		backoff := time.Duration(1<<job.Attempts) * 5 * time.Second
		log.Warn("job failed, will retry", "err", err, "retry_in", backoff)
		if e := q.db.Q.RetryJobLater(dbctx, sqlc.RetryJobLaterParams{RunAt: database.Time(time.Now().Add(backoff)), Error: sql.NullString{String: err.Error(), Valid: true}, ID: job.ID}); e != nil {
			log.Error("reschedule job", "err", e)
		}
	}
}

func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

func safeCall(ctx context.Context, h Handler, payload json.RawMessage) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, payload)
}

// maintenance requeues jobs whose lease expired and prunes old finished jobs.
func (q *Queue) maintenance(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		if n, err := q.db.Q.RequeueStaleJobs(ctx, sql.NullString{String: database.Time(now.Add(-leaseTimeout)), Valid: true}); err != nil {
			q.log.Error("requeue stale jobs", "err", err)
		} else if n > 0 {
			q.log.Warn("requeued stale jobs", "count", n)
			q.Notify()
		}
		if err := q.db.Q.DeleteOldJobs(ctx, sql.NullString{String: database.Time(now.Add(-7 * 24 * time.Hour)), Valid: true}); err != nil {
			q.log.Error("prune jobs", "err", err)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
