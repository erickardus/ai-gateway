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

// Savings are what caching did to the bill, in either direction. A negative
// figure is not a bug to clamp away: it is the report that this traffic wrote
// caches it never read, which is what a scattered conversation looks like in
// money and is the one place the gateway says so.
//
// The inverted cost model the old clamp guarded against — a cache read priced at
// or above input — cannot be loaded: validation refuses it, and refusing it
// there is what leaves this arithmetic free to mean what it says.
func TestSavingsAreNetOfTheWritePremium(t *testing.T) {
	price := Pricing{InputPer1M: 3, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75}

	// A turn that read back what an earlier one wrote: the read saves 2.70 and
	// the write it extends cost 0.75 over ordinary input.
	if got := price.CacheSavings(Usage{CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000}); !within(got, 1.95) {
		t.Errorf("CacheSavings = %v, want 1.95", got)
	}
	// The same write with nowhere to read it back is the premium alone.
	if got := price.CacheSavings(Usage{CacheWriteTokens: 1_000_000}); !within(got, -0.75) {
		t.Errorf("CacheSavings = %v, want -0.75: an unread write costs more than not caching", got)
	}
	unpriced := Pricing{}
	if got := unpriced.CacheSavings(Usage{CacheReadTokens: 1_000_000}); got != 0 {
		t.Errorf("CacheSavings = %v, want 0 with no pricing configured", got)
	}
}

// The definition savings are pinned to: what caching took off the bill is the
// difference between the bill and the one the same tokens would have run up as
// ordinary input. Everything else about the figure follows from this.
func TestCostAndSavingsReconstructTheUncachedBill(t *testing.T) {
	price := Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6}
	usage := Usage{InputTokens: 400, OutputTokens: 900, CacheReadTokens: 120_000, CacheWriteTokens: 30_000, CacheWrite1hTokens: 10_000}

	uncached := Usage{
		InputTokens:  usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens,
		OutputTokens: usage.OutputTokens,
	}
	if got, want := price.Cost(usage)+price.CacheSavings(usage), price.Cost(uncached); !within(got, want) {
		t.Errorf("cost plus savings = %v, want the uncached bill %v", got, want)
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

// longContextPrice is Anthropic's shape for the tier: every class of token
// costs more above the threshold, and the threshold is measured against the
// whole prompt rather than against the part that was not cached.
func longContextPrice() Pricing {
	return Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
		CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6,
		LongContext: &LongContextPricing{
			AbovePromptTokens: 200_000,
			InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6,
			CacheWritePer1M: 7.5, CacheWrite1hPer1M: 12,
		},
	}
}

// A long conversation is the one a prompt cache exists for, and it is also the
// one a provider charges most for. Reading it at the small-request rate
// understates every figure the gateway reports about the traffic it was built
// to carry, and does so silently.
func TestALargePromptIsBilledAtItsOwnTier(t *testing.T) {
	price := longContextPrice()

	tests := []struct {
		name  string
		usage Usage
		want  float64
	}{
		{
			name:  "below the threshold, at the base rates",
			usage: Usage{InputTokens: 100_000, OutputTokens: 1_000},
			want:  100_000*3/1e6 + 1_000*15/1e6,
		},
		{
			// The threshold is crossed by the whole prompt, not by the part of
			// it the provider charged as fresh input: a conversation reading
			// 190k tokens out of the cache is a large request, and the provider
			// prices it as one.
			name:  "cached tokens count towards the threshold",
			usage: Usage{InputTokens: 20_000, CacheReadTokens: 190_000, OutputTokens: 1_000},
			want:  20_000*6/1e6 + 190_000*0.6/1e6 + 1_000*22.5/1e6,
		},
		{
			name: "every class of token moves tier together",
			usage: Usage{
				InputTokens: 50_000, CacheReadTokens: 100_000, CacheWriteTokens: 100_000,
				CacheWrite1hTokens: 40_000, OutputTokens: 2_000,
			},
			want: 50_000*6/1e6 + 100_000*0.6/1e6 + 60_000*7.5/1e6 + 40_000*12/1e6 + 2_000*22.5/1e6,
		},
		{
			// Exactly at the threshold is not above it.
			name:  "the threshold is exclusive",
			usage: Usage{InputTokens: 200_000},
			want:  200_000 * 3 / 1e6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := price.Cost(tt.usage); !within(got, tt.want) {
				t.Errorf("Cost = %.6f, want %.6f", got, tt.want)
			}
		})
	}
}

// Savings are what the cache took off the bill, so they have to be measured
// against the input rate the request would actually have paid. Measured against
// the base rate, a long-context conversation reports about half the saving it
// made — and the identity that cost plus savings is the uncached bill stops
// holding exactly where the money is.
func TestSavingsOnALargePromptUseTheTierItWasBilledAt(t *testing.T) {
	price := longContextPrice()
	usage := Usage{InputTokens: 20_000, CacheReadTokens: 190_000, CacheWriteTokens: 10_000, OutputTokens: 500}

	want := 190_000*(6-0.6)/1e6 + 10_000*(6-7.5)/1e6
	if got := price.CacheSavings(usage); !within(got, want) {
		t.Errorf("CacheSavings = %.6f, want %.6f", got, want)
	}

	// The identity the whole report rests on: what was charged plus what was
	// not is what the same tokens would have cost as ordinary input.
	uncached := float64(usage.PromptTokens())*6/1e6 + float64(usage.OutputTokens)*22.5/1e6
	if got := price.Cost(usage) + price.CacheSavings(usage); !within(got, uncached) {
		t.Errorf("cost + savings = %.6f, want the uncached bill %.6f", got, uncached)
	}
}

// A tier that names only some of its rates falls back to the base rate for the
// rest rather than to zero. Configuration refuses that shape at load; this is
// what keeps an older binary reading a newer config from billing a class of
// token at nothing.
func TestAPartialTierFallsBackToTheBaseRate(t *testing.T) {
	price := Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
		LongContext: &LongContextPricing{AbovePromptTokens: 200_000, InputPer1M: 6},
	}
	usage := Usage{InputTokens: 300_000, OutputTokens: 1_000, CacheReadTokens: 1_000}
	want := 300_000*6/1e6 + 1_000*15/1e6 + 1_000*0.3/1e6
	if got := price.Cost(usage); !within(got, want) {
		t.Errorf("Cost = %.6f, want %.6f", got, want)
	}
}

// A cost block holding nothing but a tier is still a cost model: reporting it as
// unpriced would skip the validation that refuses it.
func TestZeroSeesATierAsPricing(t *testing.T) {
	if (Pricing{LongContext: &LongContextPricing{AbovePromptTokens: 200_000}}).Zero() {
		t.Error("a cost block carrying a long-context tier reported itself as unpriced")
	}
}
