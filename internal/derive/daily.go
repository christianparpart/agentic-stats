package derive

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/christianparpart/agentic-stats/internal/pricing"
)

// gridRow is one cell of dailyUsageQuery: a day's usage of one model, in one
// session, on one machine.
type gridRow struct {
	day      string
	model    string
	origin   string
	session  string
	cwd      string
	branch   string
	requests int64
	output   int64
	usage    pricing.Usage
}

// share is one segment key and the fraction of a row that belongs to it.
type share struct {
	key    string
	weight float64
}

// links is what the archive knows about where a session's work went.
type links struct {
	// pullRequests and repos are per session, deduplicated and sorted, so the
	// even split has a stable divisor and two nodes agree on it.
	pullRequests map[string][]string
	repos        map[string][]string
	// shipped maps a working directory to the repositories the sessions that
	// ran in it opened pull requests against. It is what makes the project fold
	// evidence rather than inference.
	shipped map[string][]string
	// projects maps a working directory to the project it belongs to, folded
	// out of any worktree. Resolved once per distinct directory rather than per
	// row: the grid holds a few thousand rows over a few dozen directories.
	projects map[string]string
	// byProject names each session by the one project it worked in, where it
	// worked in only one.
	byProject map[string]string
}

// names is everything needed to turn a key into something readable.
type names struct {
	peers   map[string]string
	self    string
	project map[string]string // session -> its repository, where there is one
}

// stacking is one way of dividing a day, as data.
//
// The table below is the whole feature: a sixth dimension is a sixth row, not
// an edit to the query, the fold, the ranking, the payload or the chart.
type stacking struct {
	stack Stack
	label string
	// note explains an allocation where the division is not a measurement.
	note string
	// divide returns the segments a row contributes to, with weights summing
	// to one. Weights rather than a single key because a session that shipped
	// to several places did work for all of them, and attributing it whole to
	// each would inflate the total -- the same double-count the requestId fold
	// exists to prevent, arriving by a different route.
	divide func(r gridRow, l links) []share
	// name renders a segment key for a person.
	name func(key string, n names) string
}

// stackings is the ordered set the dashboard offers.
func stackings() []stacking {
	return []stacking{
		{
			stack: StackModel,
			label: "Model",
			divide: func(r gridRow, _ links) []share {
				return []share{{key: r.model, weight: 1}}
			},
			name: func(key string, _ names) string { return key },
		},
		{
			stack: StackProject,
			label: "Project",
			note: "The directory each request ran in, from the record itself. " +
				"Worktrees and build trees fold back into the project they " +
				"belong to -- exactly where a pull request says which " +
				"repository it is, and by convention where only the directory " +
				"name suggests it.",
			divide: func(r gridRow, l links) []share {
				if p := l.projects[r.cwd]; p != "" {
					return []share{{key: p, weight: 1}}
				}
				return []share{{key: NoneKey, weight: 1}}
			},
			name: func(key string, _ names) string { return key },
		},
		{
			stack: StackBranch,
			label: "Branch",
			divide: func(r gridRow, _ links) []share {
				if r.branch == "" {
					return []share{{key: NoneKey, weight: 1}}
				}
				return []share{{key: r.branch, weight: 1}}
			},
			name: func(key string, _ names) string { return key },
		},
		{
			stack: StackPullRequest,
			label: "Pull request",
			note: "Where a session opened several, its cost is split evenly " +
				"between them -- an allocation, not a measurement.",
			divide: func(r gridRow, l links) []share {
				return evenly(l.pullRequests[r.session])
			},
			name: func(key string, _ names) string { return key },
		},
		{
			stack: StackSession,
			label: "Session",
			divide: func(r gridRow, _ links) []share {
				if r.session == "" {
					return []share{{key: NoneKey, weight: 1}}
				}
				return []share{{key: r.session, weight: 1}}
			},
			name: func(key string, n names) string {
				short := shorten(key)
				if p := n.project[key]; p != "" {
					return short + " · " + p
				}
				return short
			},
		},
		{
			stack: StackPeer,
			label: "Machine",
			divide: func(r gridRow, _ links) []share {
				return []share{{key: r.origin, weight: 1}}
			},
			name: func(key string, n names) string {
				if key == n.self {
					return "this machine"
				}
				if name := n.peers[key]; name != "" {
					return name
				}
				// A peer whose addresses have no reverse-DNS name. Its id is
				// still an identity, so say that rather than "unknown" -- two
				// unnamed machines must not read as one.
				return shorten(key)
			},
		},
	}
}

