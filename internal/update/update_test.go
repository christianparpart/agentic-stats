package update_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/release"
	"github.com/christianparpart/agentic-stats/internal/update"
	"github.com/christianparpart/agentic-stats/internal/version"
)

func TestTargetPicksTheNewestReleaseAPeerReports(t *testing.T) {
	tests := []struct {
		name    string
		current string
		peers   []string
		want    string
	}{
		{"a peer is ahead", "v0.1.0", []string{"v0.2.0"}, "v0.2.0"},
		{"the highest of several", "v0.1.0", []string{"v0.2.0", "v0.9.0", "v0.3.0"}, "v0.9.0"},
		{"ten beats nine", "v0.1.0", []string{"v0.9.0", "v0.10.0"}, "v0.10.0"},

		{"everyone agrees", "v0.2.0", []string{"v0.2.0", "v0.2.0"}, ""},
		{"peers are behind", "v0.9.0", []string{"v0.1.0", "v0.2.0"}, ""},
		{"nobody has said anything", "v0.1.0", nil, ""},
		{"a peer that said nothing", "v0.1.0", []string{""}, ""},

		// An unreleased build is not something another machine could obtain,
		// so it is never a target however high it sorts.
		{"a peer on dev", "v0.1.0", []string{"dev"}, ""},
		{"a peer on a dirty tree", "v0.1.0", []string{"v9.9.9-dirty"}, ""},
		{"a peer ahead of its tag", "v0.1.0", []string{"v9.9.9-5-gabc1234"}, ""},
		{"a release among unreleased ones", "v0.1.0", []string{"dev", "v0.2.0", "v9.9.9-dirty"}, "v0.2.0"},

		// A release outranks an unreleased build, so a node on dev converges
		// onto the fleet rather than holding it back.
		{"running dev, peers on a release", "dev", []string{"v0.1.0"}, "v0.1.0"},
		{"running dev, peers also unreleased", "dev", []string{"dev"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := update.Target(version.Parse(tt.current), tt.peers)
			if ok != (tt.want != "") {
				t.Fatalf("Target ok = %v, want %v (got %q)", ok, tt.want != "", got)
			}
			if got != tt.want {
				t.Errorf("Target = %q, want %q", got, tt.want)
			}
		})
	}
}

// A release must never be a target for a node already on it or past it, or two
// nodes could push each other back and forth forever.
func TestTargetNeverDowngrades(t *testing.T) {
	for _, peer := range []string{"v0.1.0", "v0.0.9", "v1.0.0"} {
		got, ok := update.Target(version.Parse("v1.0.0"), []string{peer})
		if peer == "v1.0.0" || !ok {
			continue
		}
		if version.Parse(got).Compare(version.Parse("v1.0.0")) != version.OrderNewer {
			t.Errorf("Target picked %q while running v1.0.0", got)
		}
	}
}

// --- a fake release, served without a network ---

type fakeRelease struct {
	files map[string][]byte
	fail  map[string]error
	torn  map[string]bool
}

func (f *fakeRelease) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	name := url[strings.LastIndex(url, "/")+1:]
	if err, ok := f.fail[name]; ok {
		return nil, err
	}
	body, ok := f.files[name]
	if !ok {
		return nil, errors.New("404")
	}
	if f.torn[name] {
		return io.NopCloser(&tornReader{body: body}), nil
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

// tornReader delivers some bytes and then fails, like a connection dropped
// partway through a 16 MB download.
type tornReader struct {
	body []byte
	done bool
}

func (r *tornReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("connection reset")
	}
	r.done = true
	n := copy(p, r.body[:len(r.body)/2])
	return n, nil
}

type fakePeers []string

func (f fakePeers) PeerVersions(context.Context) ([]string, error) { return []string(f), nil }

type failingPeers struct{}

func (failingPeers) PeerVersions(context.Context) ([]string, error) {
	return nil, errors.New("archive is locked")
}

type recordingInstaller struct {
	calls  int
	staged map[string]string
	err    error
}

func (r *recordingInstaller) Install(_ context.Context, staged map[string]string) error {
	r.calls++
	r.staged = staged
	return r.err
}

// buildRelease returns a verifier and a fake serving a signed release.
func buildRelease(t *testing.T, tag string, artifacts map[string]string) (*release.Verifier, *fakeRelease) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v, err := release.NewVerifier(pub)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	var manifest strings.Builder
	manifest.WriteString(release.VersionLine + tag + "\n")
	files := map[string][]byte{}
	for name, content := range artifacts {
		sum := sha256.Sum256([]byte(content))
		manifest.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
		files[name] = []byte(content)
	}
	body := []byte(manifest.String())
	files["SHA256SUMS"] = body
	files["SHA256SUMS.sig"] = ed25519.Sign(priv, body)

	return v, &fakeRelease{files: files, fail: map[string]error{}, torn: map[string]bool{}}
}

