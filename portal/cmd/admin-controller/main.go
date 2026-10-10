// Command admin-controller reconciles the cluster-admin elevation session: it
// applies the bounded grant, starts the predefined admin workspace, and removes
// both when the session expires or an operator revokes it. It runs in the
// management namespace, exposes no command API, and holds no browser-facing
// surface.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/admin"
	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("admin controller failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.ControllerFromEnv()
	if err != nil {
		return err
	}
	client, err := kube.InClusterClient(15 * time.Second)
	if err != nil {
		return err
	}
	store := &admin.SecretStore{Client: client, Namespace: cfg.PortalNamespace, Name: cfg.SessionSecret}
	reconciler := admin.NewReconciler(admin.Config{
		Namespace:          cfg.Namespace,
		Deployment:         cfg.Deployment,
		ServiceAccount:     cfg.ServiceAccount,
		RuntimeSecret:      cfg.RuntimeSecret,
		ClusterRoleBinding: cfg.ClusterRoleBinding,
		ClusterRole:        cfg.ClusterRole,
		StartupTimeout:     cfg.StartupTimeout,
	}, store, client, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The privileged delegation is bootstrapped by the installer, not by this
	// process: it verifies the predefined objects and refuses to run otherwise.
	if cfg.Bootstrap {
		if err := reconciler.VerifyTargets(ctx); err != nil {
			return fmt.Errorf("admin elevation bootstrap verification: %w", err)
		}
		logger.Info("admin elevation targets verified",
			"binding", cfg.ClusterRoleBinding, "namespace", cfg.Namespace, "deployment", cfg.Deployment)
	}

	var healthy atomic.Bool
	healthy.Store(true)
	health := &http.Server{
		Addr:              cfg.HealthAddr,
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			if !healthy.Load() {
				http.Error(w, "unhealthy", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok\n"))
		}),
	}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin controller health server stopped", "error", err)
			stop()
		}
	}()

	logger.Info("admin controller listening",
		"health", cfg.HealthAddr,
		"namespace", cfg.Namespace,
		"deployment", cfg.Deployment,
		"sessionSecret", cfg.SessionSecret,
		"reconcile", cfg.Reconcile.String(),
	)

	ticker := time.NewTicker(cfg.Reconcile)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = health.Shutdown(shutdownCtx)
			logger.Info("admin controller shutting down")
			return nil
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := reconciler.Reconcile(passCtx)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				// A failed pass is retried on the next tick; the durable record
				// keeps the session from being reported as healthy meanwhile.
				logger.Error("admin controller reconcile pass failed", "error", err)
			}
		}
	}
}
