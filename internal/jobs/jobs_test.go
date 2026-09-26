package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/jobs"
)

func open(t *testing.T) *database.DB {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestJobRunsAndCompletes(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan string, 1)
	q.Handle("test", func(ctx context.Context, p json.RawMessage) error { done <- string(p); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	if err := q.Enqueue(ctx, db.Q, "test", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-done:
		if p != `{"n":1}` {
			t.Fatalf("payload %s", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job did not run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		db.QueryRow("SELECT status FROM jobs").Scan(&status)
		if status == "done" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailedJobIsRescheduled(t *testing.T) {
	db := open(t)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls atomic.Int32
	q.Handle("bad", func(ctx context.Context, p json.RawMessage) error { calls.Add(1); return errors.New("boom") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	q.Enqueue(ctx, db.Q, "bad", nil)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var status, errMsg string
		var attempts int
		var runAt string
		db.QueryRow("SELECT status, attempts, run_at, COALESCE(error,'') FROM jobs").Scan(&status, &attempts, &runAt, &errMsg)
		if status == "pending" && attempts == 1 && errMsg == "boom" {
			if runAt <= database.Time(time.Now()) {
				t.Fatalf("retry not delayed: %s", runAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status=%s attempts=%d err=%s", status, attempts, errMsg)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestOrphanedRunningJobIsRequeuedOnStart(t *testing.T) {
	db := open(t)
	// Simulate a crash mid-job.
	db.Exec(`INSERT INTO jobs (type, status, attempts, started_at) VALUES ('test', 'running', 1, '2020-01-01T00:00:00.000Z')`)
	q := jobs.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{}, 1)
	q.Handle("test", func(ctx context.Context, p json.RawMessage) error { done <- struct{}{}; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("orphaned job was not resumed")
	}
}
