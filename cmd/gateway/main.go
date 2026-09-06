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
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/cache"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/otlp"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/rstate"
	"github.com/erickardus/ai-gateway/internal/server"
	"github.com/erickardus/ai-gateway/internal/spend"
	"github.com/erickardus/ai-gateway/internal/sso"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Subcommands are dispatched before the server's own flags are parsed, so
	// "gateway -config ..." keeps working exactly as it did. The login
	// subcommand ships in this binary rather than a second one because a
	// developer already has to obtain it, and because the settings it writes
	// have to agree with what the gateway serves.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			exit(runLogin(os.Args[2:]))
		case "logout":
			exit(runLogout(os.Args[2:]))
		}
	}
	exit(run())
}

// exit reports an error the way a command-line tool should and stops.
func exit(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
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
	authn, err := auth.NewAuthenticator(ctx, store, cfg.VirtualKeys, cfg.Scopes())
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}
	if cfg.RBAC.Enabled() {
		log.Info("rbac hierarchy loaded", "scopes", len(cfg.Scopes()),
			"organizations", len(cfg.RBAC.Organizations))
	}

	local := router.NewMemState()
	ids := make([]string, 0, len(cfg.ModelList))
	for i := range cfg.ModelList {
		ids = append(ids, cfg.ModelList[i].ID())
	}
	local.Prepare(ids)

	localLedger, err := newSpendLedger(cfg.Observability)
	if err != nil {
		return err
	}

	// Sharing state is what makes several replicas enforce one limit rather
	// than one each. Without Redis the gateway still runs, but every limit and
	// budget is per-process.
	var (
		state  router.StateStore = local
		ledger spend.Store       = localLedger
		shared *rstate.Store
	)
	if cfg.Redis.Enabled() {
		shared = rstate.New(rstate.Options{
			Addr: cfg.Redis.Addr, Username: cfg.Redis.Username,
			Password: cfg.Redis.Password, DB: cfg.Redis.DB,
			KeyPrefix: cfg.Redis.KeyPrefix, Timeout: cfg.Redis.Timeout,
		}, local, log)
		defer shared.Close()

		if err := shared.Ping(ctx); err != nil {
			// Not fatal: the gateway degrades to local state and says so, which
			// is better than refusing to start because a dependency is slow.
			log.Error("redis unreachable at startup; starting with per-instance state",
				"addr", cfg.Redis.Addr, "error", err)
		}
		state = shared
		ledger = rstate.NewLedger(shared, localLedger, cfg.Redis.KeyPrefix, log, cfg.Redis.Timeout)
		// A virtual key's own rpm/tpm allowance is shared for the same reason
		// a deployment's is: counted per process, it is multiplied by the
		// replica count, and a key limited to 60 requests a minute gets 180
		// across three instances.
		authn.UseKeyLimiter(shared.KeyLimiter())
	}

	// A shared cache means a hit on one instance serves them all; a local cache
	// is per-process but needs no dependency.
	var responses cache.Cache
	if cfg.Cache.Enabled {
		local := cache.NewMemory(cfg.Cache.MaxEntries)
		responses = local
		if cfg.Cache.Shared && shared != nil {
			responses = cache.NewRedis(shared.Client(), local, cfg.Redis.KeyPrefix, log, cfg.Redis.Timeout)
		}
	}

	// The registry is built before the router so the router can report an
	// ejection into it. Metrics are enabled by either publisher: an operator who
	// exports over OTLP but does not serve /metrics still needs them collected.
	var reg *metrics.Registry
	if cfg.Observability.Metrics || cfg.Observability.OTLP.Enabled() {
		reg = metrics.New()
	}

	client := newUpstreamClient(cfg)
	rtr, err := router.New(cfg, state, client, log, router.Options{
		Ejected: func(model, deployment string) {
			reg.Add(metrics.MCooldowns, 1, "model", model, "deployment", deployment)
		},
	})
	if err != nil {
		return fmt.Errorf("initialize router: %w", err)
	}
	registerLiveGauges(reg, cfg, rtr, store, shared, version)

	// Push the same snapshot /metrics serves to an OTLP collector. The two are
	// independent: either, both or neither may be on, and both render the same
	// snapshot so they cannot disagree about a number.
	var otlpDone chan struct{}
	if cfg.Observability.OTLP.Enabled() {
		o := cfg.Observability.OTLP
		exporter, err := otlp.New(reg, otlp.Options{
			Endpoint: o.Endpoint,
			Protocol: otlp.Protocol(o.Protocol),
			Headers:  o.Headers,
			Interval: o.Interval,
			Timeout:  o.Timeout,
			Compress: o.CompressEnabled(),
			Resource: otlpResource(o, version),
		}, log)
		if err != nil {
			return fmt.Errorf("initialize otlp exporter: %w", err)
		}
		otlpDone = make(chan struct{})
		go func() {
			defer close(otlpDone)
			exporter.Run(ctx)
		}()
	}

	gw := server.New(cfg, authn, store, rtr, log, ledger, reg, shared, responses)

	// SSO, when an identity provider is configured. Pending logins go to Redis
	// wherever it is available: a login leaves for the provider and comes back
	// as a separate request, which behind a load balancer may reach a different
	// instance than the one that started it.
	if cfg.SSO.Enabled() {
		var pending sso.Store = sso.NewMemStore()
		if shared != nil {
			pending = shared.SSOStore()
		}
		provider := sso.New(cfg.SSO, log)
		gw.UseSSO(provider, pending)
		log.Info("sso enabled", "issuer", cfg.SSO.Issuer, "roles", len(cfg.SSO.Roles),
			"key_duration", cfg.SSO.KeyDuration, "shared_state", shared != nil)

		// The same provider verifies tokens presented on requests. It shares the
		// key set the login path already fetches, so turning this on adds a
		// signature check rather than a second client of the provider.
		if cfg.SSO.JWTAuth.Enabled {
			authn.UseTokenVerifier(provider)
			log.Info("jwt auth enabled",
				"audiences", cfg.SSO.JWTAuth.Audiences, "cache_ttl", cfg.SSO.JWTAuth.CacheTTL)
		}
	}

	srv := gw.HTTPServer()

	// Persist the ledger periodically and once more on the way out, so a
	// restart does not hand every key a fresh budget.
	if cfg.Observability.SpendStorePath != "" {
		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			ticker := time.NewTicker(cfg.Observability.SpendFlushInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := localLedger.Flush(); err != nil {
						log.Warn("flush spend ledger", "error", err)
					}
				}
			}
		}()
		defer func() {
			<-flushDone
			if err := localLedger.Flush(); err != nil {
				log.Error("final spend ledger flush failed", "error", err)
			}
		}()
	}

	log.Info("starting gateway",
		"version", version,
		"addr", cfg.Server.Addr,
		"strategy", cfg.Router.Strategy,
		"model_groups", len(cfg.Groups()),
		"deployments", len(cfg.ModelList),
		"key_management", authn.HasMasterKey(),
		"metrics", cfg.Observability.Metrics,
		"otlp", cfg.Observability.OTLP.Endpoint,
		"spend_store", cfg.Observability.SpendStorePath != "",
		"shared_state", cfg.Redis.Enabled(),
		"cache", cfg.Cache.Enabled,
		"cache_scope", cfg.Cache.Scope,
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
	// The exporter's context is already cancelled; wait for its final flush so
	// the interval since the last push is not lost. It is bounded by the
	// exporter's own timeout, and the shutdown grace has already elapsed by the
	// time anything is waiting here.
	if otlpDone != nil {
		<-otlpDone
	}

	log.Info("gateway stopped")
	return nil
}

// newSpendLedger builds the spend ledger, persisted when a path is configured.
func newSpendLedger(cfg config.ObservabilityConfig) (*spend.Ledger, error) {
	if cfg.SpendStorePath == "" {
		return spend.New(), nil
	}
	ledger, err := spend.NewFileLedger(cfg.SpendStorePath)
	if err != nil {
		return nil, fmt.Errorf("open spend ledger: %w", err)
	}
	return ledger, nil
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
