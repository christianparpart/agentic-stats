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
			if got := rules(t).of(tc.cwd, nil); got != tc.want {
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
		if got := rules(t).of(cwd, nil); got == "issue" || got == "pr" || got == "wt" {
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
		w, p := r.of(tc.windows, nil), r.of(tc.posix, nil)
		if w != p {
			t.Errorf("of(%q) = %q but of(%q) = %q", tc.windows, w, tc.posix, p)
		}
	}
}

// The single-path fold answers from the path and the pattern table alone.
//
// resolve deliberately does more -- containment cannot be decided one path at a
// time -- but the *name* rules must never start inferring from similarity. A
// rule that folded `fastcached-wt-139` because some other directory happened to
// be called `fastcached` would be reading a coincidence rather than a fact
// about a path, and it is the change most likely to be attempted here.
func TestTheSinglePathFoldReadsOnlyThePath(t *testing.T) {
	r := rules(t)
	alone := r.of(`D:\fastcached-wt-139`, nil)
	for _, other := range []string{`D:\fastcached`, `D:\fastcached-wt`, `D:\endo`, ""} {
		_ = r.of(other, nil)
		if got := r.of(`D:\fastcached-wt-139`, nil); got != alone {
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
		once := r.of(cwd, nil)
		if twice := r.of(once, nil); twice != once {
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
	if got := r.of(`D:\fastcached-issue-154`, nil); got != "fastcached-issue-154" {
		t.Errorf("of(issue sibling) = %q; the default patterns should be gone", got)
	}
	if got := r.of(`D:\fastcached-only-this`, nil); got != "fastcached" {
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

// A pull request is evidence, not a hint: the assistant recorded the repository
// it opened one against, so a directory whose sessions shipped there is a
// checkout of it whatever the directory is called. This is what no naming rule
// can establish, and every case below is drawn from a real archive.
func TestADirectoryFoldsOntoTheRepositoryItShippedTo(t *testing.T) {
	const fastcached = "LASTRADA-Software/fastcached"
	for _, tc := range []struct {
		name, cwd string
		shipped   []string
		want      string
	}{
		{
			// The case a naming rule cannot reach: a worktree with no issue
			// number and no marker, indistinguishable by name from a project of
			// its own.
			name:    "a descriptively named worktree",
			cwd:     `D:\fastcached-distributed-compilation`,
			shipped: []string{fastcached},
			want:    "fastcached",
		},
		{
			// cwd is not always a repository root, which is the other half of
			// what this fixes: a build tree would otherwise become a project.
			name:    "a build directory inside the repository",
			cwd:     `D:\fastcached\out\build\cl-release`,
			shipped: []string{fastcached},
			want:    "fastcached",
		},
		{
			name:    "a directory several levels down",
			cwd:     `D:\endo\build\clangcl-release\_CPack_Packages\win64\WIX`,
			shipped: []string{"contour-terminal/endo"},
			want:    "endo",
		},
		{
			name:    "a plugin directory inside its marketplace",
			cwd:     `D:\claude-marketplace\plugins\contour-workflows\lib`,
			shipped: []string{"contour-terminal/claude-marketplace"},
			want:    "claude-marketplace",
		},
		{
			// A forge that nests groups: this is one repository called
			// lastrada, not one called developer/lastrada.
			name:    "a nested group path",
			cwd:     `D:\Lastrada`,
			shipped: []string{"jpsc/developer/lastrada"},
			want:    "Lastrada",
		},
		{
			name:    "the repository root itself is unchanged",
			cwd:     `D:\fastcached`,
			shipped: []string{fastcached},
			want:    "fastcached",
		},
		{
			// Working in one repository and opening a pull request against
			// another is ordinary. The first must not be re-labelled.
			name:    "a pull request to somewhere the path never mentions",
			cwd:     `D:\Domestique`,
			shipped: []string{fastcached},
			want:    "Domestique",
		},
		{
			// Several answers is no answer. Falls through to the name rules,
			// which get this one right anyway.
			name:    "sessions that shipped to several repositories",
			cwd:     `D:\fastcached`,
			shipped: []string{fastcached, "contour-terminal/contour", "LASTRADA-Software/morph"},
			want:    "fastcached",
		},
		{
			// Several repositories, but only one of them is named by the path,
			// so the path settles it and the fold is unambiguous.
			name:    "only one of several repositories is on the path",
			cwd:     `D:\scratch\shared-checkout`,
			shipped: []string{"a/shared", "b/checkout"},
			want:    "shared",
		},
		{
			// Two repositories that the path names equally well. There is no
			// answer, so the fold declines and the name rules take over.
			name:    "two repositories the path names equally",
			cwd:     `D:\shared\checkout`,
			shipped: []string{"a/shared", "b/checkout"},
			want:    "checkout",
		},
		{
			name:    "no pull request at all falls back to the name rules",
			cwd:     `D:\fastcached-issue-154`,
			shipped: nil,
			want:    "fastcached",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rules(t).of(tc.cwd, tc.shipped); got != tc.want {
				t.Errorf("of(%q, %v) = %q, want %q", tc.cwd, tc.shipped, got, tc.want)
			}
		})
	}
}

// The guard that makes the rule safe to apply at all: it may shorten a path to
// something the path already says, and never rename it to something else. A
// regression here would silently move a project's cost under another project's
// name, which reads as plausible and is wrong.
func TestTheShippedFoldOnlyEverShortensThePath(t *testing.T) {
	r := rules(t)
	for _, cwd := range []string{
		`D:\Domestique`, `D:\contour-terminal`, `D:\agentic-stats`,
		`D:\some\deep\unrelated\place`,
	} {
		plain := r.of(cwd, nil)
		withEvidence := r.of(cwd, []string{"owner/fastcached", "owner/entirely-elsewhere"})
		if withEvidence != plain {
			t.Errorf("of(%q) became %q on evidence naming neither; it was %q",
				cwd, withEvidence, plain)
		}
	}
}

// The order repositories arrive in must not decide the answer, or two replicas
// holding the same records could label the same directory differently.
func TestTheShippedFoldDoesNotDependOnRepositoryOrder(t *testing.T) {
	r := rules(t)
	cwd := `D:\fastcached\out\build\cl-release`
	forward := r.of(cwd, []string{"a/fastcached", "b/other", "c/another"})
	reverse := r.of(cwd, []string{"c/another", "b/other", "a/fastcached"})
	if forward != reverse {
		t.Errorf("order changed the answer: %q against %q", forward, reverse)
	}
	if forward != "fastcached" {
		t.Errorf("of = %q, want fastcached", forward)
	}
}

func TestRepoNameDropsEveryQualifier(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"owner/repo", "repo"},
		{"group/subgroup/repo", "repo"},
		{"repo", "repo"},
		{"", ""},
	} {
		if got := repoName(tc.in); got != tc.want {
			t.Errorf("repoName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// resolveWith is resolve over a set of directories with no pull requests, so a
// case exercises containment and the name rules alone.
func resolveWith(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	return rules(t).resolve(dirs, nil)
}

// A working directory is not always a repository root. A build tree inside one
// is not a project, and without containment it becomes a project named after
// the build configuration -- `cl-release`, `win64-cl-ninja-release`, `WIX`.
//
// No list of directory names fixes this. `out`, `build` and `target` would miss
// `src\apps\...`, and a list long enough to catch that would eventually swallow
// a project genuinely called `src`. Being inside a project is the property that
// matters.
func TestADirectoryInsideAProjectBelongsToIt(t *testing.T) {
	got := resolveWith(t,
		`D:\Lastrada`,
		`D:\Lastrada\out\build\win64-cl-ninja-release`,
		`D:\fastcached`,
		`D:\fastcached\out\build\cl-release`,
		`D:\fastcached\src\apps\compile-cache-testclient`,
		`D:\endo`,
		`D:\endo\build\clangcl-release\_CPack_Packages\win64\WIX`,
	)
	for dir, want := range map[string]string{
		`D:\Lastrada`: "Lastrada",
		`D:\Lastrada\out\build\win64-cl-ninja-release`: "Lastrada",
		`D:\fastcached`:                                   "fastcached",
		`D:\fastcached\out\build\cl-release`:              "fastcached",
		`D:\fastcached\src\apps\compile-cache-testclient`: "fastcached",
		`D:\endo`: "endo",
		`D:\endo\build\clangcl-release\_CPack_Packages\win64\WIX`: "endo",
	} {
		if got[dir] != want {
			t.Errorf("%s -> %q, want %q", dir, got[dir], want)
		}
	}
}

// A filesystem root contains everything and is not a project. `D:\` is a real
// working directory in a real archive, and folding onto it put every top-level
// project into "no project" -- 883 records of one of them -- when containment
// was first tried without this guard.
func TestAFilesystemRootNeverClaimsWhatIsUnderIt(t *testing.T) {
	got := resolveWith(t, `D:\`, `D:\Domestique`, `D:\tmp`, `/`, "/srv/thing")
	for dir, want := range map[string]string{
		`D:\`:           "",
		`D:\Domestique`: "Domestique",
		`D:\tmp`:        "tmp",
		"/":             "",
		"/srv/thing":    "thing",
	} {
		if got[dir] != want {
			t.Errorf("%s -> %q, want %q", dir, got[dir], want)
		}
	}
}

// A repository nested inside another that ships on its own account is its own
// project: the pull request is exact and containment must not overrule it.
func TestANestedRepositoryThatShipsSeparatelyKeepsItsProject(t *testing.T) {
	got := rules(t).resolve(
		[]string{`D:\fastcached`, `D:\fastcached\vendor\morph`, `D:\fastcached\out\build\cl-debug`},
		map[string][]string{
			`D:\fastcached`:              {"LASTRADA-Software/fastcached"},
			`D:\fastcached\vendor\morph`: {"LASTRADA-Software/morph"},
		})
	if p := got[`D:\fastcached\vendor\morph`]; p != "morph" {
		t.Errorf("the nested repository resolved to %q, want morph", p)
	}
	// While a directory with no pull request of its own still folds inward.
	if p := got[`D:\fastcached\out\build\cl-debug`]; p != "fastcached" {
		t.Errorf("the build tree resolved to %q, want fastcached", p)
	}
}

// Containment matches whole segments. A sibling whose name merely begins with
// the project's is not inside it.
func TestASiblingIsNotContainedByItsNeighbour(t *testing.T) {
	got := resolveWith(t, `D:\fastcached`, `D:\fastcached-distributed-compilation`)
	if p := got[`D:\fastcached-distributed-compilation`]; p != "fastcached-distributed-compilation" {
		t.Errorf("the sibling resolved to %q by containment; only a pull request may fold it", p)
	}
}

// The answer must not depend on the order the directories arrive in, or two
// nodes holding the same records could label the same directory differently.
func TestResolveDoesNotDependOnDirectoryOrder(t *testing.T) {
	dirs := []string{
		`D:\`, `D:\fastcached`, `D:\fastcached\out\build\cl-release`,
		`D:\fastcached\out`, `D:\Lastrada\out\build\win64-cl-ninja-release`,
		`D:\Lastrada`, `D:\tmp`, `D:\fastcached-issue-154`,
	}
	want := resolveWith(t, dirs...)
	for shift := range dirs {
		rotated := append(append([]string{}, dirs[shift:]...), dirs[:shift]...)
		got := resolveWith(t, rotated...)
		for _, d := range dirs {
			if got[d] != want[d] {
				t.Fatalf("rotating by %d changed %s from %q to %q",
					shift, d, want[d], got[d])
			}
		}
	}
}

// Containment resolves through an intermediate directory rather than stopping
// at the first ancestor it finds, so a chain collapses to the project.
func TestContainmentResolvesThroughAChain(t *testing.T) {
	got := resolveWith(t,
		`D:\fastcached`, `D:\fastcached\out`, `D:\fastcached\out\build`,
		`D:\fastcached\out\build\cl-release\deep\deeper`)
	for dir, want := range map[string]string{
		`D:\fastcached\out`:                              "fastcached",
		`D:\fastcached\out\build`:                        "fastcached",
		`D:\fastcached\out\build\cl-release\deep\deeper`: "fastcached",
	} {
		if got[dir] != want {
			t.Errorf("%s -> %q, want %q", dir, got[dir], want)
		}
	}
}