func newUpdater(t *testing.T, v *release.Verifier, f *fakeRelease, peers []string, inst update.Installer, current string) *update.Updater {
	t.Helper()
	u, err := update.New(update.Config{
		Verifier: v, Fetcher: f, Peers: fakePeers(peers), Installer: inst,
		Current: current, BaseURL: "https://example.invalid/repo",
		StageDir: t.TempDir(), GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u
}

func TestPassInstallsAVerifiedNewerRelease(t *testing.T) {
	v, f := buildRelease(t, "v0.2.0", map[string]string{"agentic-stats-linux-amd64": "the new binary"})
	inst := &recordingInstaller{}
	u := newUpdater(t, v, f, []string{"v0.2.0"}, inst, "v0.1.0")

	outcome, err := u.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if outcome != update.OutcomeInstalled {
		t.Errorf("outcome = %v, want OutcomeInstalled", outcome)
	}
	if inst.calls != 1 {
		t.Errorf("installer called %d times, want 1", inst.calls)
	}
	if _, ok := inst.staged["agentic-stats-linux-amd64"]; !ok {
		t.Errorf("staged = %v, want the linux artifact", inst.staged)
	}
}

func TestPassDoesNothingWhenTheFleetAgrees(t *testing.T) {
	v, f := buildRelease(t, "v0.1.0", map[string]string{"agentic-stats-linux-amd64": "x"})
	inst := &recordingInstaller{}
	u := newUpdater(t, v, f, []string{"v0.1.0"}, inst, "v0.1.0")

	outcome, err := u.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if outcome != update.OutcomeUpToDate {
		t.Errorf("outcome = %v, want OutcomeUpToDate", outcome)
	}
	if inst.calls != 0 {
		t.Error("installed something despite being up to date")
	}
}

// Nothing is installed unless everything verified.
func TestPassRefusesAReleaseItCannotTrust(t *testing.T) {
	const artifact = "agentic-stats-linux-amd64"

	tests := []struct {
		name    string
		corrupt func(*fakeRelease)
		want    string
	}{
		{
			name:    "the artifact is not what the manifest describes",
			corrupt: func(f *fakeRelease) { f.files[artifact] = []byte("something else") },
			want:    "does not match its digest",
		},
		{
			name:    "the signature does not match the manifest",
			corrupt: func(f *fakeRelease) { f.files["SHA256SUMS.sig"][0] ^= 0x01 },
			want:    "not signed by the release key",
		},
		{
			name:    "the manifest names a different release",
			corrupt: func(f *fakeRelease) { f.files["SHA256SUMS"] = []byte("# agentic-stats v9.9.9\n") },
			want:    "not signed by the release key",
		},
		{
			name:    "the manifest cannot be fetched",
			corrupt: func(f *fakeRelease) { f.fail["SHA256SUMS"] = errors.New("no route to host") },
			want:    "fetch SHA256SUMS",
		},
		{
			name:    "the signature cannot be fetched",
			corrupt: func(f *fakeRelease) { f.fail["SHA256SUMS.sig"] = errors.New("no route to host") },
			want:    "fetch SHA256SUMS.sig",
		},
		{
			// Reported as the connection failure it is, rather than as a
			// digest mismatch: the copy fails before the digest is ever
			// checked, and "connection reset" is the actionable half.
			name:    "the download is torn off partway",
			corrupt: func(f *fakeRelease) { f.torn[artifact] = true },
			want:    "connection reset",
		},
		{
			name:    "the release has nothing for this platform",
			corrupt: func(f *fakeRelease) { delete(f.files, artifact) },
			want:    "fetch " + artifact,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, f := buildRelease(t, "v0.2.0", map[string]string{artifact: "the new binary"})
			tt.corrupt(f)
			inst := &recordingInstaller{}
			u := newUpdater(t, v, f, []string{"v0.2.0"}, inst, "v0.1.0")

			outcome, err := u.Pass(context.Background())
			if err == nil {
				t.Fatal("Pass accepted a release it should not trust")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			if outcome != update.OutcomeUpToDate {
				t.Errorf("outcome = %v, want OutcomeUpToDate", outcome)
			}
			if inst.calls != 0 {
				t.Error("installed something from a release that did not verify")
			}
		})
	}
}

// A manifest signed by the right key but naming another release is the
// rollback case: every signature checks out, and it is still the wrong build.
func TestPassRefusesAGenuinelySignedOlderRelease(t *testing.T) {
	// v0.9.0 was asked for; the server answers with a real, correctly signed
	// v0.2.0 manifest instead.
	v, f := buildRelease(t, "v0.2.0", map[string]string{"agentic-stats-linux-amd64": "older binary"})
	inst := &recordingInstaller{}
	u := newUpdater(t, v, f, []string{"v0.9.0"}, inst, "v0.1.0")

	_, err := u.Pass(context.Background())
	if err == nil {
		t.Fatal("Pass accepted a manifest naming a different release")
	}
	if !strings.Contains(err.Error(), "manifest names") {
		t.Errorf("error = %v, want it to name the mismatch", err)
	}
	if inst.calls != 0 {
		t.Error("installed a release other than the one it asked for")
	}
}

// Windows needs both binaries: replacing only the console build would leave
// the service running the old code.
func TestPassStagesBothWindowsBinaries(t *testing.T) {
	v, f := buildRelease(t, "v0.2.0", map[string]string{
		"agentic-stats-windows-amd64.exe":  "console",
		"agentic-stats-windows-amd64w.exe": "windowless",
	})
	inst := &recordingInstaller{}
	u, err := update.New(update.Config{
		Verifier: v, Fetcher: f, Peers: fakePeers{"v0.2.0"}, Installer: inst,
		Current: "v0.1.0", BaseURL: "https://example.invalid/repo",
		StageDir: t.TempDir(), GOOS: "windows", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := u.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(inst.staged) != 2 {
		t.Errorf("staged %d artifacts, want both Windows binaries: %v", len(inst.staged), inst.staged)
	}
	for _, name := range []string{"agentic-stats-windows-amd64.exe", "agentic-stats-windows-amd64w.exe"} {
		if _, ok := inst.staged[name]; !ok {
			t.Errorf("staged is missing %s", name)
		}
	}
}

// A missing GUI twin must stop the whole install, not leave the service on a
// binary that no longer matches the command line beside it.
func TestPassRefusesWindowsWithOnlyOneBinary(t *testing.T) {
	v, f := buildRelease(t, "v0.2.0", map[string]string{
		"agentic-stats-windows-amd64.exe": "console",
	})
	inst := &recordingInstaller{}
	u, err := update.New(update.Config{
		Verifier: v, Fetcher: f, Peers: fakePeers{"v0.2.0"}, Installer: inst,
		Current: "v0.1.0", BaseURL: "https://example.invalid/repo",
		StageDir: t.TempDir(), GOOS: "windows", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := u.Pass(context.Background()); err == nil {
		t.Fatal("Pass installed a Windows release missing its windowless twin")
	}
	if inst.calls != 0 {
		t.Error("installed despite a missing artifact")
	}
}

func TestPassReportsAFailureToReadPeers(t *testing.T) {
	v, f := buildRelease(t, "v0.2.0", map[string]string{"agentic-stats-linux-amd64": "x"})
	inst := &recordingInstaller{}
	u, err := update.New(update.Config{
		Verifier: v, Fetcher: f, Peers: failingPeers{}, Installer: inst,
		Current: "v0.1.0", StageDir: t.TempDir(), GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := u.Pass(context.Background()); err == nil {
		t.Fatal("Pass succeeded despite being unable to read peer versions")
	}
	if inst.calls != 0 {
		t.Error("installed without knowing what peers are running")
	}
}

// An install that fails must not be reported as done, or the daemon would
// restart onto the build it was already running.
func TestPassDoesNotClaimSuccessWhenInstallFails(t *testing.T) {
	v, f := buildRelease(t, "v0.2.0", map[string]string{"agentic-stats-linux-amd64": "x"})
	inst := &recordingInstaller{err: errors.New("permission denied")}
	u := newUpdater(t, v, f, []string{"v0.2.0"}, inst, "v0.1.0")

	outcome, err := u.Pass(context.Background())
	if err == nil {
		t.Fatal("Pass reported success despite the install failing")
	}
	if outcome != update.OutcomeUpToDate {
		t.Errorf("outcome = %v, want OutcomeUpToDate", outcome)
	}
}

func TestRunStopsForARestartOnceInstalled(t *testing.T) {
	v, f := buildRelease(t, "v0.2.0", map[string]string{"agentic-stats-linux-amd64": "x"})
	u := newUpdater(t, v, f, []string{"v0.2.0"}, &recordingInstaller{}, "v0.1.0")

	if err := u.Run(context.Background()); !errors.Is(err, update.ErrRestartPending) {
		t.Errorf("Run = %v, want ErrRestartPending", err)
	}
}

// A node with nothing to do must keep running, not exit.
func TestRunKeepsGoingWhenUpToDate(t *testing.T) {
	v, f := buildRelease(t, "v0.1.0", map[string]string{"agentic-stats-linux-amd64": "x"})
	tick := make(chan time.Time)
	u, err := update.New(update.Config{
		Verifier: v, Fetcher: f, Peers: fakePeers{"v0.1.0"}, Installer: &recordingInstaller{},
		Current: "v0.1.0", StageDir: t.TempDir(), GOOS: "linux", GOARCH: "amd64",
		Tick: tick,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()

	tick <- time.Now() // a second pass, still nothing to do
	cancel()

	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil on cancellation", err)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	v, f := buildRelease(t, "v0.1.0", map[string]string{"a": "b"})
	full := update.Config{Verifier: v, Fetcher: f, Peers: fakePeers{}, Installer: &recordingInstaller{}}

	tests := map[string]func(*update.Config){
		"no verifier":  func(c *update.Config) { c.Verifier = nil },
		"no fetcher":   func(c *update.Config) { c.Fetcher = nil },
		"no peers":     func(c *update.Config) { c.Peers = nil },
		"no installer": func(c *update.Config) { c.Installer = nil },
	}
	for name, drop := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := full
			drop(&cfg)
			if _, err := update.New(cfg); err == nil {
				t.Error("New returned an Updater that cannot work")
			}
		})
	}
}
