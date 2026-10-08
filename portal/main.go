// Command portal serves the pi-pocket runtime configuration portal.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/auth"
	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
	"github.com/cldmnky/pi-pocket8s/portal/internal/server"
)

//go:embed web
var webFiles embed.FS

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("portal failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	// Fail closed before serving anything: without a strong token every API
	// request would have to be denied anyway.
	if _, err := auth.LoadToken(cfg.TokenFile); err != nil {
		return fmt.Errorf("portal token: %w", err)
	}

	kubeClient, err := kube.InClusterClient(10 * time.Second)
	if err != nil {
		return err
	}

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		return fmt.Errorf("embedded SPA: %w", err)
	}

	srv := &http.Server{
		Addr:              config.DefaultListenAddr,
		Handler:           server.New(cfg, kubeClient, logger, static).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("portal listening",
			"addr", srv.Addr,
			"namespace", cfg.Namespace,
			"deployment", cfg.Deployment,
			"secret", cfg.ConfigSecret,
			"origin", cfg.PortalOrigin,
		)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("portal shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
