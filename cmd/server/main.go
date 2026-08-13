package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/httpd"
	"rables/internal/jobs"
	"rables/internal/service/crosspost"
	newslettersvc "rables/internal/service/newsletter"
	"rables/internal/service/transfer"
	"rables/internal/service/twitterarchive"
	"rables/internal/service/twittersync"
	"rables/internal/templates"
)

func main() {
	os.Exit(run())
}

// run is main with an exit code: defers (scheduler stop, database close)
// execute on every return path, unlike os.Exit which skips them.
func run() int {
	startedAt := time.Now()
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	logger := config.NewLogger(cfg)
	slog.SetDefault(logger)

	database, err := db.Open(cfg.DataDir)
	if err != nil {
		logger.Error("open database", "error", err)
		return 1
	}
	defer database.Close()

	renderer, err := templates.New()
	if err != nil {
		logger.Error("load templates", "error", err)
		return 1
	}

	server := httpd.NewServer(database, cfg, logger, renderer)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpd.NewRouter(server),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	worker := jobs.NewWorker(database)
	jobs.RegisterPublishHandlers(worker, database)
	twitterarchive.RegisterImportHandler(worker, database, cfg.DataDir)
	crosspost.RegisterCrosspostHandlers(worker, database, cfg.DataDir)
	newslettersvc.RegisterSendHandlers(worker, database, cfg.DataDir)
	transfer.RegisterExportHandlers(worker, database, cfg.DataDir)
	transfer.RegisterImportHandlers(worker, database, cfg.DataDir, server.Settings().Invalidate)
	crosspost.RegisterFetchCommentsHandlers(worker, database, cfg.DataDir)

	// Startup recovery, before the worker starts polling: requeue jobs a dead
	// process left running, and fail twitter archive imports stuck active so
	// they stop blocking new imports. The 5 minute cutoff protects rows
	// freshly claimed by another process during a rolling deploy.
	staleBefore := time.Now().Add(-5 * time.Minute)
	q := query.New(database)
	if n, err := jobs.RecoverStaleJobs(ctx, q, staleBefore); err != nil {
		logger.Error("recover stale jobs", "error", err)
	} else if n > 0 {
		logger.Info("requeued stale running jobs", "count", n)
	}
	if n, err := twitterarchive.RecoverStaleImports(ctx, q, staleBefore); err != nil {
		logger.Error("recover stale twitter archive imports", "error", err)
	} else if n > 0 {
		logger.Info("failed stale twitter archive imports", "count", n)
	}
	// Last, sweep orphaned import leftovers (uploads whose process crashed
	// between writing the file and enqueueing its job, mid-write .part temp
	// files, extract_* staging dirs): rows/jobs the recovery steps just
	// terminalized stop protecting their files in the same sweep.
	if n, err := jobs.CleanupOrphanImportFiles(ctx, q, cfg.DataDir, startedAt); err != nil {
		logger.Error("clean up orphan import uploads", "error", err)
	} else if n > 0 {
		logger.Info("removed orphan import uploads", "count", n)
	}
	// Finally, reap files rows and blobs a dead process left unreferenced
	// (crash between the autocommitted CreateFile and the batch transaction
	// that would have attached them).
	if n, err := jobs.ReapOrphanFiles(ctx, q, cfg.DataDir, startedAt); err != nil {
		logger.Error("reap orphan files", "error", err)
	} else if n > 0 {
		logger.Info("removed orphan files", "count", n)
	}

	workerDone := make(chan struct{})
	go func() {
		worker.Start(ctx)
		close(workerDone)
	}()

	syncer := twittersync.NewSyncer(database, cfg.DataDir)
	server.Ext.Store("twittersync", syncer)

	scheduler := jobs.NewScheduler(ctx, database, cfg.DataDir)
	scheduler.RegisterHook("sync_twitter", syncer.Run)
	scheduler.Start()
	defer scheduler.Stop()

	// A serve failure (e.g. the port is taken at startup) is delivered over
	// serveErr instead of calling os.Exit, so the worker wait and the
	// deferred scheduler stop / database close execute on that path too.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	exitCode := 0
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		logger.Error("serve", "error", err)
		exitCode = 1
	}
	stop()

	// A shutdown failure must not skip the worker wait and the deferred
	// scheduler stop / database close: record it and exit non-zero at the end.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "error", err)
		exitCode = 1
	}

	// Wait for the worker to return from Start before the deferred
	// database.Close runs: an in-flight handler is interrupted by the ctx
	// cancellation, but its bookkeeping write (context.WithoutCancel) must
	// still land. Bound the wait so a stuck handler cannot block shutdown.
	select {
	case <-workerDone:
	case <-time.After(15 * time.Second):
		logger.Warn("worker did not stop within 15s; continuing shutdown")
	}
	logger.Info("shutdown complete")
	return exitCode
}
