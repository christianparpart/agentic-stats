package derive

import (
	"slices"
	"testing"
)

// rules is the default table, which is what a node ships with and therefore the
// only configuration whose behaviour has to be frozen.
func rules(t *testing.T) projectRules {
	t.Helper()
	r, err := newProjectRules(nil)
	if err != nil {
		t.Fatalf("newProjectRules: %v", err)
	}
	return r
}

// The frozen set. These cases are drawn from a real corpus of 76 distinct
// working directories and are written once: a refactor may not quietly change
// how an old archive reads, because the project a day's cost is attributed to
// would change with it.
func TestProjectNamesAreFrozen(t *testing.T) {
	for _, tc := range []struct{ name, cwd, want string }{
		{"a repository root", `D:\fastcached`, "fastcached"},
		{"a posix repository root", "/home/chris/agentic-stats", "agentic-stats"},
		{"a trailing separator", `D:\agentic-stats\`, "agentic-stats"},

		// Claude Code's own layout, which is exact rather than a convention.
		{"a nested agent worktree", `D:\fastcached\.claude\worktrees\agent-acbf97f69e9428729`, "fastcached"},
		{"a nested issue worktree", `D:\fastcached\.claude\worktrees\issue-124`, "fastcached"},
		{"a nested named worktree", `D:\fastcached\.claude\worktrees\next-work-steps-79eded`, "fastcached"},
		{"a nested worktree, posix", "/home/chris/fastcached/.claude/worktrees/issue-124", "fastcached"},
		{"a worktree inside a worktree", `D:\fastcached\.claude\worktrees\a\.claude\worktrees\b`, "fastcached"},

		// Sibling directories, which are a convention.
		{"an issue sibling", `D:\fastcached-issue-154`, "fastcached"},
		{"an issue range sibling", `D:\fastcached-issue-59-69`, "fastcached"},
		{"a wt sibling", `D:\fastcached-wt-139`, "fastcached"},
		{"a bare number sibling", `D:\fastcached-103-prevote`, "fastcached"},
		{"a number-led sibling", `D:\fastcached-97-raft-membership`, "fastcached"},
		{"an underscore sibling", `D:\fastcached_issue_154`, "fastcached"},

		// Names that merely look like siblings, and must survive whole.
		{"a compound project name", `D:\claude-marketplace`, "claude-marketplace"},
		{"a compound name with a word suffix", `D:\contour-terminal`, "contour-terminal"},
		{"a descriptive sibling with no marker", `D:\fastcached-distributed-compilation`, "fastcached-distributed-compilation"},
		{"a digit inside a word", `D:\my-2fa-service`, "my-2fa-service"},
		{"a .claude directory that is not a worktree", `D:\fastcached\.claude\agents`, "agents"},

		// Nothing to attribute to.
		{"a bare windows drive", `D:\`, ""},
		{"a bare drive with no separator", `D:`, ""},
		{"a posix root", "/", ""},
		{"nothing at all", "", ""},

		{"a UNC share", `\\build01\repos\fastcached`, "fastcached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rules(t).of(tc.cwd); got != tc.want {
				t.Errorf("of(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// The fold's worst failure is silent: a wrongly folded project shows a
// plausible name rather than an error, and two real projects merge into one
// segment whose number is simply wrong. So the conservative direction is worth
// its own test.
func TestAKeywordIsNeverItselfAProject(t *testing.T) {
	for _, cwd := range []string{`D:\issue-124`, "/home/chris/pr-120", `D:\wt-139`} {
		if got := rules(t).of(cwd); got == "issue" || got == "pr" || got == "wt" {
			t.Errorf("of(%q) = %q; a suffix keyword is a category, not a project", cwd, got)
		}
	}
}

// The same logical path spelled either way must fold identically, or two
// machines in one mesh draw different project legends from the same archive.
// This is the test that fails the day someone reaches for path/filepath.
func TestProjectNamesAreIndependentOfPathSeparator(t *testing.T) {
	r := rules(t)
	for _, tc := range []struct{ windows, posix string }{
		{`D:\fastcached`, "D:/fastcached"},
		{`D:\fastcached\.claude\worktrees\issue-124`, "D:/fastcached/.claude/worktrees/issue-124"},
		{`home\chris\endo`, "home/chris/endo"},
	} {
		w, p := r.of(tc.windows), r.of(tc.posix)
		if w != p {
			t.Errorf("of(%q) = %q but of(%q) = %q", tc.windows, w, tc.posix, p)
		}
	}
}

// A project name may depend on the path and the pattern table, and on nothing
// else. This forbids re-introducing a set of names learned from the archive,
// which is the change most likely to be attempted here: it would make a node
// that has received half the mesh disagree with one that has received all of it.
func TestProjectFoldingDoesNotDependOnWhatElseTheArchiveHolds(t *testing.T) {
	r := rules(t)
	alone := r.of(`D:\fastcached-wt-139`)
	for _, other := range []string{`D:\fastcached`, `D:\fastcached-wt`, `D:\endo`, ""} {
		_ = r.of(other)
		if got := r.of(`D:\fastcached-wt-139`); got != alone {
			t.Fatalf("after seeing %q the answer changed to %q, was %q", other, got, alone)
		}
	}
	if alone != "fastcached" {
		t.Errorf("of(worktree) = %q in isolation, want fastcached", alone)
	}
}

// Folding an already-folded name must be a no-op, or a chain of worktree
// directories would land in as many segments as it has links.
func TestFoldingIsAFixedPoint(t *testing.T) {
	r := rules(t)
	for _, cwd := range []string{
		`D:\fastcached`, `D:\fastcached-wt-139`, `D:\fastcached-issue-59-69`,
		`D:\fastcached\.claude\worktrees\issue-124`, `D:\claude-marketplace`,
	} {
		once := r.of(cwd)
		if twice := r.of(once); twice != once {
			t.Errorf("of(%q) = %q, but folding that again gives %q", cwd, once, twice)
		}
	}
}

// A constructed object is a usable object: a pattern that does not compile must
// fail construction rather than silently attributing work to the wrong project.
func TestABadSuffixPatternIsAConstructionError(t *testing.T) {
	if _, err := newProjectRules([]string{`^[0-9`}); err == nil {
		t.Error("an unparseable suffix pattern was accepted")
	}
}

// Supplying patterns replaces the defaults rather than adding to them, so a
// default that turns out to be wrong can be removed.
func TestSuppliedPatternsReplaceTheDefaults(t *testing.T) {
	r, err := newProjectRules([]string{`^only-this$`})
	if err != nil {
		t.Fatalf("newProjectRules: %v", err)
	}
	if got := r.of(`D:\fastcached-issue-154`); got != "fastcached-issue-154" {
		t.Errorf("of(issue sibling) = %q; the default patterns should be gone", got)
	}
	if got := r.of(`D:\fastcached-only-this`); got != "fastcached" {
		t.Errorf("of(supplied pattern) = %q, want fastcached", got)
	}
}

func TestPathSegmentsDropsWhatIsNotAName(t *testing.T) {
	got := pathSegments(`D:\\fastcached\.\internal\`)
	want := []string{"D:", "fastcached", "internal"}
	if !slices.Equal(got, want) {
		t.Errorf("pathSegments = %v, want %v", got, want)
	}
}
