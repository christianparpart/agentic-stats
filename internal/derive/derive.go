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

// Stack is a way of dividing a day's cost.
//
// A named type rather than a bare string so a caller cannot ask for "sessions"
// and silently receive nothing. The wire form is the string, which is what the
// dashboard keys its selector on.
type Stack string

const (
	// StackModel divides by the model that answered.
	StackModel Stack = "model"
	// StackProject divides by the repository the session's pull requests went
	// to, which is the only project identity the archive actually holds.
	StackProject Stack = "project"
	// StackPullRequest divides by the pull request the session opened.
	StackPullRequest Stack = "pull_request"
	// StackSession divides by the assistant session.
	StackSession Stack = "session"
	// StackPeer divides by the machine that collected the work.
	StackPeer Stack = "peer"
)

// MaxSegments is how many named slices a stack shows before the rest is folded
// into one.
//
// Six because that is how many categorical colours the dashboard has that are
// distinguishable from each other, and a stacked bar has no room for the
// direct labels the horizontal charts use -- past the palette, a seventh
// colour is not another category, it is a lie.
const MaxSegments = 6

// OtherKey and NoneKey are the two segments that are not a real category.
//
// Spelled so they cannot collide with a model name, a repository or a session
// id, because a collision would silently merge a real category into a
// catch-all and the total would still add up.
const (
	OtherKey = "__other__"
	NoneKey  = "__none__"
)

// Share is one segment's part of one day's cost.
//
// Only the key: the label lives once in the Legend rather than being repeated
// for every day it appears on.
type Share struct {
	Key     string  `json:"key"`
	CostUSD float64 `json:"cost_usd"`
}

// Segment names one slice of a stacked bar.
type Segment struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// TotalUSD is this segment's cost across every day reported, which is what
	// the segments were ranked by.
	//
	// Ranked over the whole range rather than per day on purpose: a colour has
	// to mean the same thing on every bar, and a per-day ranking would make
	// slot three a different session each time it appeared.
	TotalUSD float64 `json:"total_usd"`
}

// Legend is one stacking dimension and the segments it divides into, in the
// order they stack.
type Legend struct {
	Stack Stack  `json:"stack"`
	Label string `json:"label"`
	// Note explains an allocation where the division is not a measurement.
	Note     string    `json:"note,omitempty"`
	Segments []Segment `json:"segments"`
}

// Day is one day's activity, in UTC.
type Day struct {
	Date         string  `json:"date"`
	Requests     int64   `json:"requests"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`

	// Stacks divides CostUSD by each dimension, in legend order.
	//
	// Every dimension's shares sum to CostUSD. That is the property the chart
	// rests on and the one the tests check hardest: splitting a session's cost
	// between the pull requests it opened must redistribute it, never create
	// or destroy it.
	Stacks map[Stack][]Share `json:"stacks,omitempty"`
}

