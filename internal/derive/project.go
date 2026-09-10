package derive

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// separators are the two path separators a stored working directory can use.
//
// Both, always, whatever this machine's own separator is. Neither `path` nor
// `path/filepath` can do this: `filepath` follows the host OS, so splitting
// `D:\fastcached` yields "fastcached" on Windows and the whole string on Linux
// -- and records replicate between the two, so a host-dependent split would
// give two nodes different project legends for the same archive. `path` is
// slash-only and forbidden outright by .golangci.yml. Hand-rolling this is the
// correct answer here rather than a shortcut: do not "fix" it to use filepath.
const separators = "/\\"

// worktreeLayout is a directory convention that nests a worktree inside the
// project it came from: a marker directory followed by a named child.
//
// Claude Code's own layout is <project>/.claude/worktrees/<name>, so the project
// is whatever sits immediately above the marker. This is exact rather than a
// guess -- the tool created the directory -- which is why it is a separate rule
// from the suffix table below.
type worktreeLayout struct {
	marker string
	child  string
}

// defaultWorktreeLayouts is the table of nested-worktree conventions. A second
// assistant that nests its worktrees somewhere else is a row here.
func defaultWorktreeLayouts() []worktreeLayout {
	return []worktreeLayout{
		{marker: ".claude", child: "worktrees"},
	}
}

// DefaultProjectSuffixes marks a sibling directory as a worktree of the project
// its name starts with, so fastcached-issue-154 is work on fastcached.
//
// Unlike the nested layout this is a convention rather than a fact, so the
// patterns are deliberately narrow: the suffix must lead with a complete run of
// digits, or with a word that names an issue, a branch or a worktree. A merely
// hyphenated name is not enough, or every project with a compound name would be
// folded into a shorter one that may not even exist -- contour-terminal would
// become contour, and agentic-stats would become agentic.
//
// Each pattern is matched against the whole suffix, with the separator already
// removed.
func DefaultProjectSuffixes() []string {
	return []string{
		// 97-raft-membership, 139, 59-69
		`^[0-9]+([-_].*)?$`,
		// issue-154, wt-139, pr-120, branch-x
		`^(issue|issues|wt|worktree|pr|branch|agent)([-_].*)?$`,
	}
}

// worktreeWords are the suffix keywords, which may not themselves become a
// project: a directory named issue-124 on its own would otherwise fold to
// "issue", which is a category and not something anyone works on.
var worktreeWords = map[string]struct{}{
	"issue": {}, "issues": {}, "wt": {}, "worktree": {},
	"pr": {}, "branch": {}, "agent": {},
}

// projectRules turns working directories into project names.
//
// Nothing here consults the filesystem or the host's own path separator. That
// is load-bearing: a rule that looked at the local disk could not be applied to
// a directory collected on another machine, and half of them do not exist here.
//
// Two entry points, and the difference matters. `of` is a pure function of one
// path -- name conventions and whatever the pull requests say. `resolve` folds
// a whole set at once, because containment cannot be decided one path at a
// time: whether `D:\p\out\build` is a project depends on whether `D:\p` is one.
//
// So `resolve` does depend on which directories the archive holds, and that is
// a deliberate, bounded departure. The property the legend actually rests on is
// that *replicas holding the same records agree*, and it still holds: the set
// of directories is a function of the records. Two nodes mid-convergence may
// briefly differ and then converge, exactly as the daily report already does
// while re-extraction is still running.
//
// What is still refused is a rule that would learn project *names* from
// similarity -- folding `fastcached-wt-139` because some other directory
// happens to be called `fastcached`. That is inference from a coincidence
// rather than a fact about a path, and it is why the suffix table stays narrow.
type projectRules struct {
	layouts  []worktreeLayout
	suffixes []*regexp.Regexp
}

// newProjectRules compiles the suffix patterns, so a bad one is a construction
// error rather than a wrong chart.
func newProjectRules(suffixes []string) (projectRules, error) {
	if len(suffixes) == 0 {
		suffixes = DefaultProjectSuffixes()
	}
	out := projectRules{layouts: defaultWorktreeLayouts()}
	for _, p := range suffixes {
		re, err := regexp.Compile(p)
		if err != nil {
			return projectRules{}, fmt.Errorf("derive: project suffix %q: %w", p, err)
		}
		out.suffixes = append(out.suffixes, re)
	}
	return out, nil
}

