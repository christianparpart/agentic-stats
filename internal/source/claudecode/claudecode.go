// Package claudecode discovers Claude Code and Claude Desktop transcript
// streams.
//
// It locates files; it does not interpret them. What a line means is decided
// server-side in internal/derive, so that collection survives a format change.
package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/christianparpart/agentic-stats/internal/source"
)

// Name is the adapter's stable identifier, recorded on every collected line.
const Name = "claudecode"

// rootSpec describes one place transcripts may live.
//
// Roots are a table rather than branching logic: supporting a new install
// layout means adding a row (AGENT.md: data-driven design).
type rootSpec struct {
	// Description explains what the root holds, for diagnostics.
	Description string
	// Relative is the path relative to the user's home directory.
	Relative string
	// Platform limits the root to one GOOS, or is empty for all.
	Platform string
}

// knownRoots enumerates every location transcripts are known to appear.
//
// The Claude Desktop entries matter disproportionately: agent-mode sessions
// write a complete, token-bearing transcript tree nested several levels deep
// under Application Support, which a scraper aimed only at ~/.claude misses
// entirely.
var knownRoots = []rootSpec{
	{
		Description: "Claude Code sessions",
		Relative:    ".claude/projects",
	},
	{
		Description: "archived Claude Code sessions from a previous install",
		Relative:    ".claude.old/projects",
	},
	{
		Description: "global prompt history, which outlives deleted transcripts",
		Relative:    ".claude",
	},
	{
		Description: "Claude Desktop agent-mode sessions",
		Relative:    "Library/Application Support/Claude/local-agent-mode-sessions",
		Platform:    "darwin",
	},
	{
		Description: "Claude Desktop agent-mode sessions",
		Relative:    ".config/Claude/local-agent-mode-sessions",
		Platform:    "linux",
	},
	{
		Description: "Claude Desktop agent-mode sessions",
		Relative:    "AppData/Roaming/Claude/local-agent-mode-sessions",
		Platform:    "windows",
	},
}

// Config is everything the adapter needs. A constructed Source is usable
// (AGENT.md: configuration at construction time).
type Config struct {
	// FS supplies filesystem access. Required.
	FS source.FileSystem
	// Home is the user's home directory. Required.
	Home string
	// Platform is the GOOS to select roots for. Required.
	Platform string
	// Roots overrides the built-in root list entirely, for testing or for
	// unusual installs. Paths are used as given.
	Roots []string
	// ExcludePrefixes are absolute path prefixes that must never be read.
	ExcludePrefixes []string
	// MaxBytesPerRead bounds a single read of one stream. Zero selects a default.
	MaxBytesPerRead int64
}

// Source discovers Claude transcript streams.
type Source struct {
	fsys            source.FileSystem
	roots           []string
	excludePrefixes []string
	maxBytesPerRead int64
}

// New returns a Source that searches the roots implied by cfg.
func New(cfg Config) (*Source, error) {
	if cfg.FS == nil {
		return nil, errors.New("claudecode: Config.FS is required")
	}
	roots := cfg.Roots
	if len(roots) == 0 {
		if cfg.Home == "" {
			return nil, errors.New("claudecode: Config.Home is required when Roots is empty")
		}
		if cfg.Platform == "" {
			return nil, errors.New("claudecode: Config.Platform is required when Roots is empty")
		}
		roots = defaultRoots(cfg.Home, cfg.Platform)
	}
	prefixes := make([]string, 0, len(cfg.ExcludePrefixes))
	for _, p := range cfg.ExcludePrefixes {
		if p = normalizePath(p); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	return &Source{
		fsys:            cfg.FS,
		roots:           roots,
		excludePrefixes: prefixes,
		maxBytesPerRead: cfg.MaxBytesPerRead,
	}, nil
}

// normalizePath puts a path in the one form exclusion comparisons use.
//
// Separators are unified and the path is cleaned, because the two sides of the
// comparison come from different places: the walker builds paths with filepath
// while an exclusion is typed into a TOML file by hand, and on Windows those
// disagree about the separator. Case is folded there too, since Windows paths
// are case-insensitive and an exclusion that fails on capitalisation is a
// privacy hole rather than an inconvenience.
func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(filepath.FromSlash(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// defaultRoots resolves the built-in root table for one platform.
func defaultRoots(home, platform string) []string {
	var out []string
	for _, spec := range knownRoots {
		if spec.Platform != "" && spec.Platform != platform {
			continue
		}
		out = append(out, filepath.Join(home, filepath.FromSlash(spec.Relative)))
	}
	return out
}

// Name implements source.Source.
func (s *Source) Name() string { return Name }

// Discover implements source.Source. It walks every configured root and
// returns a stream per transcript file found, including subagent sidechains
// and the nested trees Claude Desktop writes.
//
// A root that does not exist is not an error: most machines have only some of
// them.
func (s *Source) Discover(ctx context.Context) ([]source.Stream, error) {
	seen := make(map[string]struct{})
	var streams []source.Stream

	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := s.fsys.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A missing or unreadable root or subtree is expected; skip it
				// rather than abandoning discovery of everything else.
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if s.excluded(path) {
					return fs.SkipDir
				}
				return nil
			}
			if !isTranscript(path) || s.excluded(path) {
				return nil
			}
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}

			stream, serr := s.newStream(path)
			if serr != nil {
				return serr
			}
			streams = append(streams, stream)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("claudecode: walk %s: %w", root, err)
		}
	}
	return streams, nil
}

// newStream builds a tailer for one transcript file.
func (s *Source) newStream(path string) (source.Stream, error) {
	identity := source.FileIdentity{}
	if info, err := s.fsys.Stat(path); err == nil {
		identity = info.Identity
	}
	return source.NewFileStream(source.FileStreamConfig{
		ID: source.StreamID{
			Source:   Name,
			Path:     path,
			Identity: identity,
		},
		FS:              s.fsys,
		MaxBytesPerRead: s.maxBytesPerRead,
	})
}

// excluded reports whether a path falls under a configured exclusion.
//
// Both sides are normalized first, and the match is on a path boundary rather
// than on raw characters. The previous string-prefix test was wrong twice over:
// it silently excluded nothing on Windows, where the walker produces
// backslashes and a configured prefix usually has forward slashes, so a project
// the user had asked to withhold was collected and replicated to every peer;
// and everywhere it over-matched, so excluding /home/me/work also withheld
// /home/me/workspace. Under-exclusion is a privacy failure and over-exclusion
// is silent data loss, so neither is acceptable.
func (s *Source) excluded(path string) bool {
	path = normalizePath(path)
	for _, prefix := range s.excludePrefixes {
		if path == prefix {
			return true
		}
		if strings.HasPrefix(path, prefix) && isPathBoundary(path[len(prefix):]) {
			return true
		}
	}
	return false
}

// isPathBoundary reports whether what follows a prefix begins a new segment.
func isPathBoundary(rest string) bool {
	return rest != "" && os.IsPathSeparator(rest[0])
}

// isTranscript reports whether a path is an append-only transcript stream.
//
// Only .jsonl files qualify. The sibling .json files (session metadata, plan
// usage history) are rewritten wholesale rather than appended to, so they are
// not streams and are collected separately.
func isTranscript(path string) bool {
	return filepath.Ext(path) == ".jsonl"
}
