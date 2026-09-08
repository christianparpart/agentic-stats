// Package derive interprets archived raw lines.
//
// This is the only package that knows what a transcript line means, and the
// only place the requestId fold happens.
//
// That fold is the difference between right and wrong numbers rather than a
// refinement. Claude Code writes one JSONL line per content block, and every
// one of those lines repeats the complete usage object for the whole API
// response. Summing them naively over-counts output tokens by roughly 2.9x and
// thinking tokens by 4.2x, and the factor differs per metric, so it cannot be
// corrected afterwards with a constant.
package derive

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// ModelUsage is one model's folded token totals.
type ModelUsage struct {
	Model    string        `json:"model"`
	Requests int64         `json:"requests"`
	Usage    pricing.Usage `json:"-"`
	Thinking int64         `json:"thinking_tokens"`

	// Reported separately so the API can expose the tokens without callers
	// reaching into an internal type.
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	CacheReadTokens    int64 `json:"cache_read_tokens"`
	CacheWrite5mTokens int64 `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens"`

	// CostUSD is the API-equivalent cost, and Priced reports whether the model
	// was in the price table. An unknown model is surfaced, never silently free.
	CostUSD float64 `json:"cost_usd"`
	Priced  bool    `json:"priced"`

	// UncachedCostUSD is what the same work would have cost with no prompt
	// cache at all. The gap is what caching saved.
	UncachedCostUSD float64 `json:"uncached_cost_usd"`
}

// Summary is the whole-archive picture for one tenant.
type Summary struct {
	// Lines is every archived line, of any kind.
	Lines int64 `json:"lines"`
	// AssistantLines is how many lines carried a usage object.
	AssistantLines int64 `json:"assistant_lines"`
	// Requests is how many distinct API requests those lines represent.
	Requests int64 `json:"requests"`
	// Inflation is AssistantLines / Requests: how much a naive sum would have
	// over-counted. Reported because it is the clearest evidence the fold is
	// doing its job.
	Inflation float64 `json:"inflation"`

	Models []ModelUsage `json:"models"`

	TotalCostUSD         float64 `json:"total_cost_usd"`
	TotalUncachedCostUSD float64 `json:"total_uncached_cost_usd"`
	CacheSavingsUSD      float64 `json:"cache_savings_usd"`
	CacheHitRate         float64 `json:"cache_hit_rate"`

	// UnpricedModels names models absent from the price table.
	UnpricedModels []string `json:"unpriced_models,omitempty"`
}

// Day is one day's activity, in UTC.
type Day struct {
	Date         string  `json:"date"`
	Requests     int64   `json:"requests"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// Service derives facts from the archive.
type Service struct {
	db     *store.DB
	prices *pricing.Table
}

// NewService returns a Service backed by db and a price table.
func NewService(db *store.DB, prices *pricing.Table) (*Service, error) {
	if db == nil {
		return nil, errors.New("derive: a database is required")
	}
	if prices == nil {
		return nil, errors.New("derive: a price table is required")
	}
	return &Service{db: db, prices: prices}, nil
}

// foldedUsageCTE selects one row per API request.
//
// DISTINCT ON (request_id) is the fold: every line of a multi-block response
// carries an identical usage object, so any one of them is the whole truth and
// the rest are copies. Synthetic and error lines carry a null requestId and
// all-zero usage, and are excluded rather than counted as free requests.
const foldedUsageCTE = `
WITH parsed AS (
    SELECT try_jsonb(raw) AS b FROM raw_lines
),
assistant AS (
    SELECT
        b->>'requestId'                                                  AS request_id,
        COALESCE(b->'message'->>'model', 'unknown')                      AS model,
        left(b->>'timestamp', 10)                                        AS day,
        COALESCE((b->'message'->'usage'->>'input_tokens')::bigint, 0)    AS input_tokens,
        COALESCE((b->'message'->'usage'->>'output_tokens')::bigint, 0)   AS output_tokens,
        COALESCE((b->'message'->'usage'->>'cache_read_input_tokens')::bigint, 0)
                                                                         AS cache_read_tokens,
        COALESCE((b->'message'->'usage'->'cache_creation'->>'ephemeral_5m_input_tokens')::bigint, 0)
                                                                         AS cache_write_5m_tokens,
        COALESCE((b->'message'->'usage'->'cache_creation'->>'ephemeral_1h_input_tokens')::bigint, 0)
                                                                         AS cache_write_1h_tokens,
        COALESCE((b->'message'->'usage'->'output_tokens_details'->>'thinking_tokens')::bigint, 0)
                                                                         AS thinking_tokens
    FROM parsed
    WHERE b IS NOT NULL
      AND b->>'type' = 'assistant'
      AND b->>'requestId' IS NOT NULL
      AND COALESCE(b->'message'->>'model', '') <> '<synthetic>'
      AND b->'message'->'usage' IS NOT NULL
),
folded AS (
    SELECT DISTINCT ON (request_id) * FROM assistant ORDER BY request_id
)`

// Summarize computes the whole-archive summary for one tenant.
func (s *Service) Summarize(ctx context.Context, userID string) (Summary, error) {
	var sum Summary

	err := s.db.InTenantTx(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM raw_lines`).Scan(&sum.Lines); err != nil {
			return fmt.Errorf("derive: count lines: %w", err)
		}
		if err := tx.QueryRow(ctx,
			foldedUsageCTE+` SELECT count(*) FROM assistant`).Scan(&sum.AssistantLines); err != nil {
			return fmt.Errorf("derive: count assistant lines: %w", err)
		}

		rows, err := tx.Query(ctx, foldedUsageCTE+`
			SELECT model,
			       count(*),
			       sum(input_tokens),
			       sum(output_tokens),
			       sum(cache_read_tokens),
			       sum(cache_write_5m_tokens),
			       sum(cache_write_1h_tokens),
			       sum(thinking_tokens)
			  FROM folded
			 GROUP BY model
			 ORDER BY sum(output_tokens) DESC`)
		if err != nil {
			return fmt.Errorf("derive: summarize by model: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var m ModelUsage
			if err := rows.Scan(&m.Model, &m.Requests,
				&m.InputTokens, &m.OutputTokens, &m.CacheReadTokens,
				&m.CacheWrite5mTokens, &m.CacheWrite1hTokens, &m.Thinking); err != nil {
				return fmt.Errorf("derive: scan model row: %w", err)
			}
			sum.Models = append(sum.Models, m)
		}
		return rows.Err()
	})
	if err != nil {
		return Summary{}, err
	}

	s.price(&sum)
	return sum, nil
}