// of returns the project a working directory belongs to, or "" when the path
// names none -- an empty value, or a bare filesystem root, which is somewhere
// work happened but not a project it can be attributed to.
//
// shippedTo is the repositories the sessions that ran in this directory opened
// pull requests against, which is evidence rather than inference and is
// therefore preferred over the name rules below.
func (r projectRules) of(cwd string, shippedTo []string) string {
	segs := r.stripWorktree(pathSegments(cwd))
	if len(segs) == 0 {
		return ""
	}
	if p := shippedProject(segs, shippedTo); p != "" {
		return p
	}
	return r.nameProject(segs)
}

// nameProject is the last resort: what the directory calls itself, with a
// sibling worktree suffix folded away.
func (r projectRules) nameProject(segs []string) string {
	if len(segs) == 0 {
		return ""
	}
	name := segs[len(segs)-1]
	if isRoot(name) {
		return ""
	}
	return r.foldSibling(name)
}

// resolve gives every working directory its project, folding one that sits
// inside another onto the project that contains it.
//
// Containment is the rule that needs no evidence and no naming convention, and
// it is what makes the others' gaps survivable. A working directory is not
// always a repository root -- `D:\fastcached\out\build\cl-release` and
// `D:\Lastrada\out\build\win64-cl-ninja-release` are both real, and without
// this they become projects named after a build tree. There is no list of
// directory names that fixes it: `out`, `build` and `target` would miss
// `src\apps\...` and `plugins\...`, and a stoplist long enough to catch those
// would eventually swallow a project genuinely called `src`. Being *inside* a
// project is the property that actually matters, and the archive knows it.
//
// Only an ancestor that is itself a project may claim a directory. A filesystem
// root contains everything and is not a project, and `D:\` is a working
// directory in a real archive -- folding onto it moved every top-level project
// into "no project" when this was first tried.
//
// Pull-request evidence comes first, so a repository nested inside another that
// ships on its own account stays its own project. Containment only takes over
// where nothing exact is known.
//
// The answer depends on the set of directories, so it depends on the records --
// which is the property the legend already requires: replicas holding the same
// records agree. Two nodes mid-convergence may differ for as long as one is
// missing records, and then converge, like every other report here.
func (r projectRules) resolve(dirs []string, shipped map[string][]string) map[string]string {
	type entry struct {
		dir  string
		segs []string
		key  string
	}
	entries := make([]entry, 0, len(dirs))
	for _, dir := range dirs {
		segs := r.stripWorktree(pathSegments(dir))
		entries = append(entries, entry{dir: dir, segs: segs, key: segmentKey(segs)})
	}
	// Shortest first, so a directory's ancestors are always resolved before it
	// and one pass suffices; then by name, so the order is total and two nodes
	// walk the same directories in the same order.
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].segs) != len(entries[j].segs) {
			return len(entries[i].segs) < len(entries[j].segs)
		}
		return entries[i].dir < entries[j].dir
	})

	out := make(map[string]string, len(dirs))
	projects := make(map[string]string, len(dirs))
	for _, e := range entries {
		project := shippedProject(e.segs, shipped[e.dir])
		if project == "" {
			project = ancestorProject(e.segs, projects)
		}
		if project == "" {
			project = r.nameProject(e.segs)
		}
		out[e.dir] = project
		// Only a directory that is a project can contain one. First writer
		// wins, which the sort above makes deterministic.
		if _, taken := projects[e.key]; !taken && project != "" {
			projects[e.key] = project
		}
	}
	return out
}

// ancestorProject returns the project of the nearest enclosing directory that
// has one, and "" when nothing encloses this path.
func ancestorProject(segs []string, projects map[string]string) string {
	// Nearest first: the innermost enclosing project is the one that owns the
	// directory, which matters once a repository contains another.
	for cut := len(segs) - 1; cut > 0; cut-- {
		if p := projects[segmentKey(segs[:cut])]; p != "" {
			return p
		}
	}
	return ""
}

// segmentKey identifies a path by its segments, for comparing one path against
// another.
//
// Case-folded, because Windows and macOS both treat `D:\Foo` and `d:\foo` as
// one directory and a machine that spells one of them differently must not be
// read as working somewhere else. Joined on a byte that cannot occur in a path
// segment, so `a\b` and `a-b` cannot collide however the segments are split.
func segmentKey(segs []string) string {
	var b strings.Builder
	for i, seg := range segs {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(strings.ToLower(seg))
	}
	return b.String()
}

