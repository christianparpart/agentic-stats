package integration_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/seal"
)

// dayLine is an assistant response on a named day, so a test can have more
// than one bar. Everything else here fixes a single timestamp, which is why no
// test could see the daily report split anything until now.
func dayLine(date, session, requestID, model string, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":%q,"sessionId":%q,`+
		`"timestamp":"%sT12:00:00.000Z","message":{"model":%q,`+
		`"usage":{"input_tokens":10,"output_tokens":%d,"cache_read_input_tokens":100,`+
		`"output_tokens_details":{"thinking_tokens":5},`+
		`"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":0}}}}`,
		requestID, requestID+"-u", session, date, model, output)
}

// stackTotals sums a day's shares per dimension.
func stackTotals(d derive.Day, stack derive.Stack) float64 {
	var total float64
	for _, s := range d.Stacks[stack] {
		total += s.CostUSD
	}
	return total
}

func shareOf(d derive.Day, stack derive.Stack, key string) float64 {
	for _, s := range d.Stacks[stack] {
		if s.Key == key {
			return s.CostUSD
		}
	}
	return 0
}

func legendFor(t *testing.T, a derive.Activity, stack derive.Stack) derive.Legend {
	t.Helper()
	for _, l := range a.Legends {
		if l.Stack == stack {
			return l
		}
	}
	t.Fatalf("no legend for %q; got %d legends", stack, len(a.Legends))
	return derive.Legend{}
}

// The property the whole chart rests on: however a day is divided, the pieces
// are that day's cost. A stack that does not add up is not a view of the data,
// it is a second, contradictory answer to the same question.
func TestEveryStackSumsToTheDaysCost(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLine("2026-09-01", "s-alpha", "r1", "claude-opus-5", 1000),
		dayLine("2026-09-01", "s-beta", "r2", "claude-sonnet-5", 400),
		dayLine("2026-09-02", "s-alpha", "r3", "claude-opus-5", 700),
		dayLine("2026-09-02", "s-gamma", "r4", "claude-opus-5", 250),
		prLink("s-alpha", "acme/widgets", 1),
		prLink("s-beta", "acme/gadgets", 2),
		// s-gamma shipped nothing, so it lands in the unattributed segment
		// rather than being guessed at.
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(activity.Days) != 2 {
		t.Fatalf("got %d days, want 2", len(activity.Days))
	}
	if len(activity.Legends) != 5 {
		t.Fatalf("got %d legends, want one per dimension", len(activity.Legends))
	}

	for _, d := range activity.Days {
		if d.CostUSD <= 0 {
			t.Fatalf("%s cost nothing; the fixture should be priced", d.Date)
		}
		for _, l := range activity.Legends {
			got := stackTotals(d, l.Stack)
			if math.Abs(got-d.CostUSD) > 1e-9 {
				t.Errorf("%s stacked by %s sums to $%.6f, but the day cost $%.6f",
					d.Date, l.Stack, got, d.CostUSD)
			}
		}
	}
}

// Days must stay separate and in order, or the bars are in the wrong places.
func TestDaysAreSeparateAndOldestFirst(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLine("2026-09-03", "s1", "r3", "claude-opus-5", 300),
		dayLine("2026-09-01", "s1", "r1", "claude-opus-5", 100),
		dayLine("2026-09-02", "s1", "r2", "claude-opus-5", 200),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	var dates []string
	for _, d := range activity.Days {
		dates = append(dates, d.Date)
	}
	want := []string{"2026-09-01", "2026-09-02", "2026-09-03"}
	if fmt.Sprint(dates) != fmt.Sprint(want) {
		t.Errorf("days = %v, want %v", dates, want)
	}
}

// A session that shipped to two repositories did work for both, and there is
// nothing saying how to divide it -- so it splits evenly, the same rule the
// delivery card uses. Attributing it whole to each would inflate the total,
// which is the double-count the requestId fold exists to prevent.
func TestASessionShippingToTwoRepositoriesSplitsEvenly(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLine("2026-09-01", "s-split", "r1", "claude-opus-5", 1000),
		prLink("s-split", "acme/widgets", 1),
		prLink("s-split", "acme/gadgets", 2),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(activity.Days) != 1 {
		t.Fatalf("got %d days, want 1", len(activity.Days))
	}
	d := activity.Days[0]

	widgets := shareOf(d, derive.StackProject, "acme/widgets")
	gadgets := shareOf(d, derive.StackProject, "acme/gadgets")
	if widgets <= 0 || math.Abs(widgets-gadgets) > 1e-9 {
		t.Errorf("split = %v and %v, want two equal halves", widgets, gadgets)
	}
	if total := widgets + gadgets; math.Abs(total-d.CostUSD) > 1e-9 {
		t.Errorf("halves sum to $%.6f, want the day's $%.6f", total, d.CostUSD)
	}
}

