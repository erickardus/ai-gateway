package router

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// Strategy picks one deployment from a set already filtered for health and
// capacity. Implementations must tolerate an empty candidate list.
type Strategy interface {
	// Name returns the configuration name of the strategy.
	Name() string
	// Pick chooses a deployment, or returns ErrNoHealthyDeployment.
	Pick(ctx context.Context, candidates []*config.Deployment, st StateStore, rnd *rand.Rand) (*config.Deployment, error)
}

// NewStrategy returns the strategy named in configuration.
func NewStrategy(cfg config.RouterConfig) (Strategy, error) {
	switch cfg.Strategy {
	case config.StrategyWeightedShuffle:
		return WeightedShuffle{}, nil
	case config.StrategyLeastBusy:
		return LeastBusy{}, nil
	case config.StrategyUsageBased:
		return UsageBased{}, nil
	case config.StrategyLatencyBased:
		return LatencyBased{Buffer: cfg.LowestLatencyBuffer}, nil
	default:
		return nil, fmt.Errorf("unknown routing strategy %q", cfg.Strategy)
	}
}

// WeightedShuffle picks randomly with probability proportional to each
// deployment's weight.
//
// Weight is the only knob it reads. LiteLLM also treats rpm and tpm as weights,
// choosing between the three by inspecting whichever deployment happens to sit
// first in the candidate list, which makes its selection depend on list order.
// Here rpm and tpm are enforced limits instead, and weight alone decides share.
type WeightedShuffle struct{}

// Name implements Strategy.
func (WeightedShuffle) Name() string { return config.StrategyWeightedShuffle }

// Pick implements Strategy.
func (WeightedShuffle) Pick(_ context.Context, candidates []*config.Deployment, _ StateStore, rnd *rand.Rand) (*config.Deployment, error) {
	if len(candidates) == 0 {
		return nil, core.ErrNoHealthyDeployment
	}

	total := 0
	for _, d := range candidates {
		if w := d.Share(); w > 0 {
			total += w
		}
	}
	if total <= 0 {
		// Every candidate has zero weight: fall back to a uniform choice rather
		// than refusing to route.
		return candidates[rnd.IntN(len(candidates))], nil
	}

	target := rnd.IntN(total)
	for _, d := range candidates {
		w := d.Share()
		if w <= 0 {
			continue
		}
		target -= w
		if target < 0 {
			return d, nil
		}
	}
	return candidates[len(candidates)-1], nil
}

// LeastBusy picks the deployment with the fewest outstanding requests.
type LeastBusy struct{}

// Name implements Strategy.
func (LeastBusy) Name() string { return config.StrategyLeastBusy }

// Pick implements Strategy.
func (LeastBusy) Pick(ctx context.Context, candidates []*config.Deployment, st StateStore, rnd *rand.Rand) (*config.Deployment, error) {
	if len(candidates) == 0 {
		return nil, core.ErrNoHealthyDeployment
	}
	best := make([]*config.Deployment, 0, len(candidates))
	lowest := -1
	for _, d := range candidates {
		n, err := st.InFlight(ctx, d.ID())
		if err != nil {
			return nil, fmt.Errorf("read in-flight count: %w", err)
		}
		switch {
		case lowest < 0 || n < lowest:
			lowest, best = n, append(best[:0], d)
		case n == lowest:
			best = append(best, d)
		}
	}
	return best[rnd.IntN(len(best))], nil
}

// UsageBased picks the deployment that has consumed the fewest tokens in the
// current window, spreading load by actual cost rather than request count.
type UsageBased struct{}

// Name implements Strategy.
func (UsageBased) Name() string { return config.StrategyUsageBased }

// Pick implements Strategy.
func (UsageBased) Pick(ctx context.Context, candidates []*config.Deployment, st StateStore, rnd *rand.Rand) (*config.Deployment, error) {
	if len(candidates) == 0 {
		return nil, core.ErrNoHealthyDeployment
	}
	best := make([]*config.Deployment, 0, len(candidates))
	lowest := -1
	for _, d := range candidates {
		tokens, err := st.TokensUsed(ctx, d.ID())
		if err != nil {
			return nil, fmt.Errorf("read token usage: %w", err)
		}
		switch {
		case lowest < 0 || tokens < lowest:
			lowest, best = tokens, append(best[:0], d)
		case tokens == lowest:
			best = append(best, d)
		}
	}
	return best[rnd.IntN(len(best))], nil
}

// LatencyBased picks among the fastest deployments by mean recent latency.
//
// Buffer widens the candidate set to everything within that fraction of the best
// observed latency, so near-equal deployments still share traffic instead of the
// fastest taking everything. Deployments with no samples yet are treated as
// fastest, which guarantees a new or recovered deployment gets tried.
type LatencyBased struct {
	Buffer float64
}

// Name implements Strategy.
func (LatencyBased) Name() string { return config.StrategyLatencyBased }

// Pick implements Strategy.
func (l LatencyBased) Pick(ctx context.Context, candidates []*config.Deployment, st StateStore, rnd *rand.Rand) (*config.Deployment, error) {
	if len(candidates) == 0 {
		return nil, core.ErrNoHealthyDeployment
	}

	type scored struct {
		dep     *config.Deployment
		latency time.Duration
	}
	all := make([]scored, 0, len(candidates))
	lowest := time.Duration(-1)

	unseen := make([]*config.Deployment, 0, len(candidates))
	for _, d := range candidates {
		mean, seen, err := st.MeanLatency(ctx, d.ID())
		if err != nil {
			return nil, fmt.Errorf("read latency: %w", err)
		}
		if !seen {
			// No successful sample yet. Such a deployment is handled
			// separately rather than scored as infinitely fast: a deployment
			// that has only ever failed never records latency, and treating it
			// as zero would collapse the candidate band onto the one upstream
			// known to be broken.
			unseen = append(unseen, d)
			continue
		}
		all = append(all, scored{d, mean})
		if lowest < 0 || mean < lowest {
			lowest = mean
		}
	}

	// Give an untried deployment a turn, but never to the exclusion of
	// deployments with a proven record.
	if len(all) == 0 {
		return unseen[rnd.IntN(len(unseen))], nil
	}
	if len(unseen) > 0 && rnd.IntN(len(candidates)) < len(unseen) {
		return unseen[rnd.IntN(len(unseen))], nil
	}

	limit := lowest + time.Duration(float64(lowest)*l.Buffer)
	within := make([]*config.Deployment, 0, len(all))
	for _, s := range all {
		if s.latency <= limit {
			within = append(within, s.dep)
		}
	}
	if len(within) == 0 {
		return all[rnd.IntN(len(all))].dep, nil
	}
	return within[rnd.IntN(len(within))], nil
}