// shippedProject folds a directory onto the repository its work demonstrably
// went to.
//
// This is the one rule here that is not a guess. The assistant records the
// repository it opened a pull request against, so a directory whose sessions
// shipped to `owner/fastcached` is a checkout of fastcached whatever it happens
// to be called -- which no naming rule can establish. It is what tells
// `fastcached-distributed-compilation` (a worktree, no issue number in sight)
// from `contour-terminal` (a project of its own), and it is what stops a
// build directory deep inside a repository from becoming a project called
// `cl-release`.
//
// Two guards keep it safe:
//
// The repository's name must actually appear in the path, so this can only ever
// shorten a directory, never rename it to something the path does not mention.
// Working in one repository and opening a pull request against another is
// ordinary, and must not silently re-label the first.
//
// Repositories that disagree fold nothing. A directory whose sessions shipped
// to several places has no single answer, and saying nothing beats picking one
// -- the same rule the session label follows.
func shippedProject(segs, shippedTo []string) string {
	var found string
	for _, repo := range shippedTo {
		hit := matchInPath(segs, repoName(repo))
		switch {
		case hit == "":
			continue
		case found == "":
			found = hit
		case found != hit:
			return ""
		}
	}
	return found
}

// matchInPath returns the path's own spelling of name where the path contains
// it, and "" where it does not.
//
// The path's spelling and not the repository's, because they disagree: a
// repository named `lastrada` checked out in `D:\Lastrada` must stay
// `Lastrada`, which is what the person reading the chart called it. Matching
// case-insensitively is what makes that comparison work at all, and is right on
// its own terms -- Windows and macOS both treat those as one directory.
func matchInPath(segs []string, name string) string {
	if name == "" {
		return ""
	}
	// A directory on the path is the repository itself: everything below it,
	// a build tree included, is work on that project.
	for _, seg := range segs {
		if strings.EqualFold(seg, name) {
			return seg
		}
	}
	// Or the last segment is the repository plus a suffix, which is how a
	// sibling worktree is named.
	last := segs[len(segs)-1]
	if len(last) > len(name) && strings.EqualFold(last[:len(name)], name) &&
		(last[len(name)] == '-' || last[len(name)] == '_') {
		return last[:len(name)]
	}
	return ""
}

// repoName is a repository's own name, without the owner that qualifies it.
//
// After the last separator, not the first: a self-hosted forge nests groups, so
// `jpsc/developer/lastrada` is one repository called lastrada and not a
// repository called `developer/lastrada`.
func repoName(repo string) string {
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// stripWorktree drops a nested worktree suffix, leaving the project's own path.
//
// The first match rather than the last, so a worktree created inside a worktree
// resolves to the real project in one step.
func (r projectRules) stripWorktree(segs []string) []string {
	for i := 1; i+1 < len(segs); i++ {
		for _, l := range r.layouts {
			if segs[i] == l.marker && segs[i+1] == l.child {
				return segs[:i]
			}
		}
	}
	return segs
}

// foldSibling folds a name of the form <project><separator><suffix> back to
// <project>.
//
// Cutting at the leftmost separator whose remainder matches, not at the longest
// base, and the difference is not cosmetic: fastcached-issue-105 cut at its last
// separator leaves the base fastcached-issue, whose remainder 105 also matches
// the digit rule -- inventing a fastcached-issue project that never existed.
// Cutting leftmost yields the suffix issue-105 and the base fastcached. Both
// directions are deterministic; only one is right.
func (r projectRules) foldSibling(name string) string {
	for i := 1; i < len(name)-1; i++ {
		if name[i] != '-' && name[i] != '_' {
			continue
		}
		base, suffix := name[:i], name[i+1:]
		if _, reserved := worktreeWords[strings.ToLower(base)]; reserved {
			continue
		}
		for _, re := range r.suffixes {
			if re.MatchString(suffix) {
				return base
			}
		}
	}
	return name
}

// pathSegments splits a path on either separator, dropping the empty segments a
// root, a doubled separator or a UNC prefix leaves behind.
func pathSegments(p string) []string {
	var out []string
	for _, seg := range strings.FieldsFunc(p, isSeparator) {
		// "." adds nothing to a name.
		if seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

func isSeparator(r rune) bool { return strings.ContainsRune(separators, r) }

// isRoot reports whether a segment is a filesystem root rather than a
// directory: a bare Windows drive, which is what D: leaves behind.
func isRoot(seg string) bool {
	if len(seg) != 2 || seg[1] != ':' {
		return false
	}
	c := seg[0] | ('a' - 'A')
	return c >= 'a' && c <= 'z'
}