// price fills in costs and the derived ratios.
func (s *Service) price(sum *Summary) {
	var cacheRead, cacheOther int64
	for i := range sum.Models {
		m := &sum.Models[i]
		m.Usage = pricing.Usage{
			Input:        m.InputTokens,
			Output:       m.OutputTokens,
			CacheRead:    m.CacheReadTokens,
			CacheWrite5m: m.CacheWrite5mTokens,
			CacheWrite1h: m.CacheWrite1hTokens,
		}
		cost, ok := s.prices.Cost(m.Model, m.Usage)
		m.CostUSD, m.Priced = cost, ok
		if !ok {
			sum.UnpricedModels = append(sum.UnpricedModels, m.Model)
		} else {
			uncached, _ := s.prices.UncachedCost(m.Model, m.Usage)
			m.UncachedCostUSD = uncached
			sum.TotalCostUSD += cost
			sum.TotalUncachedCostUSD += uncached
		}
		sum.Requests += m.Requests
		cacheRead += m.CacheReadTokens
		cacheOther += m.InputTokens + m.CacheWrite5mTokens + m.CacheWrite1hTokens
	}

	sum.CacheSavingsUSD = sum.TotalUncachedCostUSD - sum.TotalCostUSD
	if total := cacheRead + cacheOther; total > 0 {
		sum.CacheHitRate = float64(cacheRead) / float64(total)
	}
	if sum.Requests > 0 {
		sum.Inflation = float64(sum.AssistantLines) / float64(sum.Requests)
	}
}

// Daily returns per-day activity, most recent last.
func (s *Service) Daily(ctx context.Context, userID string) ([]Day, error) {
	var days []Day
	err := s.db.InTenantTx(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, foldedUsageCTE+`
			SELECT day, model, count(*), sum(output_tokens),
			       sum(input_tokens), sum(cache_read_tokens),
			       sum(cache_write_5m_tokens), sum(cache_write_1h_tokens)
			  FROM folded
			 WHERE day <> ''
			 GROUP BY day, model
			 ORDER BY day`)
		if err != nil {
			return fmt.Errorf("derive: daily: %w", err)
		}
		defer rows.Close()

		byDay := make(map[string]*Day)
		var order []string
		for rows.Next() {
			var (
				day, model string
				u          pricing.Usage
				requests   int64
				output     int64
			)
			if err := rows.Scan(&day, &model, &requests, &output,
				&u.Input, &u.CacheRead, &u.CacheWrite5m, &u.CacheWrite1h); err != nil {
				return fmt.Errorf("derive: scan daily row: %w", err)
			}
			u.Output = output
			d, ok := byDay[day]
			if !ok {
				d = &Day{Date: day}
				byDay[day] = d
				order = append(order, day)
			}
			d.Requests += requests
			d.OutputTokens += output
			if cost, priced := s.prices.Cost(model, u); priced {
				d.CostUSD += cost
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, day := range order {
			days = append(days, *byDay[day])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return days, nil
}
