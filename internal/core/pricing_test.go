package core

import "testing"

// TestCostSplitsTheTwoWriteTiers pins the arithmetic that separates a
// five-minute cache write from a one-hour one. The long tier is a subset of the
// total rather than an addition to it, so a request reporting both must not be
// charged for its long writes twice.
func TestCostSplitsTheTwoWriteTiers(t *testing.T) {
	price := Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6}

	tests := []struct {
		name  string
		usage Usage
		want  float64
	}{
		{
			name:  "all five-minute",
			usage: Usage{CacheWriteTokens: 1_000_000},
			want:  3.75,
		},
		{
			name:  "all one-hour",
			usage: Usage{CacheWriteTokens: 1_000_000, CacheWrite1hTokens: 1_000_000},
			want:  6,
		},
		{
			name:  "a mix, charged at each tier for its own share",
			usage: Usage{CacheWriteTokens: 1_000_000, CacheWrite1hTokens: 400_000},
			want:  0.6*3.75 + 0.4*6,
		},
		{
			name:  "a subset larger than its total is clamped, not credited",
			usage: Usage{CacheWriteTokens: 1_000_000, CacheWrite1hTokens: 5_000_000},
			want:  6,
		},
		{
			name:  "a negative subset does not discount the total",
			usage: Usage{CacheWriteTokens: 1_000_000, CacheWrite1hTokens: -400_000},
			want:  3.75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := price.Cost(tt.usage); !within(got, tt.want) {
				t.Errorf("Cost = %v, want %v", got, tt.want)
			}
		})
	}
}

// An unpriced long tier falls back to the five-minute rate rather than to zero.
// Falling back to zero would make the most expensive tokens a provider sells
// look free, which is the opposite of the safe direction to be wrong in.
func TestUnpricedOneHourTierFallsBackToTheWriteRate(t *testing.T) {
	price := Pricing{InputPer1M: 3, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75}
	usage := Usage{CacheWriteTokens: 1_000_000, CacheWrite1hTokens: 1_000_000}
	if got := price.Cost(usage); !within(got, 3.75) {
		t.Errorf("Cost = %v, want the five-minute rate 3.75", got)
	}
}

// The one-hour count is a subset of the write total, so counting it in the
// per-request token tally would charge a caller's rate limit twice for the same
// tokens.
func TestTotalExcludesTheLongTierSubset(t *testing.T) {
	usage := Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, CacheWrite1hTokens: 4}
	if got := usage.Total(); got != 10 {
		t.Errorf("Total = %d, want 10", got)
	}
}

// Savings are what caching took off the bill. An unpriced or inverted cost model
// must report nothing rather than a negative saving, which would read as the
// cache having cost money.
func TestSavingsNeverGoNegative(t *testing.T) {
	inverted := Pricing{InputPer1M: 0.3, CacheReadPer1M: 3}
	if got := inverted.CacheSavings(Usage{CacheReadTokens: 1_000_000}); got != 0 {
		t.Errorf("CacheSavings = %v, want 0 when a cache read is priced above input", got)
	}
	unpriced := Pricing{}
	if got := unpriced.CacheSavings(Usage{CacheReadTokens: 1_000_000}); got != 0 {
		t.Errorf("CacheSavings = %v, want 0 with no pricing configured", got)
	}
}

// Zero must account for every field, or a cost model carrying only the newest
// price would be treated as absent — and silently exempted from the validation
// that makes a partial cost model an error.
func TestZeroCoversEveryPrice(t *testing.T) {
	for name, price := range map[string]Pricing{
		"input":    {InputPer1M: 1},
		"output":   {OutputPer1M: 1},
		"read":     {CacheReadPer1M: 1},
		"write":    {CacheWritePer1M: 1},
		"write 1h": {CacheWrite1hPer1M: 1},
	} {
		if price.Zero() {
			t.Errorf("a cost model carrying only %s reports itself as unpriced", name)
		}
	}
	if !(Pricing{}).Zero() {
		t.Error("an empty cost model must report itself as unpriced")
	}
}

func within(a, b float64) bool {
	const epsilon = 1e-9
	d := a - b
	return d < epsilon && d > -epsilon
}