// evenly splits one row between the keys it belongs to, or attributes it to
// nothing when it belongs nowhere.
func evenly(keys []string) []share {
	if len(keys) == 0 {
		return []share{{key: NoneKey, weight: 1}}
	}
	w := 1 / float64(len(keys))
	out := make([]share, 0, len(keys))
	for _, k := range keys {
		out = append(out, share{key: k, weight: w})
	}
	return out
}

// shorten renders an opaque identifier at a length a person can compare.
func shorten(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// Daily returns per-day activity, oldest first, divided every way the
// dashboard can stack it.
//
// One query and one pass: the dimensions are folded from the same grid rather
// than queried separately, so asking for all five costs what asking for one
// used to, and the dashboard can switch between them without another round
// trip.
func (s *Service) Daily(ctx context.Context) (Activity, error) {
	// Empty rather than nil, for the same reason as Summarize: a list-shaped
	// field should never serialise as null.
	out := Activity{Days: []Day{}, Legends: []Legend{}}

	// The grid first: where a session's work went now includes which project
	// its own rows ran in, and the grid is where those rows are.
	grid, days, err := s.dailyGrid(ctx)
	if err != nil {
		return Activity{}, err
	}
	if len(days) == 0 {
		return out, nil
	}

	l, err := s.sessionLinks(ctx, grid)
	if err != nil {
		return Activity{}, err
	}
	n, err := s.segmentNames(ctx, l)
	if err != nil {
		return Activity{}, err
	}

	// Per dimension: what each segment cost on each day, and overall.
	type totals struct {
		perDay map[string]map[string]float64
		range_ map[string]float64
	}
	acc := make(map[Stack]*totals)
	table := stackings()
	for _, st := range table {
		acc[st.stack] = &totals{
			perDay: make(map[string]map[string]float64),
			range_: make(map[string]float64),
		}
	}

	for _, r := range grid {
		cost, priced := s.prices.Cost(r.model, r.usage)
		if !priced {
			// An unpriced model contributes nothing to the day's cost, so it
			// must contribute nothing to the stacks either or they would stop
			// summing to it. Summarize is where an unpriced model is reported.
			continue
		}
		for _, st := range table {
			t := acc[st.stack]
			byKey := t.perDay[r.day]
			if byKey == nil {
				byKey = make(map[string]float64)
				t.perDay[r.day] = byKey
			}
			for _, sh := range st.divide(r, l) {
				byKey[sh.key] += cost * sh.weight
				t.range_[sh.key] += cost * sh.weight
			}
		}
	}

	for _, st := range table {
		t := acc[st.stack]
		segments, keep := rank(t.range_, st, n)
		out.Legends = append(out.Legends, Legend{
			Stack: st.stack, Label: st.label, Note: st.note, Segments: segments,
		})
		for i := range days {
			shares := fold(t.perDay[days[i].Date], segments, keep)
			if len(shares) == 0 {
				continue
			}
			if days[i].Stacks == nil {
				days[i].Stacks = make(map[Stack][]Share, len(table))
			}
			days[i].Stacks[st.stack] = shares
		}
	}

	out.Days = days
	return out, nil
}

// rank orders a dimension's segments by what they cost over the whole range
// and folds everything past MaxSegments into one.
//
// Over the whole range rather than per day, because the segment order decides
// the colour order: ranking each bar separately would give slot three to a
// different session on every bar, which is worse than one colour.
func rank(totals map[string]float64, st stacking, n names) (segments []Segment, keep map[string]struct{}) {
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	// Cost first, then the key, so replicas that hold the same records produce
	// the same legend rather than looking like they disagree.
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})

	keep = make(map[string]struct{}, MaxSegments)
	var other float64
	for i, k := range keys {
		// The catch-alls never take a named slot from a real category: they
		// are folded in below, keeping their own identity if they are large
		// and joining Other if they are not.
		if i < MaxSegments && k != OtherKey {
			keep[k] = struct{}{}
			segments = append(segments, Segment{
				Key: k, Label: label(k, st, n), TotalUSD: totals[k],
			})
			continue
		}
		other += totals[k]
	}
	if other > 0 {
		segments = append(segments, Segment{
			Key: OtherKey, Label: "Other", TotalUSD: other,
		})
	}
	return segments, keep
}

