package config

import (
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
)

// A cost model missing its cache prices does not fail at request time. It
// serves traffic, reports a number, and is wrong by the whole of the discount
// prompt caching was supposed to deliver — which is why these are load-time
// errors rather than warnings on a dashboard.
func TestPricingValidation(t *testing.T) {
	tests := []struct {
		name    string
		format  core.Format
		cost    core.Pricing
		wantErr string
	}{
		{
			name:   "a complete anthropic cost model",
			format: core.FormatAnthropic,
			cost:   core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75},
		},
		{
			name:   "a complete anthropic cost model with both write tiers",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
				CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6,
			},
		},
		{
			name:   "an openai cost model, which has no write price to give",
			format: core.FormatOpenAI,
			cost:   core.Pricing{InputPer1M: 1.25, OutputPer1M: 10, CacheReadPer1M: 0.125},
		},
		{
			name:    "input and output only",
			format:  core.FormatAnthropic,
			cost:    core.Pricing{InputPer1M: 3, OutputPer1M: 15},
			wantErr: "cache_read_per_1m: required",
		},
		{
			name:    "openai priced without a cache read rate",
			format:  core.FormatOpenAI,
			cost:    core.Pricing{InputPer1M: 1.25, OutputPer1M: 10},
			wantErr: "cache_read_per_1m: required",
		},
		{
			name:    "no cache write price on an anthropic deployment",
			format:  core.FormatAnthropic,
			cost:    core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3},
			wantErr: "cache_write_per_1m: required",
		},
		{
			name:    "a cache read priced at or above fresh input",
			format:  core.FormatAnthropic,
			cost:    core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 3, CacheWritePer1M: 3.75},
			wantErr: "must be below input_per_1m",
		},
		{
			name:    "a cache write priced at no premium",
			format:  core.FormatAnthropic,
			cost:    core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3},
			wantErr: "must exceed input_per_1m",
		},
		{
			name:   "a one-hour write cheaper than a five-minute one",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
				CacheWritePer1M: 3.75, CacheWrite1hPer1M: 1,
			},
			wantErr: "must be at least cache_write_per_1m",
		},
		{
			name:    "a negative price",
			format:  core.FormatAnthropic,
			cost:    core.Pricing{InputPer1M: -3},
			wantErr: "must be >= 0",
		},
		{
			name:   "no cost model at all, which is a coherent state",
			format: core.FormatAnthropic,
			cost:   core.Pricing{},
		},
		{
			name:   "a complete long-context tier",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
				LongContext: &core.LongContextPricing{
					AbovePromptTokens: 200_000,
					InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
				},
			},
		},
		{
			name:   "a long-context tier with no threshold to trigger it",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
				LongContext: &core.LongContextPricing{
					InputPer1M: 6, OutputPer1M: 22.5, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
				},
			},
			wantErr: "above_prompt_tokens: must be > 0",
		},
		{
			name:   "a long-context tier naming only its input price",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
				LongContext: &core.LongContextPricing{AbovePromptTokens: 200_000, InputPer1M: 6},
			},
			wantErr: "long_context.cache_read_per_1m: required",
		},
		{
			name:   "a long-context tier with no output price against a base that has one",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
				LongContext: &core.LongContextPricing{
					AbovePromptTokens: 200_000,
					InputPer1M:        6, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
				},
			},
			wantErr: "long_context.output_per_1m: required",
		},
		{
			name:   "the two blocks written the wrong way round",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 6, OutputPer1M: 22.5, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
				LongContext: &core.LongContextPricing{
					AbovePromptTokens: 200_000,
					InputPer1M:        3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
				},
			},
			wantErr: "must be at least",
		},
		{
			name:   "a long-context tier with no base rates to override",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				LongContext: &core.LongContextPricing{
					AbovePromptTokens: 200_000,
					InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
				},
			},
			wantErr: "requires",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := pricedConfig(tt.format, tt.cost)
			err := Finalize(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Finalize: %v", err)
			case tt.wantErr == "":
				return
			case err == nil:
				t.Fatalf("want an error containing %q, got none", tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// A passthrough deployment carries no cost at all, so the pricing rules must not
// reach it: the caller's own subscription is billed, and any price here would
// invent a charge nobody receives.
func TestPricingRulesDoNotApplyToPassthrough(t *testing.T) {
	cfg := pricedConfig(core.FormatAnthropic, core.Pricing{})
	cfg.ModelList[0].Params.AuthMode = core.AuthModePassthrough
	cfg.ModelList[0].Params.APIKey = ""
	cfg.ModelList[0].Params.AuthHeader = ""
	cfg.VirtualKeys.AllowedUpstreamHosts = []string{"api.anthropic.com"}
	if err := Finalize(cfg); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

func pricedConfig(format core.Format, cost core.Pricing) *Config {
	return &Config{
		ModelList: []Deployment{{
			ModelName: "group",
			Params: DeploymentParams{
				Format:     format,
				APIBase:    "https://api.anthropic.com",
				Model:      "a-model",
				AuthMode:   core.AuthModeAPIKey,
				AuthHeader: "x-api-key",
				APIKey:     "sk-test",
			},
			Cost: cost,
		}},
	}
}

// An Anthropic-compatible endpoint in front of another model need not price
// like Anthropic. Moonshot's charges nothing to write its prompt cache, and
// before cache_writes_free existed such a deployment could not be priced at all:
// every write price an operator could write there would invent a premium the
// invoice never shows, so the deployment was left unpriced and reported real
// billable traffic at zero.
func TestFreeCacheWritesOnAnAnthropicCompatibleUpstream(t *testing.T) {
	tests := []struct {
		name    string
		format  core.Format
		cost    core.Pricing
		wantErr string
	}{
		{
			name:   "anthropic format, writes declared free",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
				CacheWritesFree: true,
			},
		},
		{
			name:   "openai format, writes declared free",
			format: core.FormatOpenAI,
			cost: core.Pricing{
				InputPer1M: 1.25, OutputPer1M: 10, CacheReadPer1M: 0.125,
				CacheWritesFree: true,
			},
		},
		{
			name:   "free writes alongside a long-context tier, which inherits the flag",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
				CacheWritesFree: true,
				LongContext: &core.LongContextPricing{
					AbovePromptTokens: 200000,
					InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6,
				},
			},
		},
		{
			name:   "free writes and a write price contradict each other",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
				CacheWritePer1M: 3.75, CacheWritesFree: true,
			},
			wantErr: "must not be set alongside cache_writes_free",
		},
		{
			name:    "the flag alone prices nothing",
			format:  core.FormatAnthropic,
			cost:    core.Pricing{CacheWritesFree: true},
			wantErr: "cache_writes_free: requires",
		},
		{
			name:   "a long-context tier may not reintroduce a write charge",
			format: core.FormatAnthropic,
			cost: core.Pricing{
				InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
				CacheWritesFree: true,
				LongContext: &core.LongContextPricing{
					AbovePromptTokens: 200000,
					InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6,
					CacheWritePer1M: 7.5,
				},
			},
			wantErr: "must not be set alongside cache_writes_free",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Finalize(pricedConfig(tt.format, tt.cost))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Finalize: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Finalize succeeded, want error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Finalize error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
