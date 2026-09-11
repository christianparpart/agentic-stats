// Package update converges a node onto the newest release its peers report.
//
// A node that has been switched off has no way to learn that the mesh moved on
// except by asking it, and peers already say what they are running on every
// exchange. So the decision needs no network call and no upstream service: a
// node with no peers does nothing, which is the right answer for a machine that
// cannot see the fleet.
//
// What it will run is a narrower question than what it will fetch. Only a
// signed release can be a target -- the artifacts are checked against a
// manifest this project signed, with a key no node holds -- and only one
// strictly newer than the build asking.
//
// It only ever upgrades, and that cuts both ways. An unreleased build is never
// a target, so the machine somebody is developing on cannot pull the fleet
// onto its working tree; and an unreleased build is never replaced either,
// because that working tree is ahead of anything published and installing a
// release over it would be a downgrade however the version strings sort.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/christianparpart/agentic-stats/internal/release"
	"github.com/christianparpart/agentic-stats/internal/version"
)

// DefaultInterval is how often a node looks at what its peers are running.
//
// Slow on purpose. The versions it reads are gossiped on every exchange and
// cost nothing to re-read, but acting on them replaces a binary and restarts a
// daemon, and a fleet that converges within the hour is converged.
const DefaultInterval = time.Hour

// DefaultBaseURL is where releases are published.
const DefaultBaseURL = "https://github.com/christianparpart/agentic-stats"

// ErrRestartPending reports that a new build is in place and the node has to
// restart to be running it.
//
// Returned rather than acted on inside a pass, so the decision to take the
// daemon down belongs to the daemon rather than to this package.
//
// Deliberately not prefixed with the package name, unlike the errors around
// it. This one is only ever surfaced through the subsystem the supervisor
// calls "update", which prefixes what it reports -- so the usual prefix would
// print as "update: update: ...", and a daemon's last line before it exits is
// the wrong place to look sloppy.
var ErrRestartPending = errors.New("a newer build is installed and needs a restart")

// Outcome is what one pass did.
//
// OutcomeUpToDate is the zero value because it is the ordinary answer: most
// passes find the fleet already agreeing.
type Outcome uint8

const (
	// OutcomeUpToDate means no peer reported a newer release.
	OutcomeUpToDate Outcome = iota
	// OutcomeInstalled means a newer release was verified and put in place.
	OutcomeInstalled
)

// Fetcher retrieves one file from a release.
//
// An interface rather than an *http.Client so a test can serve a release
// without a network, and so the one place this package reaches outside the
// machine is named and replaceable.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (io.ReadCloser, error)
}

// PeerSource reports the builds peers last said they were running.
type PeerSource interface {
	PeerVersions(ctx context.Context) ([]string, error)
}

// Installer puts verified artifacts in place.
//
// Takes paths rather than readers: every artifact is verified against the
// signed manifest before any of them is installed, so a release that is
// partly corrupt replaces nothing.
type Installer interface {
	// Install moves each staged file into place. staged maps a release
	// artifact name to the path of a verified temporary file.
	Install(ctx context.Context, staged map[string]string) error
}

// Config is everything an Updater needs.
type Config struct {
	// Verifier opens the signed manifest. Required.
	Verifier *release.Verifier
	// Fetcher retrieves release files. Required.
	Fetcher Fetcher
	// Peers reports what the rest of the fleet is running. Required.
	Peers PeerSource
	// Installer puts the new build in place. Required.
	Installer Installer
	// Current is what this build reports itself as. Required.
	Current string
	// BaseURL is the project's release location. DefaultBaseURL when empty.
	BaseURL string
	// StageDir is where artifacts are downloaded before being verified.
	// Empty uses the system temporary directory.
	StageDir string
	// Interval is how often to look. DefaultInterval when zero.
	Interval time.Duration
	// Tick replaces the internal ticker, so a test can drive the schedule
	// without waiting for one.
	Tick <-chan time.Time
	// Logger receives progress. Nil discards it.
	Logger *slog.Logger
	// GOOS and GOARCH name the platform to fetch for. Empty uses this
	// machine's, which is what production wants; a test sets them to check
	// the artifact names for a platform it is not running on.
	GOOS   string
	GOARCH string
}

// Updater keeps one node on the newest release its peers report.
type Updater struct {
	verifier  *release.Verifier
	fetcher   Fetcher
	peers     PeerSource
	installer Installer
	current   version.Version
	baseURL   string
	stageDir  string
	interval  time.Duration
	tick      <-chan time.Time
	log       *slog.Logger
	goos      string
	goarch    string
}

