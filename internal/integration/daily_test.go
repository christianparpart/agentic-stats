package integration_test

import (
	"context"
	"encoding/json"
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
	return dayLineIn(date, `D:\fastcached`, "master", session, requestID, model, output)
}

// dayLineIn is dayLine with the provenance a project and a branch are read
// from. A separate helper rather than four more parameters on every caller: the
// tests that care about where work happened are the minority.
func dayLineIn(date, cwd, branch, session, requestID, model string, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":%q,"sessionId":%q,`+
		`"cwd":%s,"gitBranch":%q,`+
		`"timestamp":"%sT12:00:00.000Z","message":{"model":%q,`+
		`"usage":{"input_tokens":10,"output_tokens":%d,"cache_read_input_tokens":100,`+
		`"output_tokens_details":{"thinking_tokens":5},`+
		`"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":0}}}}`,
		requestID, requestID+"-u", session, mustJSON(cwd), branch, date, model, output)
}

// mustJSON quotes a value the way the transcript does. Needed for the working
// directory alone: a Windows path is full of backslashes and %q would render
// them as Go escapes rather than JSON ones.
func mustJSON(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
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

// labelOf is how a segment reads to a person, which for the two segments that
// are not categories is the whole assertion.
func labelOf(t *testing.T, a derive.Activity, stack derive.Stack, key string) string {
	t.Helper()
	for _, seg := range legendFor(t, a, stack).Segments {
		if seg.Key == key {
			return seg.Label
		}
	}
	t.Fatalf("no %q segment in the %q legend", key, stack)
	return ""
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
		// s-gamma shipped nothing, so it lands in the unattributed segment of
		// the pull-request stack. It still has a project: every line says which
		// directory it ran in.
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
	if len(activity.Legends) != 6 {
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

// A session that opened two pull requests did work for both, and there is
// nothing saying how to divide it -- so it splits evenly, the same rule the
// delivery card uses. Attributing it whole to each would inflate the total,
// which is the double-count the requestId fold exists to prevent.
//
// This is the pull-request dimension's rule and only its own. A project is read
// from each record's working directory, so a row belongs to exactly one and
// there is nothing to allocate.
func TestASessionShippingToTwoPullRequestsSplitsEvenly(t *testing.T) {
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

	widgets := shareOf(d, derive.StackPullRequest, "acme/widgets#1")
	gadgets := shareOf(d, derive.StackPullRequest, "acme/gadgets#2")
	if widgets <= 0 || math.Abs(widgets-gadgets) > 1e-9 {
		t.Errorf("split = %v and %v, want two equal halves", widgets, gadgets)
	}
	if total := widgets + gadgets; math.Abs(total-d.CostUSD) > 1e-9 {
		t.Errorf("halves sum to $%.6f, want the day's $%.6f", total, d.CostUSD)
	}
}

// Work that never opened a pull request is not attributed to one. Saying so is
// the point: the alternative is inventing one.
//
// What it must no longer do is cost that work its project. Most work opens no
// pull request, and for as long as a project meant "the repository this
// session's pull requests went to", most of the archive was attributed to
// nothing at all -- one enormous segment labelled "No pull request", which is
// what prompted all of this.
func TestWorkWithoutAPullRequestStillHasAProject(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s-shipped", "r1", "claude-opus-5", 1000),
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s-explored", "r2", "claude-opus-5", 1000),
		prLink("s-shipped", "acme/widgets", 1),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := activity.Days[0]

	// The pull-request stack still says so, and still by that name.
	if got := shareOf(d, derive.StackPullRequest, derive.NoneKey); got <= 0 {
		t.Errorf("the unshipped session contributed %v to the unattributed segment, want its cost", got)
	}
	if got := labelOf(t, activity, derive.StackPullRequest, derive.NoneKey); got != "No pull request" {
		t.Errorf("the pull-request stack labels its empty segment %q", got)
	}

	// The project stack does not: both sessions worked on fastcached.
	if got := shareOf(d, derive.StackProject, "fastcached"); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("fastcached got $%.6f of the day, want all $%.6f", got, d.CostUSD)
	}
	if got := shareOf(d, derive.StackProject, derive.NoneKey); got != 0 {
		t.Errorf("$%.6f was attributed to no project", got)
	}
}

// A record that carries no working directory at all has no project, and must
// say that rather than borrow one.
func TestWorkWithoutAWorkingDirectoryHasNoProject(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", "", "", "s-nowhere", "r1", "claude-opus-5", 1000),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := activity.Days[0]

	if got := shareOf(d, derive.StackProject, derive.NoneKey); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("unattributed got $%.6f, want the day's $%.6f", got, d.CostUSD)
	}
	// Its own name, not the pull request's: these are different absences, and
	// sharing one label was how the old rule hid behind the new one's excuse.
	if got := labelOf(t, activity, derive.StackProject, derive.NoneKey); got != "No project" {
		t.Errorf("the project stack labels its empty segment %q", got)
	}
	if got := labelOf(t, activity, derive.StackBranch, derive.NoneKey); got != "No branch" {
		t.Errorf("the branch stack labels its empty segment %q", got)
	}
}

// Every worktree of a project is that project. Without this the chart shatters:
// this author's archive holds 68 distinct directories for one repository, most
// of them created by the assistant itself, and each would take a slice of the
// bar and push the real projects into Other.
func TestWorktreesFoldIntoTheirProject(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s1", "r1", "claude-opus-5", 500),
		dayLineIn("2026-09-01", `D:\fastcached\.claude\worktrees\agent-a0c5d2774e77a518f`,
			"claude/x", "s2", "r2", "claude-opus-5", 500),
		dayLineIn("2026-09-01", `D:\fastcached-issue-154`, "feature/154", "s3", "r3", "claude-opus-5", 500),
		dayLineIn("2026-09-01", `D:\endo`, "master", "s4", "r4", "claude-opus-5", 500),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := activity.Days[0]

	if got := len(d.Stacks[derive.StackProject]); got != 2 {
		t.Fatalf("the day divides into %d projects, want fastcached and endo", got)
	}
	fastcached := shareOf(d, derive.StackProject, "fastcached")
	endo := shareOf(d, derive.StackProject, "endo")
	if math.Abs(fastcached-3*endo) > 1e-9 {
		t.Errorf("fastcached got $%.6f and endo $%.6f; want three of the four rows folded together",
			fastcached, endo)
	}
	if total := fastcached + endo; math.Abs(total-d.CostUSD) > 1e-9 {
		t.Errorf("projects sum to $%.6f, want the day's $%.6f", total, d.CostUSD)
	}
}

// The branch is what tells apart the work that opened no pull request, which is
// why it is worth a dimension of its own.
func TestTheBranchStackSeparatesTheBranches(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s1", "r1", "claude-opus-5", 1000),
		dayLineIn("2026-09-01", `D:\fastcached`, "feature/139-toolchain", "s2", "r2", "claude-opus-5", 1000),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := activity.Days[0]

	master := shareOf(d, derive.StackBranch, "master")
	feature := shareOf(d, derive.StackBranch, "feature/139-toolchain")
	if master <= 0 || math.Abs(master-feature) > 1e-9 {
		t.Errorf("branches got $%.6f and $%.6f, want the two halves", master, feature)
	}
	// One project, two branches: the dimensions have to divide the same day
	// differently without either losing any of it.
	if got := shareOf(d, derive.StackProject, "fastcached"); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("fastcached got $%.6f, want the whole day's $%.6f", got, d.CostUSD)
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

// A worktree with nothing in its name to give it away still belongs to its
// project, because the pull request it opened says which repository it is.
//
// This is the case the naming rules cannot reach and must not guess at:
// `fastcached-distributed-compilation` looks exactly like a project of its own,
// and `contour-terminal` -- which is one -- looks exactly like a worktree. Only
// the pull request tells them apart.
func TestAWorktreeWithNoMarkerFoldsOnItsPullRequest(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s-main", "r1", "claude-opus-5", 500),
		// A worktree named after the work, not after an issue number.
		dayLineIn("2026-09-01", `D:\fastcached-distributed-compilation`,
			"feature/distributed-compilation", "s-wt", "r2", "claude-opus-5", 500),
		// A build tree inside the repository, which is not a project either.
		dayLineIn("2026-09-01", `D:\fastcached\out\build\cl-release`,
			"master", "s-build", "r3", "claude-opus-5", 500),
		// And a genuinely separate project whose name merely looks like a
		// worktree of another. Its pull request names the path, so it stays.
		dayLineIn("2026-09-01", `D:\contour-terminal`, "master", "s-other", "r4", "claude-opus-5", 500),
		prLink("s-wt", "LASTRADA-Software/fastcached", 7),
		prLink("s-build", "LASTRADA-Software/fastcached", 8),
		prLink("s-other", "contour-terminal/contour-terminal", 9),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := activity.Days[0]

	if got := len(d.Stacks[derive.StackProject]); got != 2 {
		t.Fatalf("the day divides into %d projects, want fastcached and contour-terminal", got)
	}
	fastcached := shareOf(d, derive.StackProject, "fastcached")
	other := shareOf(d, derive.StackProject, "contour-terminal")
	if math.Abs(fastcached-3*other) > 1e-9 {
		t.Errorf("fastcached got $%.6f and contour-terminal $%.6f; want three of the four rows folded",
			fastcached, other)
	}
	// The one that only looks like a worktree keeps its own name.
	if other <= 0 {
		t.Error("contour-terminal was folded away; a project was lost into another")
	}
	if total := fastcached + other; math.Abs(total-d.CostUSD) > 1e-9 {
		t.Errorf("projects sum to $%.6f, want the day's $%.6f", total, d.CostUSD)
	}
}
