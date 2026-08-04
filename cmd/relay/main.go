package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codeagentrouter/internal/api"
	"codeagentrouter/internal/auth"
	"codeagentrouter/internal/config"
	"codeagentrouter/internal/quota"
	"codeagentrouter/internal/ratelimit"
	"codeagentrouter/internal/report"
	"codeagentrouter/internal/store"
	"codeagentrouter/internal/upstream"
	"codeagentrouter/web"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	dataDir := flag.String("data", "data", "directory for state.json")
	logDir := flag.String("logs", "", "directory for request logs (defaults to config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	encKey := os.Getenv(cfg.Security.EncryptKeyEnv)
	if encKey == "" {
		encKey = cfg.Security.EncryptKey
	}
	if encKey == "" {
		log.Println("WARNING: no encryption key set, using insecure default")
		encKey = "dev-only-insecure-key"
	}
	adminPass := os.Getenv(cfg.Security.AdminPasswordEnv)
	if adminPass == "" {
		adminPass = cfg.Security.AdminPassword
	}

	logger := log.New(os.Stderr, "[relay] ", log.LstdFlags)

	st, err := store.New(filepath.Join(*dataDir, "state.json"), encKey, logger.Printf)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	if *logDir == "" {
		*logDir = cfg.Logging.Dir
	}
	reports := report.NewService(*logDir)
	logWriter, err := report.NewLogger(*logDir, reports.Invalidate)
	if err != nil {
		log.Fatalf("init request logger: %v", err)
	}

	am := auth.New(st, cfg.Security.AdminUsername, adminPass)
	eq := quota.New(cfg, st)
	rl := ratelimit.New(cfg.Quota.PerMinuteRequests, time.Minute)
	up := upstream.New(st)

	handler := api.NewServer(cfg, st, am, eq, rl, up, reports, logWriter, web.FS)
	srv := &http.Server{Addr: cfg.Server.Addr, Handler: handler}

	go func() {
		logger.Printf("listening on %s", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logger.Printf("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	_ = logWriter.Close()
	if err := st.Flush(); err != nil {
		logger.Printf("flush state: %v", err)
	}
}
