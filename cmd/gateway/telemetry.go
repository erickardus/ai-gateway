package main

import (
	"context"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/otlp"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/rstate"
)

// gaugeReadTimeout bounds one live gauge read. A gauge that consults shared
// state must not be able to hang a scrape: a Prometheus server whose target
// stops answering marks it down, which would turn a slow Redis into an
// apparently dead gateway.
const gaugeReadTimeout = 2 * time.Second

// registerLiveGauges publishes the state other components already own.
//
// These are read at collection time rather than accumulated as events, because
// each of them has an owner that already tracks it: the router knows which
// deployments are cooling, the key store knows how many keys exist. Mirroring
// them into counters here would create a second copy to keep in step, and the
// copy would be wrong exactly when something updated the original by a path
// nobody remembered to instrument.
func registerLiveGauges(reg *metrics.Registry, cfg *config.Config, rtr *router.Router, store auth.KeyStore, shared *rstate.Store, version string) {
	if reg == nil {
		return
	}

	// Build information as a gauge pinned at 1, which is the established way to
	// get a version string onto a dashboard: the value carries nothing, the
	// labels are the point, and a change in them is a deploy.
	reg.RegisterGauge(metrics.MBuildInfo,
		"Always 1. The labels carry the build: a change in them is a deploy.", "{build}",
		func() []metrics.Point {
			return []metrics.Point{{
				Labels: []metrics.Label{
					{Key: "version", Value: version},
					{Key: "strategy", Value: rtr.Strategy()},
				},
				Value: 1,
			}}
		})

	// Whether each deployment is currently taking traffic. A gauge rather than
	// only the ejection counter, because a counter says an ejection happened
	// and a gauge says the outage is still going on.
	reg.RegisterGauge(metrics.MDeploymentAvailable,
		"1 when a deployment is taking traffic, 0 while it is cooling down after failures.", "{deployment}",
		func() []metrics.Point {
			ctx, cancel := context.WithTimeout(context.Background(), gaugeReadTimeout)
			defer cancel()
			out := make([]metrics.Point, 0, len(cfg.ModelList))
			for i := range cfg.ModelList {
				d := &cfg.ModelList[i]
				cooling, _ := rtr.DeploymentStatus(ctx, d.ID())
				available := 1.0
				if cooling {
					available = 0
				}
				out = append(out, metrics.Point{
					Labels: []metrics.Label{
						{Key: "model", Value: d.ModelName},
						{Key: "deployment", Value: d.ID()},
					},
					Value: available,
				})
			}
			return out
		})

	if store != nil {
		reg.RegisterGauge(metrics.MVirtualKeys,
			"Virtual keys currently issued. Keys are never labelled individually: a runtime-minted key is unbounded in number, and one series per key would be an unbounded metric.", "{key}",
			func() []metrics.Point {
				ctx, cancel := context.WithTimeout(context.Background(), gaugeReadTimeout)
				defer cancel()
				keys, err := store.List(ctx)
				if err != nil {
					return nil
				}
				return []metrics.Point{{Value: float64(len(keys))}}
			})
	}

	if shared != nil {
		// Reported as a gauge of the running total rather than a counter,
		// because the count belongs to the shared-state store and is read from
		// it rather than incremented here. Its rate of change is still what
		// matters, and rate() over a monotonic gauge is well defined.
		reg.RegisterGauge(metrics.MSharedStateDegraded,
			"Times this instance fell back to per-process limits because shared state was unreachable. Every limit is silently multiplied by the replica count while that is happening.", "{event}",
			func() []metrics.Point {
				return []metrics.Point{{Value: float64(shared.Degradations())}}
			})
	}
}

// otlpResource builds the resource attributes that identify this process.
//
// service.name is the one almost every OTLP backend groups by, so it is always
// present. The rest come from configuration, and anything an operator sets wins
// over these defaults — a fleet that already names its services a particular
// way should not have this gateway insist otherwise.
func otlpResource(cfg config.OTLPConfig, version string) otlp.Resource {
	attrs := []metrics.Label{
		{Key: "service.name", Value: cfg.ServiceName},
		{Key: "service.version", Value: version},
	}
	seen := map[string]bool{"service.name": true, "service.version": true}
	for _, k := range sortedKeys(cfg.ResourceAttributes) {
		if seen[k] {
			// Replace rather than duplicate: two attributes with one key is
			// undefined in OTLP, and backends differ on which they keep.
			for i := range attrs {
				if attrs[i].Key == k {
					attrs[i].Value = cfg.ResourceAttributes[k]
				}
			}
			continue
		}
		attrs = append(attrs, metrics.Label{Key: k, Value: cfg.ResourceAttributes[k]})
		seen[k] = true
	}
	return otlp.Resource{Attributes: attrs}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
