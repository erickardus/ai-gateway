// Command gateway runs the AI gateway: an OpenAI- and Anthropic-compatible
// proxy that load balances across upstream deployments, authenticates callers
// with virtual keys, and can relay a caller's own provider credential so that a
// Claude.ai subscription login keeps working through the gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/server"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "config/gateway.yaml", "path to the gateway configuration file")
		addr        = flag.String("addr", "", "override server.addr from the configuration")
		logLevel    = flag.String("log-level", "", "override observability.log_level (debug, info, warn, error)")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("ai-gateway", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	if *logLevel != "" {
		cfg.Observability.LogLevel = *logLevel
	}

	log := newLogger(cfg.Observability)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := newKeyStore(cfg.VirtualKeys)
	if err != nil {
		return err
	}
	authn, err := auth.NewAuthenticator(ctx, store, cfg.VirtualKeys)
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}

	state := router.NewMemState()
	ids := make([]string, 0, len(cfg.ModelList))
	for i := range cfg.ModelList {
		ids = append(ids, cfg.ModelList[i].ID())
	}
	state.Prepare(ids)

	client := newUpstreamClient(cfg)
	rtr, err := router.New(cfg, state, client, log, router.Options{})
	if err != nil {
		return fmt.Errorf("initialize router: %w", err)
	}

	srv := server.New(cfg, authn, store, rtr, log).HTTPServer()

	log.Info("starting gateway",
		"version", version,
		"addr", cfg.Server.Addr,
		"strategy", cfg.Router.Strategy,
		"model_groups", len(cfg.Groups()),
		"deployments", len(cfg.ModelList),
		"key_management", authn.HasMasterKey(),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen on %s: %w", cfg.Server.Addr, err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining", "grace", cfg.Server.ShutdownGrace)
	}

	// Stop listening and let in-flight requests finish. Streaming responses can
	// legitimately still be running, so the grace period is generous.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out; closing remaining connections", "error", err)
		if closeErr := srv.Close(); closeErr != nil {
			return fmt.Errorf("close server: %w", closeErr)
		}
	}
	log.Info("gateway stopped")
	return nil
}

// newKeyStore builds the configured key store.
func newKeyStore(cfg config.VirtualKeysConfig) (auth.KeyStore, error) {
	switch cfg.Store.Kind {
	case "file":
		store, err := auth.NewFileStore(cfg.Store.Path)
		if err != nil {
			return nil, fmt.Errorf("open key store: %w", err)
		}
		return store, nil
	default:
		return auth.NewMemStore(), nil
	}
}

// newUpstreamClient builds the HTTP client used for every upstream call.
func newUpstreamClient(cfg *config.Config) *provider.Client {
	return provider.NewClient(cfg.VirtualKeys.HeaderNames, cfg.VirtualKeys.AllowedUpstreamHosts)
}

// newLogger builds the process logger. Credentials are never logged, so no
// redacting handler is needed on top of this.
func newLogger(cfg config.ObservabilityConfig) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