// New returns an Updater ready to run.
func New(cfg Config) (*Updater, error) {
	switch {
	case cfg.Verifier == nil:
		return nil, errors.New("update: a verifier is required")
	case cfg.Fetcher == nil:
		return nil, errors.New("update: a fetcher is required")
	case cfg.Peers == nil:
		return nil, errors.New("update: a peer source is required")
	case cfg.Installer == nil:
		return nil, errors.New("update: an installer is required")
	}

	u := &Updater{
		verifier:  cfg.Verifier,
		fetcher:   cfg.Fetcher,
		peers:     cfg.Peers,
		installer: cfg.Installer,
		current:   version.Parse(cfg.Current),
		baseURL:   cfg.BaseURL,
		stageDir:  cfg.StageDir,
		interval:  cfg.Interval,
		tick:      cfg.Tick,
		log:       cfg.Logger,
		goos:      cfg.GOOS,
		goarch:    cfg.GOARCH,
	}
	if u.baseURL == "" {
		u.baseURL = DefaultBaseURL
	}
	if u.interval <= 0 {
		u.interval = DefaultInterval
	}
	if u.log == nil {
		u.log = slog.New(slog.DiscardHandler)
	}
	if u.goos == "" {
		u.goos = runtime.GOOS
	}
	if u.goarch == "" {
		u.goarch = runtime.GOARCH
	}
	return u, nil
}

