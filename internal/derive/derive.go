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
	"sort"
	"strconv"

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
// The fold is `GROUP BY request_id` with a single `min()` over bare columns.
// SQLite documents this: with exactly one min() or max() in the query, bare
// columns are taken from the row that produced it. That is precisely the fold's
// semantics -- every line of a multi-block response carries an identical usage
// object, so any one of them is the whole truth and the rest are copies.
//
// The min() is over a composite of origin and sequence rather than over one
// column, because ties must be *impossible*, not merely unlikely: two nodes
// running this query must return the same row or we would chase phantom
// divergence between replicas that actually agree.
//
// Rows with a NULL request_id are excluded by construction -- ingest leaves it
// NULL on anything that is not a billable assistant response, including the
// synthetic error lines that carry all-zero usage.
const foldedUsageCTE = `
WITH folded AS (
    SELECT model,
           session_id,
           substr(coalesce(captured_at, ''), 1, 10) AS day,
           input_tokens, output_tokens, think_tokens,
           cache_read, cache_write5m, cache_write1h,
           min(origin_id || ':' || printf('%020d', seq)) AS pick
      FROM records
     WHERE request_id IS NOT NULL
     GROUP BY request_id
)`

// Summarize computes the whole-archive summary.
func (s *Service) Summarize(ctx context.Context) (Summary, error) {
	// Empty rather than nil, so an archive with nothing in it serialises as []
	// and not null. A fresh node is the common case -- init, run, open the
	// dashboard before the first pass finishes -- and a consumer that reasonably
	// expects a list should not meet null on its very first request.
	sum := Summary{Models: []ModelUsage{}}
	db := s.db.SQL()

	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&sum.Lines); err != nil {
		return Summary{}, fmt.Errorf("derive: count lines: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM records WHERE request_id IS NOT NULL`).Scan(&sum.AssistantLines); err != nil {
		return Summary{}, fmt.Errorf("derive: count assistant lines: %w", err)
	}

	rows, err := db.QueryContext(ctx, foldedUsageCTE+`
		SELECT coalesce(model, 'unknown'),
		       count(*),
		       sum(input_tokens), sum(output_tokens),
		       sum(cache_read), sum(cache_write5m), sum(cache_write1h),
		       sum(think_tokens)
		  FROM folded
		 GROUP BY model
		 ORDER BY sum(output_tokens) DESC`)
	if err != nil {
		return Summary{}, fmt.Errorf("derive: summarize by model: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests,
			&m.InputTokens, &m.OutputTokens, &m.CacheReadTokens,
			&m.CacheWrite5mTokens, &m.CacheWrite1hTokens, &m.Thinking); err != nil {
			return Summary{}, fmt.Errorf("derive: scan model row: %w", err)
		}
		sum.Models = append(sum.Models, m)
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("derive: summarize by model: %w", err)
	}

	s.price(&sum)
	return sum, nil
}

