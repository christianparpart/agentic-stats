package claudecode_test

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/source/claudecode"
	"github.com/christianparpart/agentic-stats/internal/source/sourcetest"
)

// projects is where the three fixture projects live.
var projects = home + "/.claude/projects"

// discoverWith reports which of the fixture projects survived an exclusion.
func discoverWith(t *testing.T, exclude string) []string {
	t.Helper()
	m := sourcetest.NewMapFS()
	// -work and -work-secret are siblings whose names share a prefix. That is
	// the shape a raw string comparison gets wrong.
	for _, p := range []string{"-work", "-work-secret", "-open"} {
		m.Put(projects+"/"+p+"/session.jsonl", "{}\n")
	}

	src, err := claudecode.New(claudecode.Config{
		FS:              m,
		Home:            home,
		Platform:        "linux",
		ExcludePrefixes: []string{exclude},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	streams, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	var found []string
	for _, p := range discoveredPaths(t, streams) {
		found = append(found, filepath.Base(filepath.Dir(p)))
	}
	return found
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// An exclusion must hold whatever form it was written in.
//
// The configured prefix is typed into a TOML file by hand while the walker
// builds its paths with filepath, so on Windows the two disagree about the
// separator. The raw string comparison this replaces therefore excluded nothing
// at all there: a project the user had asked to withhold was collected and
// replicated to every peer in the mesh.
func TestExclusionHoldsWhateverFormItIsWrittenIn(t *testing.T) {
	native := filepath.Clean(filepath.FromSlash(projects + "/-work-secret"))

	tests := []struct {
		name    string
		exclude string
		// windowsOnly marks a form that can only match where paths are
		// case-insensitive.
		windowsOnly bool
	}{
		{name: "native separators", exclude: native},
		{name: "forward slashes", exclude: projects + "/-work-secret"},
		{name: "trailing separator", exclude: native + string(filepath.Separator)},
		{name: "unclean path", exclude: projects + "/./-work-secret"},
		{name: "different case", exclude: strings.ToUpper(native), windowsOnly: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.windowsOnly && runtime.GOOS != "windows" {
				t.Skip("paths are case-sensitive on this platform")
			}
			found := discoverWith(t, tc.exclude)
			if contains(found, "-work-secret") {
				t.Errorf("the excluded project was collected anyway; found %v", found)
			}
			if len(found) != 2 {
				t.Errorf("collected %v, want the two projects that were not excluded", found)
			}
		})
	}
}

// Excluding a directory must not withhold its siblings.
//
// A raw string prefix makes /home/me/work exclude /home/me/workspace too, which
// is silent data loss: the user asked to withhold one project and quietly lost
// another. Under-exclusion is a privacy failure and over-exclusion is data loss,
// so the match has to land on a path boundary.
func TestExclusionDoesNotSwallowASiblingWithASharedPrefix(t *testing.T) {
	found := discoverWith(t, projects+"/-work")

	if contains(found, "-work") {
		t.Error("the excluded project was collected anyway")
	}
	if !contains(found, "-work-secret") {
		t.Error("excluding -work also withheld -work-secret, which was not asked for")
	}
	if !contains(found, "-open") {
		t.Error("an unrelated project was withheld")
	}
}

// Excluding a whole tree must take everything under it, not just its own name.
func TestExcludingAParentTakesEverythingBeneathIt(t *testing.T) {
	if found := discoverWith(t, projects); len(found) != 0 {
		t.Errorf("collected %v after excluding the whole projects tree", found)
	}
}

// An empty exclusion is not an exclusion of everything. Getting this wrong
// would silently stop collection on any machine with a stray blank entry.
func TestAnEmptyExclusionExcludesNothing(t *testing.T) {
	if found := discoverWith(t, ""); len(found) != 3 {
		t.Errorf("collected %v with an empty exclusion, want all three", found)
	}
}
