// Command photos runs the event photo collection service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tiim/photo-collect/internal/config"
	"github.com/tiim/photo-collect/internal/database"
	"github.com/tiim/photo-collect/internal/downloads"
	apphttp "github.com/tiim/photo-collect/internal/http"
	"github.com/tiim/photo-collect/internal/images"
	"github.com/tiim/photo-collect/internal/jobs"
	"github.com/tiim/photo-collect/internal/oidc"
	"github.com/tiim/photo-collect/internal/sessions"
	"github.com/tiim/photo-collect/internal/storage"
	"github.com/tiim/photo-collect/internal/storage/filesystem"
	"github.com/tiim/photo-collect/internal/storage/s3"
	"github.com/tiim/photo-collect/internal/storage/webdav"
	"github.com/tiim/photo-collect/internal/uploads"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	var err error
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		err = serve(log)
	case "backup":
		err = backup(os.Args[2:])
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q (available: serve, backup <dest-file>, healthcheck)", cmd)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// backup writes a consistent snapshot of the SQLite database using VACUUM INTO.
// It is safe to run while the service is running.
func backup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: photos backup <dest-file>")
	}
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "/data/photos.db"
	}
	dest, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", dest)
	}
	ctx := context.Background()
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Backup(ctx, dest); err != nil {
		return err
	}
	fmt.Println("backup written to", dest)
	return nil
}

// healthcheck probes the local readiness endpoint; used as the Docker HEALTHCHECK
// so the image needs no curl.
func healthcheck() error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := stdhttp.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusOK {
		return fmt.Errorf("readiness returned %s", resp.Status)
	}
	return nil
}

// backfillPHashes re-enqueues thumbnail/preview derivation for every image
// that predates the duplicate-detection feature (and so has no perceptual
// hash yet), so existing folders get near-duplicate detection without a
// dedicated periodic sweep. Re-running TypeDeriveImage is idempotent.
func backfillPHashes(ctx context.Context, db *database.DB, queue *jobs.Queue, log *slog.Logger) error {
	missing, err := db.Q.ListImagesMissingPHash(ctx)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	log.Info("backfilling perceptual hashes", "count", len(missing))
	for _, img := range missing {
		if err := queue.Enqueue(ctx, db.Q, jobs.TypeDeriveImage, jobs.DerivePayload{ImageID: img.ID}); err != nil {
			return err
		}
	}
	return nil
}

// backfillGPS queues a position lookup for every image that predates GPS
// support. Each image is attempted once (the job records it), so this is
// cheap on later starts.
func backfillGPS(ctx context.Context, db *database.DB, queue *jobs.Queue, log *slog.Logger) error {
	missing, err := db.Q.ListImagesMissingGPS(ctx)
	if err != nil || len(missing) == 0 {
		return err
	}
	log.Info("backfilling GPS positions", "count", len(missing))
	for _, id := range missing {
		if err := queue.Enqueue(ctx, db.Q, jobs.TypeExtractGPS, jobs.ExtractGPSPayload{ImageID: id}); err != nil {
			return err
		}
	}
	return nil
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o755); err != nil {
		return err
	}
	db, err := database.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	var store storage.Store
	switch cfg.StorageBackend {
	case "s3":
		store, err = s3.New(ctx, s3.Options{
			Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket, Region: cfg.S3Region,
			AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, PathStyle: cfg.S3PathStyle,
		})
	case "webdav":
		store, err = webdav.New(ctx, webdav.Options{
			URL: cfg.WebDAVURL, User: cfg.WebDAVUser, Password: cfg.WebDAVPassword, BasePath: cfg.WebDAVBasePath,
		})
	default:
		store, err = filesystem.New(cfg.StoragePath)
	}
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}

	secure := strings.HasPrefix(cfg.BaseURL, "https://")
	signer := sessions.NewSigner(cfg.SessionSecret)
	sessionMgr := sessions.NewManager(db, cfg.SessionTTL, secure)
	oidcHandler := oidc.New(oidc.Config{
		IssuerURL: cfg.OIDCIssuerURL, ClientID: cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.OIDCRedirectURL,
		RequireVerifiedEmail: cfg.OIDCRequireVerifiedEmail,
	}, db, sessionMgr, signer, secure, log)

	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}
	log.Info("access control", "oidc_restrictions", oidcHandler.Restrictions(), "trusted_proxies", len(cfg.TrustedProxies))

	queue := jobs.New(db, log)
	processor := images.NewGoProcessor(cfg.ThumbnailSize, cfg.PreviewSize, max(1, min(cfg.WorkerCount, runtime.NumCPU())))
	(&jobs.Handlers{DB: db, Store: store, Processor: processor, Queue: queue, ExportDir: cfg.ExportDir, Log: log}).Register(queue)
	if err := backfillPHashes(ctx, db, queue, log); err != nil {
		return fmt.Errorf("backfill perceptual hashes: %w", err)
	}
	if err := backfillGPS(ctx, db, queue, log); err != nil {
		return fmt.Errorf("backfill GPS positions: %w", err)
	}
	dl, err := downloads.New(db, store, queue, cfg.ExportDir, cfg.ExportTTL, log)
	if err != nil {
		return err
	}
	dl.SetLimits(downloads.Limits{MaxConcurrent: cfg.ExportMaxConcurrent, MaxBytes: cfg.ExportMaxBytes})
	up := uploads.New(db, store, queue, uploads.Limits{
		MaxFileSize: cfg.UploadMaxFileSize, MaxImagesPerFolder: cfg.UploadMaxImagesPerFolder,
		MaxPixels: cfg.UploadMaxPixels,
	}, log)

	srv, err := apphttp.NewServer(apphttp.Deps{
		Config: cfg, DB: db, Store: store, Sessions: sessionMgr, Signer: signer, OIDC: oidcHandler,
		Uploads: up, Downloads: dl, Queue: queue, Log: log,
	})
	if err != nil {
		return err
	}
	httpSrv := &stdhttp.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No overall Read/WriteTimeout: uploads and image streams can legitimately take long.
	}

	var wg sync.WaitGroup
	background := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	background(func() { queue.Run(ctx, cfg.WorkerCount) })
	background(func() { dl.RunCleanup(ctx) })
	background(func() { queue.RunSweepSchedule(ctx) })
	background(func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := sessionMgr.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
				log.Error("purge sessions", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "storage", cfg.StorageBackend)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	wg.Wait()
	return nil
}