// Work that never opened a pull request has no project. Saying so is the
// point: the alternative is inventing one.
func TestWorkWithoutAPullRequestIsNotAttributed(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLine("2026-09-01", "s-shipped", "r1", "claude-opus-5", 1000),
		dayLine("2026-09-01", "s-explored", "r2", "claude-opus-5", 1000),
		prLink("s-shipped", "acme/widgets", 1),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := activity.Days[0]

	if got := shareOf(d, derive.StackProject, derive.NoneKey); got <= 0 {
		t.Errorf("the unshipped session contributed %v to the unattributed segment, want its cost", got)
	}
	for _, seg := range legendFor(t, activity, derive.StackProject).Segments {
		if seg.Key != derive.NoneKey {
			continue
		}
		if seg.Label != "No pull request" {
			t.Errorf("unattributed segment is labelled %q", seg.Label)
		}
	}
}

// Past the palette a seventh colour is not another category, so the tail is
// gathered -- and gathering it must not lose any of it.
func TestTheTailIsGatheredWithoutLosingIt(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	// Ten sessions on one day, each cheaper than the last, so the ranking has
	// something unambiguous to do.
	var lines []string
	for i := range 10 {
		session := fmt.Sprintf("s-%02d", i)
		lines = append(lines, dayLine("2026-09-01", session,
			fmt.Sprintf("r-%02d", i), "claude-opus-5", int64(1000-i*50)))
	}
	if _, err := n.writer.Ingest(ctx, records(lines...)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	legend := legendFor(t, activity, derive.StackSession)
	if len(legend.Segments) != derive.MaxSegments+1 {
		t.Fatalf("legend has %d segments, want %d named plus Other",
			len(legend.Segments), derive.MaxSegments)
	}
	last := legend.Segments[len(legend.Segments)-1]
	if last.Key != derive.OtherKey {
		t.Errorf("last segment is %q, want the gathered tail last so it never takes a colour slot", last.Key)
	}

	// Ranked by cost, so the most expensive session must be named rather than
	// swept into the tail.
	if legend.Segments[0].Key != "s-00" {
		t.Errorf("first segment is %q, want the most expensive session", legend.Segments[0].Key)
	}
	// Descending, or the colours are assigned to the wrong sessions.
	for i := 1; i < len(legend.Segments)-1; i++ {
		if legend.Segments[i].TotalUSD > legend.Segments[i-1].TotalUSD {
			t.Errorf("segment %d costs more than the one before it; the ranking is not descending", i)
		}
	}

	d := activity.Days[0]
	if got := stackTotals(d, derive.StackSession); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("gathering the tail changed the total: $%.6f against the day's $%.6f", got, d.CostUSD)
	}
	if shareOf(d, derive.StackSession, derive.OtherKey) <= 0 {
		t.Error("four sessions were dropped rather than gathered")
	}
}

// Records collected on another machine must stack under that machine, which is
// the whole point of the dimension.
func TestPeerStackingSeparatesTheMachines(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	a := newNodeWithKey(t, psk)
	b := newNodeWithKey(t, psk)
	ctx := context.Background()

	if _, err := a.writer.Ingest(ctx, records(
		dayLine("2026-09-01", "s-a", "r-a", "claude-opus-5", 1000))); err != nil {
		t.Fatalf("Ingest on A: %v", err)
	}
	if _, err := b.writer.Ingest(ctx, records(
		dayLine("2026-09-01", "s-b", "r-b", "claude-opus-5", 1000))); err != nil {
		t.Fatalf("Ingest on B: %v", err)
	}
	// Hand A's records to B exactly as a sync would, origin and sequence intact.
	shipped, err := a.db.Since(ctx, a.db.OriginID(), 0, 1000)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if _, err := b.db.AppendRemote(ctx, shipped); err != nil {
		t.Fatalf("AppendRemote on B: %v", err)
	}

	activity, err := b.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily on B: %v", err)
	}
	d := activity.Days[0]
	if got := len(d.Stacks[derive.StackPeer]); got != 2 {
		t.Fatalf("the day divides into %d machines, want 2", got)
	}
	if got := stackTotals(d, derive.StackPeer); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("machines sum to $%.6f, want the day's $%.6f", got, d.CostUSD)
	}

	// B is looking at its own report, so B is "this machine" and A is not.
	var self int
	for _, seg := range legendFor(t, activity, derive.StackPeer).Segments {
		if seg.Label == "this machine" {
			self++
			if seg.Key != b.db.OriginID() {
				t.Errorf("%q is labelled as this machine, but this machine is %q",
					seg.Key, b.db.OriginID())
			}
		}
	}
	if self != 1 {
		t.Errorf("%d machines call themselves this one", self)
	}
}

// A fresh node must serve a shaped report rather than nulls: init, run, open
// the dashboard before the first pass finishes is the ordinary first run.
func TestAnEmptyArchiveReportsEmptyRatherThanNull(t *testing.T) {
	n := newNode(t)
	activity, err := n.derive.Daily(context.Background())
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if activity.Days == nil {
		t.Error("days is nil, which serialises as null and breaks a .map on the page")
	}
	if len(activity.Days) != 0 {
		t.Errorf("got %d days from an empty archive", len(activity.Days))
	}
}
