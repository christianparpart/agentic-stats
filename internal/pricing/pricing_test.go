package pricing_test

import (
	"math"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/pricing"
)

func TestNormalizeStripsDecorations(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// Claude Code reports the short alias, but cost-state and the config
		// file use fuller forms; all must price identically.
		{"claude-opus-5", "claude-opus-5"},
		{"claude-opus-5[1m]", "claude-opus-5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"  claude-sonnet-5  ", "claude-sonnet-5"},
	}
	for _, tc := range tests {
		if got := pricing.Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCostUsesPublishedRates(t *testing.T) {
	table, err := pricing.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// One million of each, so the result is the per-million rate itself.
	const million = 1_000_000
	cost, ok := table.Cost("claude-opus-5", pricing.Usage{Input: million, Output: million})
	if !ok {
		t.Fatal("claude-opus-5 must be priced")
	}
	if want := 5.0 + 25.0; math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", cost, want)
	}

	// Cache reads are a tenth of input, writes 1.25x and 2x.
	cached, _ := table.Cost("claude-opus-5", pricing.Usage{
		CacheRead: million, CacheWrite5m: million, CacheWrite1h: million,
	})
	if want := 0.5 + 6.25 + 10.0; math.Abs(cached-want) > 1e-9 {
		t.Errorf("cached cost = %v, want %v", cached, want)
	}
}

// Fable 5.1 departs from the usual cache-read multiplier; the table must say so.
func TestFableCacheReadRateIsExplicit(t *testing.T) {
	table, err := pricing.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, ok := table.Lookup("claude-fable-5-1")
	if !ok {
		t.Fatal("claude-fable-5-1 must be priced")
	}
	if m.CacheRead != 0.25 {
		t.Errorf("cache read = %v, want 0.25 (a quarter of the usual 0.1x)", m.CacheRead)
	}
}

// An unknown model must be reported, never silently priced at zero.
func TestUnknownModelIsReportedNotFree(t *testing.T) {
	table, err := pricing.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := table.Lookup("claude-from-the-future-9"); ok {
		t.Error("an unknown model must not resolve")
	}
	cost, ok := table.Cost("claude-from-the-future-9", pricing.Usage{Output: 1_000_000})
	if ok {
		t.Error("Cost must report that the model is unpriced")
	}
	if cost != 0 {
		t.Errorf("unpriced cost = %v, want 0 alongside ok=false", cost)
	}
}

// The cache-savings figure is the whole argument for prompt caching, so the
// uncached comparison must bill every cached token as fresh input.
func TestUncachedCostBillsCacheTokensAsInput(t *testing.T) {
	table, err := pricing.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const million = 1_000_000
	u := pricing.Usage{Input: million, CacheRead: 9 * million, Output: million}

	actual, _ := table.Cost("claude-opus-5", u)
	uncached, _ := table.UncachedCost("claude-opus-5", u)

	// 10M input-equivalent at $5 plus 1M output at $25.
	if want := 10*5.0 + 25.0; math.Abs(uncached-want) > 1e-9 {
		t.Errorf("uncached = %v, want %v", uncached, want)
	}
	if uncached <= actual {
		t.Errorf("caching must be cheaper: actual %v, uncached %v", actual, uncached)
	}
}
