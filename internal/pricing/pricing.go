// Package pricing turns token counts into money.
//
// The table is data, not code: adding a model is a row in prices.toml
// (AGENT.md: data-driven design). Prices are applied at query time so that
// history can be re-priced without rewriting a single stored fact.
package pricing

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed prices.toml
var pricesTOML []byte

// Standard multipliers applied to a model's input price when it does not
// state its own rate.
const (
	cacheReadMultiplier    = 0.1
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// Model is one row of the price table, in dollars per million tokens.
type Model struct {
	ID           string  `toml:"id"`
	Input        float64 `toml:"input"`
	Output       float64 `toml:"output"`
	CacheRead    float64 `toml:"cache_read"`
	CacheWrite5m float64 `toml:"cache_write_5m"`
	CacheWrite1h float64 `toml:"cache_write_1h"`
}

// Usage is the token counts for one or more requests.
type Usage struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// Table maps model identifiers to prices.
type Table struct {
	models map[string]Model
}

type fileFormat struct {
	Model []Model `toml:"model"`
}

// Load returns the built-in price table.
func Load() (*Table, error) {
	var parsed fileFormat
	if err := toml.Unmarshal(pricesTOML, &parsed); err != nil {
		return nil, fmt.Errorf("pricing: parse price table: %w", err)
	}
	t := &Table{models: make(map[string]Model, len(parsed.Model))}
	for _, m := range parsed.Model {
		if m.CacheRead == 0 {
			m.CacheRead = m.Input * cacheReadMultiplier
		}
		if m.CacheWrite5m == 0 {
			m.CacheWrite5m = m.Input * cacheWrite5mMultiplier
		}
		if m.CacheWrite1h == 0 {
			m.CacheWrite1h = m.Input * cacheWrite1hMultiplier
		}
		t.models[m.ID] = m
	}
	return t, nil
}

// suffixPattern strips the decorations Claude Code appends to a model id, such
// as the "[1m]" context marker and dated snapshot suffixes, so that
// "claude-haiku-4-5-20251001" and "claude-opus-5[1m]" price correctly.
var suffixPattern = regexp.MustCompile(`-\d{8}$`)

// Normalize reduces a reported model identifier to a price-table key.
func Normalize(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = m[:i]
	}
	return suffixPattern.ReplaceAllString(m, "")
}

// Lookup returns the price row for a model, reporting whether one exists.
//
// An unknown model is reported rather than silently priced at zero: a missing
// row must show up as an unpriced bucket, not as free usage.
func (t *Table) Lookup(model string) (Model, bool) {
	m, ok := t.models[Normalize(model)]
	return m, ok
}

// Cost returns the dollar cost of usage under a model's prices.
func (t *Table) Cost(model string, u Usage) (float64, bool) {
	m, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	total := float64(u.Input)*m.Input +
		float64(u.Output)*m.Output +
		float64(u.CacheRead)*m.CacheRead +
		float64(u.CacheWrite5m)*m.CacheWrite5m +
		float64(u.CacheWrite1h)*m.CacheWrite1h
	return total / perMillion, true
}

// UncachedCost returns what the same work would have cost had every cache-read
// token been billed as fresh input. The difference is what caching saved.
func (t *Table) UncachedCost(model string, u Usage) (float64, bool) {
	m, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	inputEquivalent := u.Input + u.CacheRead + u.CacheWrite5m + u.CacheWrite1h
	return (float64(inputEquivalent)*m.Input + float64(u.Output)*m.Output) / perMillion, true
}