// label renders a segment key, including the two that are not categories.
func label(key string, st stacking, n names) string {
	switch key {
	case NoneKey:
		switch st.stack {
		case StackProject:
			// A record whose line carried no working directory. Distinct from
			// "No pull request": most work opens no pull request, but almost
			// none of it happened nowhere.
			return "No project"
		case StackBranch:
			// An empty branch means unknown -- a detached HEAD, or a directory
			// that is not a repository -- not a branch named HEAD.
			return "No branch"
		case StackPullRequest:
			return "No pull request"
		case StackSession:
			return "No session"
		case StackModel, StackPeer:
			return "Unattributed"
		default:
			return "Unattributed"
		}
	case OtherKey:
		return "Other"
	default:
		return st.name(key, n)
	}
}

// fold reduces one day's totals to shares in legend order, moving anything
// that did not make the legend into Other.
func fold(byKey map[string]float64, segments []Segment, keep map[string]struct{}) []Share {
	if len(byKey) == 0 {
		return nil
	}
	var other float64
	for k, v := range byKey {
		if _, ok := keep[k]; !ok {
			other += v
		}
	}

	shares := make([]Share, 0, len(segments))
	for _, seg := range segments {
		cost := byKey[seg.Key]
		if seg.Key == OtherKey {
			cost = other
		}
		// Omit a segment that contributed nothing to this day: the chart draws
		// what it is given, and a zero-height slice is noise in the payload
		// and an invisible entry in the tooltip.
		if cost == 0 {
			continue
		}
		shares = append(shares, Share{Key: seg.Key, CostUSD: cost})
	}
	return shares
}