// Run looks for a newer release until ctx is done.
//
// Returns ErrRestartPending once a new build is in place, so the caller decides
// what taking the daemon down means. Every other failure is logged and left for
// the next pass: a release that could not be fetched is not a reason to stop
// collecting, and the peers will still be reporting it in an hour.
func (u *Updater) Run(ctx context.Context) error {
	tick := u.tick
	if tick == nil {
		ticker := time.NewTicker(u.interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		switch outcome, err := u.Pass(ctx); {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			u.log.Warn("could not check for a newer build", "error", err)
		case outcome == OutcomeInstalled:
			return ErrRestartPending
		}

		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// Pass looks once, and installs if a peer reports something newer.
func (u *Updater) Pass(ctx context.Context) (Outcome, error) {
	reported, err := u.peers.PeerVersions(ctx)
	if err != nil {
		return OutcomeUpToDate, fmt.Errorf("update: read peer versions: %w", err)
	}
	target, ok := Target(u.current, reported)
	if !ok {
		return OutcomeUpToDate, nil
	}

	u.log.Info("a peer reports a newer build", "running", u.current, "available", target)

	manifest, err := u.manifest(ctx, target)
	if err != nil {
		return OutcomeUpToDate, err
	}

	staged, err := u.stage(ctx, target, manifest)
	// Staged files are temporary whatever happens: on the failure path they
	// are incomplete, and on the success path the installer has moved what it
	// wanted and a copy left behind is just a stale binary in the temp dir.
	defer func() {
		for _, p := range staged {
			_ = os.Remove(p)
		}
	}()
	if err != nil {
		return OutcomeUpToDate, err
	}

	if err := u.installer.Install(ctx, staged); err != nil {
		return OutcomeUpToDate, fmt.Errorf("update: install %s: %w", target, err)
	}
	u.log.Info("installed a newer build", "from", u.current, "to", target)
	return OutcomeInstalled, nil
}

// manifest fetches and verifies the signed manifest for one release.
func (u *Updater) manifest(ctx context.Context, target string) (release.Manifest, error) {
	body, err := u.get(ctx, target, "SHA256SUMS")
	if err != nil {
		return release.Manifest{}, err
	}
	signature, err := u.get(ctx, target, "SHA256SUMS.sig")
	if err != nil {
		return release.Manifest{}, err
	}

	manifest, err := u.verifier.Open(body, signature)
	if err != nil {
		return release.Manifest{}, fmt.Errorf("update: %s: %w", target, err)
	}
	// The signature binds the artifacts to each other; this binds them to the
	// release that was asked for. Without it a genuinely signed older release
	// serves in place of a newer one and every other check still passes.
	if manifest.Version() != target {
		return release.Manifest{}, fmt.Errorf(
			"update: asked for %s but its manifest names %q", target, manifest.Version())
	}
	return manifest, nil
}

// get reads one file of a release into memory. Only ever used for the manifest
// and its signature, which are a kilobyte between them.
func (u *Updater) get(ctx context.Context, target, name string) ([]byte, error) {
	rc, err := u.fetcher.Fetch(ctx, releaseURL(u.baseURL, target, name))
	if err != nil {
		return nil, fmt.Errorf("update: fetch %s: %w", name, err)
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(io.LimitReader(rc, maxMetadataBytes))
	if err != nil {
		return nil, fmt.Errorf("update: read %s: %w", name, err)
	}
	return body, nil
}

// maxMetadataBytes bounds the manifest and its signature. A release of this
// project needs under a kilobyte; anything far larger is not one.
const maxMetadataBytes = 1 << 20

// stage downloads and verifies every artifact this machine needs.
//
// Nothing is installed until all of them verify, so a release that is partly
// corrupt leaves the node exactly as it was.
func (u *Updater) stage(ctx context.Context, target string, manifest release.Manifest) (map[string]string, error) {
	staged := make(map[string]string)
	for _, name := range artifactNames(u.goos, u.goarch) {
		file, err := u.download(ctx, target, name)
		if file != "" {
			staged[name] = file
		}
		if err != nil {
			return staged, err
		}
		if err := verifyFile(manifest, name, file); err != nil {
			return staged, fmt.Errorf("update: %s: %w", target, err)
		}
	}
	if len(staged) == 0 {
		return staged, fmt.Errorf("update: %s publishes nothing for %s/%s", target, u.goos, u.goarch)
	}
	return staged, nil
}

// download streams one artifact to a temporary file beside the others.
func (u *Updater) download(ctx context.Context, target, name string) (string, error) {
	rc, err := u.fetcher.Fetch(ctx, releaseURL(u.baseURL, target, name))
	if err != nil {
		return "", fmt.Errorf("update: fetch %s: %w", name, err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp(u.stageDir, "agentic-stats-update-*")
	if err != nil {
		return "", fmt.Errorf("update: stage %s: %w", name, err)
	}
	staged := f.Name()
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		return staged, fmt.Errorf("update: download %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return staged, fmt.Errorf("update: stage %s: %w", name, err)
	}
	return staged, nil
}

// verifyFile checks a staged artifact against the signed manifest.
func verifyFile(manifest release.Manifest, name, staged string) error {
	f, err := os.Open(staged)
	if err != nil {
		return fmt.Errorf("reopen %s: %w", name, err)
	}
	defer func() { _ = f.Close() }() // read-only; the digest is the verdict
	return manifest.Check(name, f)
}

// Target picks the release to converge on, if there is one.
//
// The highest release any peer reports, and only when it is strictly newer than
// what is running. Anything a peer says that is not a release is ignored rather
// than ranked: an unreleased build is not something another machine could
// obtain even if it wanted to.
func Target(current version.Version, reported []string) (string, bool) {
	// An unreleased build is never replaced.
	//
	// It is a working tree -- the machine the software is being written on --
	// and what is in it is ahead of anything published, not behind it.
	// Installing a release over it would be a downgrade wearing an upgrade's
	// clothes, and "only ever upgrade" has to mean that here too.
	//
	// The same conservatism as internal/version, and for the same reason:
	// where the two cannot be ranked, the answer is to do nothing rather than
	// to guess. A node deliberately given an unreleased build keeps it until
	// someone deliberately takes it away.
	if !current.IsRelease() {
		return "", false
	}

	best := ""
	bestV := version.Version{}
	for _, raw := range reported {
		v := version.Parse(raw)
		if !v.IsRelease() {
			continue
		}
		if v.Compare(current) != version.OrderNewer {
			continue
		}
		if best == "" || v.Compare(bestV) == version.OrderNewer {
			best, bestV = raw, v
		}
	}
	return best, best != ""
}

// artifactNames lists the files this machine needs from a release.
//
// Windows needs two: the console build is the command line and the
// GUI-subsystem twin beside it is what the service runs, and replacing only
// one would leave the service on the old code. The "w" goes before the
// extension because that is where service.windowlessPath looks for it.
func artifactNames(goos, goarch string) []string {
	base := "agentic-stats-" + goos + "-" + goarch
	if goos != "windows" {
		return []string{base}
	}
	return []string{base + ".exe", base + "w.exe"}
}

// ArtifactName is the console build's name for one platform, exported for
// callers that report what they would fetch.
func ArtifactName(goos, goarch string) string {
	return artifactNames(goos, goarch)[0]
}

// releaseURL is where one artifact of one release lives.
//
// Joined by hand rather than with path.Join, which this project reserves for
// nothing and forbids outright, or url.JoinPath, which would escape what does
// not need escaping. Neither is needed: a tag is a parsed version and an
// artifact name is rejected by the manifest parser unless it is a plain file
// name, so there is no separator here to normalise.
func releaseURL(base, target, name string) string {
	return base + "/releases/download/" + target + "/" + name
}
