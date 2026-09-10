package derive

import (
	"fmt"
	"regexp"
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

// projectRules turns a working directory into a project name.
//
// It is a pure function of one path string and this table -- no filesystem, no
// hostname, and above all no knowledge of what else the archive holds. That is
// load-bearing twice over. A rule that consulted the local disk could not be
// recomputed from a body collected on another machine, and a rule that learned
// its project names from the records would give two replicas different answers
// while they were still converging: a node holding only fastcached-wt-139 would
// show it as its own project, and a node that had also received fastcached
// would not. Replicas that agree must not look like they disagree, so there is
// nothing here to learn from and nothing to iterate.
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
func (r projectRules) of(cwd string) string {
	segs := r.stripWorktree(pathSegments(cwd))
	if len(segs) == 0 {
		return ""
	}
	name := segs[len(segs)-1]
	if isRoot(name) {
		return ""
	}
	return r.foldSibling(name)
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
