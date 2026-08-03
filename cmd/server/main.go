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
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logger := config.NewLogger(cfg)
	slog.SetDefault(logger)

	database, err := db.Open(cfg.DataDir)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	renderer, err := templates.New()
	if err != nil {
		logger.Error("load templates", "error", err)
		os.Exit(1)
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
	enqueuer := jobs.NewEnqueuer(database)
	jobs.RegisterPublishHandlers(worker, database, enqueuer)
	twitterarchive.RegisterImportHandler(worker, database, cfg.DataDir)
	crosspost.RegisterCrosspostHandlers(worker, database, cfg.DataDir)
	newslettersvc.RegisterSendHandlers(worker, database, cfg.DataDir)
	transfer.RegisterExportHandlers(worker, database, cfg.DataDir)
	transfer.RegisterImportHandlers(worker, database, cfg.DataDir)
	crosspost.RegisterFetchCommentsHandlers(worker, database, cfg.DataDir)
	go worker.Start(ctx)

	syncer := twittersync.NewSyncer(database, cfg.DataDir)
	server.Ext.Store("twittersync", syncer)

	scheduler := jobs.NewScheduler(database, cfg.DataDir)
	scheduler.RegisterHook("sync_twitter", syncer.Run)
	scheduler.Start()
	defer scheduler.Stop()

	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