// dailyGrid reads the usage grid and the per-day totals in one pass.
func (s *Service) dailyGrid(ctx context.Context) ([]gridRow, []Day, error) {
	rows, err := s.db.SQL().QueryContext(ctx, dailyUsageQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("derive: daily: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	var grid []gridRow
	byDay := make(map[string]*Day)
	var order []string
	for rows.Next() {
		var r gridRow
		if err := rows.Scan(&r.day, &r.model, &r.origin, &r.session,
			&r.cwd, &r.branch,
			&r.requests, &r.output,
			&r.usage.Input, &r.usage.CacheRead,
			&r.usage.CacheWrite5m, &r.usage.CacheWrite1h); err != nil {
			return nil, nil, fmt.Errorf("derive: scan daily row: %w", err)
		}
		r.usage.Output = r.output
		grid = append(grid, r)

		d, ok := byDay[r.day]
		if !ok {
			d = &Day{Date: r.day}
			byDay[r.day] = d
			order = append(order, r.day)
		}
		d.Requests += r.requests
		d.OutputTokens += r.output
		if cost, priced := s.prices.Cost(r.model, r.usage); priced {
			d.CostUSD += cost
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("derive: daily: %w", err)
	}

	days := make([]Day, 0, len(order))
	for _, day := range order {
		days = append(days, *byDay[day])
	}
	return grid, days, nil
}

// sessionLinks gathers everything the archive knows about where each session's
// work went: the pull requests it opened, and the project its records ran in.
func (s *Service) sessionLinks(ctx context.Context, grid []gridRow) (links, error) {
	rows, err := s.db.SQL().QueryContext(ctx, sessionLinksQuery)
	if err != nil {
		return links{}, fmt.Errorf("derive: session links: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	prs := make(map[string]map[string]struct{})
	repos := make(map[string]map[string]struct{})
	for rows.Next() {
		var session, repo string
		var number int64
		if err := rows.Scan(&session, &repo, &number); err != nil {
			return links{}, fmt.Errorf("derive: scan session link: %w", err)
		}
		add(prs, session, repo+"#"+strconv.FormatInt(number, 10))
		add(repos, session, repo)
	}
	if err := rows.Err(); err != nil {
		return links{}, fmt.Errorf("derive: session links: %w", err)
	}
	l := links{pullRequests: sorted(prs), repos: sorted(repos)}
	l.shipped = shippedByDirectory(l.repos, grid)
	l.projects = s.projectsOf(grid, l.shipped)
	l.byProject = sessionProjects(l, grid)
	return l, nil
}

// shippedByDirectory carries what a session shipped over to the directories it
// ran in, so a directory can be folded onto the repository its work went to.
//
// A session that moved between directories -- into a worktree, most often --
// lends its pull requests to both, which is correct: the work went to that
// repository from both places.
func shippedByDirectory(repos map[string][]string, grid []gridRow) map[string][]string {
	byCWD := make(map[string]map[string]struct{})
	for _, r := range grid {
		if r.cwd == "" {
			continue
		}
		for _, repo := range repos[r.session] {
			add(byCWD, r.cwd, repo)
		}
	}
	return sorted(byCWD)
}

func add(m map[string]map[string]struct{}, key, value string) {
	set := m[key]
	if set == nil {
		set = make(map[string]struct{})
		m[key] = set
	}
	set[value] = struct{}{}
}

// sorted flattens the sets into ordered slices, so the even split's divisor
// and the order it is applied in are the same on every node.
func sorted(m map[string]map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(m))
	for key, set := range m {
		list := make([]string, 0, len(set))
		for v := range set {
			list = append(list, v)
		}
		sort.Strings(list)
		out[key] = list
	}
	return out
}

// projectsOf resolves the grid's distinct working directories.
//
// All of them together, not one at a time: a directory that sits inside another
// belongs to the project that contains it, so the answer for one depends on
// which others exist.
func (s *Service) projectsOf(grid []gridRow, shipped map[string][]string) map[string]string {
	seen := make(map[string]struct{}, len(grid))
	dirs := make([]string, 0, len(grid))
	for _, r := range grid {
		if _, done := seen[r.cwd]; done {
			continue
		}
		seen[r.cwd] = struct{}{}
		dirs = append(dirs, r.cwd)
	}
	return s.projects.resolve(dirs, shipped)
}

// segmentNames gathers everything needed to label a segment.
func (s *Service) segmentNames(ctx context.Context, l links) (names, error) {
	peers, err := s.db.PeerNames(ctx)
	if err != nil {
		return names{}, fmt.Errorf("derive: peer names: %w", err)
	}
	return names{
		peers:   peers,
		self:    s.db.OriginID(),
		project: l.byProject,
	}, nil
}

// sessionProjects names each session by the project it worked in.
//
// From the working directory first, which nearly every session has, and from
// the pull request it shipped only as a fallback -- the repository was the only
// answer available before, and it is still the right one for a session whose
// records somehow carry no directory.
//
// Either way, a session showing more than one answer gets none: saying nothing
// beats picking one. That is a test of the set of values rather than of the
// order they arrived in, so two replicas cannot disagree about it.
func sessionProjects(l links, grid []gridRow) map[string]string {
	found := make(map[string]map[string]struct{})
	for _, r := range grid {
		if r.session == "" {
			continue
		}
		if p := l.projects[r.cwd]; p != "" {
			add(found, r.session, p)
		}
	}

	out := make(map[string]string, len(found)+len(l.repos))
	for session, set := range found {
		if len(set) != 1 {
			continue
		}
		for p := range set {
			out[session] = p
		}
	}
	for session, repos := range l.repos {
		if _, named := out[session]; named || len(repos) != 1 {
			continue
		}
		out[session] = repoName(repos[0])
	}
	return out
}
