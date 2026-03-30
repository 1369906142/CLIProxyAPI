package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/gateway"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisstate"
	log "github.com/sirupsen/logrus"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "gateway.example.yaml", "Path to gateway config file")
	flag.Parse()

	logging.SetupBaseLogger()

	cfg, err := gateway.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("load gateway config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := redisstate.New(ctx, cfg.Redis)
	if err != nil {
		log.Fatalf("init redis store: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			log.WithError(errClose).Warn("close redis store")
		}
	}()

	srv, err := gateway.NewServer(cfg, store)
	if err != nil {
		log.Fatalf("init gateway server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Infof("gateway listening on %s", cfg.Listen)
		if errServe := httpServer.ListenAndServe(); errServe != nil && errServe != http.ErrServerClosed {
			serverErr <- errServe
			return
		}
		serverErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		if errShutdown := httpServer.Shutdown(shutdownCtx); errShutdown != nil {
			log.Fatalf("shutdown gateway: %v", errShutdown)
		}
	case errServe := <-serverErr:
		if errServe != nil {
			log.Fatalf("gateway serve failed: %v", errServe)
		}
	}

	fmt.Println("gateway stopped")
}
