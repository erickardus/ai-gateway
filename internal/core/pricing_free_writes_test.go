package core

import "testing"

// The flag has to change what a write costs, not merely what validation
// permits. Without it an unset write price falls back to the input rate, so a
// provider that charges nothing would be billed as if every cache write were
// fresh input — the same overstatement in the other direction.
func TestFreeCacheWritesAreChargedAtZero(t *testing.T) {
	usage := Usage{InputTokens: 1_000_000, OutputTokens: 0, CacheWriteTokens: 1_000_000}

	free := Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritesFree: true}
	if got, want := free.Cost(usage), 3.0; got != want {
		t.Errorf("cost with free writes = %v, want %v (input only)", got, want)
	}

	// The same deployment without the flag: the write falls back to the input
	// rate, which is the behaviour the flag exists to correct.
	unset := Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3}
	if got, want := unset.Cost(usage), 6.0; got != want {
		t.Errorf("cost with an unset write price = %v, want %v (write billed as input)", got, want)
	}
}

// A long-context tier is an override of the base rates, and a provider that
// charges nothing to write does not start charging above a prompt-size
// threshold.
func TestFreeCacheWritesSurviveTheLongContextTier(t *testing.T) {
	p := Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritesFree: true,
		LongContext: &LongContextPricing{
			AbovePromptTokens: 200_000,
			InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6,
		},
	}
	usage := Usage{InputTokens: 300_000, CacheWriteTokens: 1_000_000}
	if got, want := p.Cost(usage), 6.0*0.3; got != want {
		t.Errorf("long-context cost = %v, want %v (tier input only, writes still free)", got, want)
	}
}

// Pricing.Zero decides whether a deployment is priced at all. A cost block
// carrying only the flag prices nothing, but it must not read as "no cost block
// was written" either, or validation would never see it to reject it.
func TestFreeCacheWritesFlagIsNotAnEmptyCostBlock(t *testing.T) {
	if (Pricing{CacheWritesFree: true}).Zero() {
		t.Error("a cost block declaring free writes reported itself as absent")
	}
	if !(Pricing{}).Zero() {
		t.Error("an empty cost block did not report itself as absent")
	}
}
