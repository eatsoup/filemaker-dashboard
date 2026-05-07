package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"filemaker-dashboard/internal/auth"
	"filemaker-dashboard/internal/config"
	"filemaker-dashboard/internal/ingest"
	"filemaker-dashboard/internal/server"
	"filemaker-dashboard/internal/store"
)

//go:embed web/templates/*.html
var templatesFS embed.FS

//go:embed web/static
var staticFS embed.FS

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	importPath := flag.String("import", "", "import an older log file (e.g. Access-old.log) and exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	s, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	authMgr := &auth.Manager{Store: s, SessionTTL: cfg.SessionTTL}
	if err := authMgr.EnsureAdminFromConfig(cfg.InitialAdmin.Username, cfg.InitialAdmin.Password); err != nil {
		logger.Error("ensure admin user", "err", err)
		os.Exit(1)
	}

	tmplSub, err := fs.Sub(templatesFS, "web/templates")
	if err != nil {
		logger.Error("templates fs", "err", err)
		os.Exit(1)
	}
	staticSub, err := fs.Sub(staticFS, "web/static")
	if err != nil {
		logger.Error("static fs", "err", err)
		os.Exit(1)
	}

	srv, err := server.New(s, authMgr, cfg.SessionTTL, cfg.Defaults, tmplSub, staticSub, logger)
	if err != nil {
		logger.Error("build server", "err", err)
		os.Exit(1)
	}

	ing := ingest.New(s, cfg.LogfilePath, cfg.IngestInterval, cfg.AbandonedSessionAfter, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *importPath != "" {
		logger.Info("import starting", "path", *importPath)
		if err := ing.ImportFile(ctx, *importPath); err != nil {
			logger.Error("import failed", "err", err)
			os.Exit(1)
		}
		logger.Info("import complete")
		return
	}

	go ing.Run(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(staticFS),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	// Periodically purge expired web sessions.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = s.PurgeExpiredWebSessions()
			}
		}
	}()

	logger.Info("starting server", "addr", cfg.ListenAddr, "logfile", cfg.LogfilePath)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server", "err", err)
		os.Exit(1)
	}
}