// Daily returns per-day activity, oldest first.
func (s *Service) Daily(ctx context.Context) ([]Day, error) {
	rows, err := s.db.SQL().QueryContext(ctx, foldedUsageCTE+`
		SELECT day, coalesce(model, 'unknown'), count(*), sum(output_tokens),
		       sum(input_tokens), sum(cache_read),
		       sum(cache_write5m), sum(cache_write1h)
		  FROM folded
		 WHERE day <> ''
		 GROUP BY day, model
		 ORDER BY day`)
	if err != nil {
		return nil, fmt.Errorf("derive: daily: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

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
			return nil, fmt.Errorf("derive: scan daily row: %w", err)
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
		return nil, fmt.Errorf("derive: daily: %w", err)
	}

	days := make([]Day, 0, len(order))
	for _, day := range order {
		days = append(days, *byDay[day])
	}
	return days, nil
}

// PullRequest is what one shipped pull request cost.
type PullRequest struct {
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
	// Sessions and Requests count the work that *touched* this pull request.
	// They are not additive across pull requests: one session can open
	// several, and its requests are counted against each of them.
	Sessions int64 `json:"sessions"`
	Requests int64 `json:"requests"`
	Output   int64 `json:"output_tokens"`
	// CostUSD is this pull request's *share*. Where a session opened several,
	// its cost is split evenly between them, so these do sum -- and sum to no
	// more than the archive total.
	CostUSD float64 `json:"cost_usd"`
	// SharedSessions is how many of this PR's sessions also produced others,
	// which is what makes the share an allocation rather than a measurement.
	SharedSessions int64 `json:"shared_sessions"`
}

// Delivery reports what the archive says about shipped work.
type Delivery struct {
	PullRequests []PullRequest `json:"pull_requests"`
	// TotalCostUSD is the cost of sessions that produced a pull request.
	TotalCostUSD float64 `json:"total_cost_usd"`
	// Attributed is the share of all requests belonging to a session that
	// produced at least one pull request. Counted on distinct requests, so it
	// cannot exceed one however many pull requests a session opened.
	Attributed float64 `json:"attributed"`
}

// Deliveries attributes cost to the pull requests sessions produced.
//
// The join itself is exact: the assistant records a pr-link line naming the
// repository and number when it opens a pull request, so no branch-name
// guessing or time-window correlation is involved.
//
// The *allocation* is not exact, and says so. A session that opened three pull
// requests did work for all three, and there is nothing in the transcript that
// says how to divide it, so the cost is split evenly. Attributing the whole
// session to each would triple-count -- which is the same error the requestId
// fold exists to prevent, arriving by a different route.
func (s *Service) Deliveries(ctx context.Context) (Delivery, error) {
	// Empty rather than nil, for the same reason as Summarize: a list-shaped
	// field should never serialise as null.
	out := Delivery{PullRequests: []PullRequest{}}

	rows, err := s.db.SQL().QueryContext(ctx, foldedUsageCTE+`,
links AS (
    SELECT DISTINCT session_id, pr_repo, pr_number
      FROM records
     WHERE pr_repo IS NOT NULL AND session_id IS NOT NULL
),
-- How many pull requests each session produced, which is the divisor.
weights AS (
    SELECT session_id, count(*) AS pr_count FROM links GROUP BY session_id
)
SELECT l.pr_repo, l.pr_number,
       coalesce(f.model, 'unknown'),
       count(*),
       count(DISTINCT f.session_id),
       sum(CASE WHEN w.pr_count > 1 THEN 1 ELSE 0 END),
       sum(f.output_tokens),
       sum(CAST(f.input_tokens   AS REAL) / w.pr_count),
       sum(CAST(f.output_tokens  AS REAL) / w.pr_count),
       sum(CAST(f.cache_read     AS REAL) / w.pr_count),
       sum(CAST(f.cache_write5m  AS REAL) / w.pr_count),
       sum(CAST(f.cache_write1h  AS REAL) / w.pr_count)
  FROM folded f
  JOIN links   l ON l.session_id = f.session_id
  JOIN weights w ON w.session_id = f.session_id
 GROUP BY l.pr_repo, l.pr_number, f.model`)
	if err != nil {
		return Delivery{}, fmt.Errorf("derive: deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	// One row per (pull request, model); fold to one entry per pull request so
	// a session that switched models still reports a single cost.
	index := make(map[string]int)
	for rows.Next() {
		var (
			repo, model                       string
			number, requests, sessions, share int64
			output                            int64
			in, outTok, cr, c5, c1            float64
		)
		if err := rows.Scan(&repo, &number, &model, &requests, &sessions, &share, &output,
			&in, &outTok, &cr, &c5, &c1); err != nil {
			return Delivery{}, fmt.Errorf("derive: scan delivery row: %w", err)
		}
		cost, _ := s.prices.Cost(model, pricing.Usage{
			Input:        int64(in),
			Output:       int64(outTok),
			CacheRead:    int64(cr),
			CacheWrite5m: int64(c5),
			CacheWrite1h: int64(c1),
		})

		key := repo + "#" + strconv.FormatInt(number, 10)
		if i, ok := index[key]; ok {
			out.PullRequests[i].Requests += requests
			out.PullRequests[i].Output += output
			out.PullRequests[i].CostUSD += cost
			continue
		}
		index[key] = len(out.PullRequests)
		out.PullRequests = append(out.PullRequests, PullRequest{
			Repo: repo, Number: number, Sessions: sessions, Requests: requests,
			Output: output, CostUSD: cost, SharedSessions: share,
		})
	}
	if err := rows.Err(); err != nil {
		return Delivery{}, fmt.Errorf("derive: deliveries: %w", err)
	}

	for _, pr := range out.PullRequests {
		out.TotalCostUSD += pr.CostUSD
	}
	sort.Slice(out.PullRequests, func(i, j int) bool {
		return out.PullRequests[i].CostUSD > out.PullRequests[j].CostUSD
	})

	// Counted on distinct requests, so a session opening several pull requests
	// contributes once rather than once per pull request.
	var attributed, total int64
	row := s.db.SQL().QueryRowContext(ctx, `
		SELECT
		  (SELECT count(DISTINCT request_id) FROM records
		    WHERE request_id IS NOT NULL
		      AND session_id IN (SELECT session_id FROM records WHERE pr_repo IS NOT NULL)),
		  (SELECT count(DISTINCT request_id) FROM records WHERE request_id IS NOT NULL)`)
	if err := row.Scan(&attributed, &total); err != nil {
		return Delivery{}, fmt.Errorf("derive: count attributed requests: %w", err)
	}
	if total > 0 {
		out.Attributed = float64(attributed) / float64(total)
	}
	return out, nil
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
			// An unknown model is surfaced, never silently free.
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