// Activity is the daily report: one entry per active day, and the legends
// needed to read the stacks.
type Activity struct {
	Days    []Day    `json:"days"`
	Legends []Legend `json:"legends"`
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
           -- The machine that collected this request. A bare column beside the
           -- min(), so it comes from the same picked row: a request collected
           -- on two machines is billed to one of them, deterministically,
           -- rather than to both.
           origin_id,
           substr(coalesce(captured_at, ''), 1, 10) AS day,
           input_tokens, output_tokens, think_tokens,
           cache_read, cache_write5m, cache_write1h,
           min(origin_id || ':' || printf('%020d', seq)) AS pick
      FROM records
     WHERE request_id IS NOT NULL
     GROUP BY request_id
)`

// The reports the dashboard asks for, as constants rather than string literals
// inside their methods.
//
// They are here because their cost is a property of the schema, not of the Go
// around them: each one is only fast while `records_fold` still carries every
// column the fold reads, and the moment one of them asks for a column the
// index does not have, the fold goes back to fetching whole rows -- sealed
// bodies and all -- out of a multi-gigabyte table. Gathering them makes that
// checkable, and planTest walks exactly this list.

// modelUsageQuery totals the fold per model, biggest producer first.
const modelUsageQuery = foldedUsageCTE + `
SELECT coalesce(model, 'unknown'),
       count(*),
       sum(input_tokens), sum(output_tokens),
       sum(cache_read), sum(cache_write5m), sum(cache_write1h),
       sum(think_tokens)
  FROM folded
 GROUP BY model
 ORDER BY sum(output_tokens) DESC`

// dailyUsageQuery totals the fold per day and per stackable dimension.
//
// One grid rather than one query per dimension. Model has to stay in the
// grouping whatever else is asked for, because cost is a per-model function of
// five token counts applied in Go, so every other dimension has to be totalled
// alongside it and folded afterwards. Once model is there, adding the origin
// and the session costs nothing but rows -- both are columns of `records_fold`
// already -- and one query then answers every dimension the dashboard offers.
//
// Around 1,200 rows on a 480k-record archive: a day has a handful of sessions,
// each using a couple of models, on one machine.
const dailyUsageQuery = foldedUsageCTE + `
SELECT day, coalesce(model, 'unknown'), origin_id, coalesce(session_id, ''),
       count(*), sum(output_tokens),
       sum(input_tokens), sum(cache_read),
       sum(cache_write5m), sum(cache_write1h)
  FROM folded
 WHERE day <> ''
 GROUP BY day, model, origin_id, session_id
 ORDER BY day`

// sessionLinksQuery names the pull requests each session opened.
//
// Covered by records_pr_session, and small -- a few hundred rows. It carries
// two dimensions at once: the pull request itself, and the project, which is
// the repository the pull request went to.
const sessionLinksQuery = `
SELECT DISTINCT session_id, pr_repo, pr_number
  FROM records
 WHERE pr_repo IS NOT NULL AND session_id IS NOT NULL`

// deliveriesQuery splits each session's usage between the pull requests it
// produced, one row per (pull request, model).
const deliveriesQuery = foldedUsageCTE + `,
-- Totalled per session before the join rather than after it.
--
-- Every term below is a sum or a count, and the divisor is fixed for a
-- session, so summing within the session first and joining the totals gives
-- the same answer as joining first -- but over a few hundred rows instead of
-- one per request. Joining first made the planner build the pull-request side
-- into an ephemeral index and probe it once per folded request: 23 seconds on
-- a 480k-record archive, against 0.3 for this.
per_session AS (
    SELECT session_id, model,
           count(*)           AS requests,
           sum(input_tokens)  AS input_tokens,
           sum(output_tokens) AS output_tokens,
           sum(cache_read)    AS cache_read,
           sum(cache_write5m) AS cache_write5m,
           sum(cache_write1h) AS cache_write1h
      FROM folded
     WHERE session_id IS NOT NULL
     GROUP BY session_id, model
),
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
       coalesce(s.model, 'unknown'),
       sum(s.requests),
       -- links holds each (session, pull request) once, so one row here is
       -- one session and counting rows counts distinct sessions.
       count(*),
       sum(CASE WHEN w.pr_count > 1 THEN s.requests ELSE 0 END),
       sum(s.output_tokens),
       sum(CAST(s.input_tokens   AS REAL) / w.pr_count),
       sum(CAST(s.output_tokens  AS REAL) / w.pr_count),
       sum(CAST(s.cache_read     AS REAL) / w.pr_count),
       sum(CAST(s.cache_write5m  AS REAL) / w.pr_count),
       sum(CAST(s.cache_write1h  AS REAL) / w.pr_count)
  FROM per_session s
  JOIN links   l ON l.session_id = s.session_id
  JOIN weights w ON w.session_id = s.session_id
 GROUP BY l.pr_repo, l.pr_number, s.model`

// attributionQuery counts folded requests belonging to a session that shipped
// a pull request, against all folded requests.
const attributionQuery = foldedUsageCTE + `
SELECT coalesce(sum(CASE WHEN session_id IN (
           SELECT session_id FROM records WHERE pr_repo IS NOT NULL
       ) THEN 1 ELSE 0 END), 0),
       count(*)
  FROM folded`

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

	rows, err := db.QueryContext(ctx, modelUsageQuery)
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

	rows, err := s.db.SQL().QueryContext(ctx, deliveriesQuery)
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

	// Counted on the fold, so a session opening several pull requests
	// contributes once rather than once per pull request.
	//
	// Over the fold rather than over raw lines, because the two do not always
	// agree: 80 requests in a 480k-record archive have lines under more than
	// one session id, and counting lines called every one of them attributed
	// if any of its sessions shipped a pull request -- while the table above
	// bills each to the single session the fold picked. The ratio has to
	// describe the same attribution the table performed, or it is measuring
	// nothing. It is also the difference between 12 seconds and 0.2.
	var attributed, total int64
	row := s.db.SQL().QueryRowContext(ctx, attributionQuery)
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
