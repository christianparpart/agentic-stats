// Package agentcfg loads the collector's configuration.
//
// Nothing machine-specific is compiled in: the server address, the device
// token and any excluded paths all come from configuration, which is what
// keeps the repository free of one user's setup.
package agentcfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the agent's full configuration.
type Config struct {
	Server  ServerConfig  `toml:"server"`
	Agent   AgentConfig   `toml:"agent"`
	Privacy PrivacyConfig `toml:"privacy"`
}

// ServerConfig locates and authenticates against the central server.
type ServerConfig struct {
	// URL is the server root.
	URL string `toml:"url"`
	// Token authenticates this device. Written by `agent enroll`.
	Token string `toml:"token"`
}

// AgentConfig tunes collection.
type AgentConfig struct {
	// PollInterval is how often to look for new data, as a duration string.
	PollInterval string `toml:"poll_interval"`
	// BatchMaxLines bounds records per upload.
	BatchMaxLines int `toml:"batch_max_lines"`
}

// PrivacyConfig withholds data at the source.
type PrivacyConfig struct {
	// ExcludePaths are absolute prefixes that are never read or transmitted.
	ExcludePaths []string `toml:"exclude_paths"`
}

// DefaultPath returns the standard configuration location for this user.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("agentcfg: locate config dir: %w", err)
	}
	return filepath.Join(dir, "agentic-stats", "agent.toml"), nil
}

// Load reads configuration from path, applying environment overrides.
//
// A missing file is not an error: the environment alone is enough to run, which
// keeps container and CI use simple.
func Load(path string) (Config, error) {
	cfg := Config{
		Agent: AgentConfig{PollInterval: "30s", BatchMaxLines: 2000},
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("agentcfg: parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// Fall through to environment-only configuration.
	default:
		return Config{}, fmt.Errorf("agentcfg: read %s: %w", path, err)
	}

	if v := os.Getenv("AGENTIC_STATS_SERVER"); v != "" {
		cfg.Server.URL = v
	}
	if v := os.Getenv("AGENTIC_STATS_TOKEN"); v != "" {
		cfg.Server.Token = v
	}
	return cfg, nil
}

// Save writes configuration to path with owner-only permissions, creating the
// directory if needed. The file holds a device token, so the mode matters.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("agentcfg: create config dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("agentcfg: open %s: %w", path, err)
	}
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		return errors.Join(fmt.Errorf("agentcfg: write %s: %w", path, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("agentcfg: close %s: %w", path, err)
	}
	return nil
}
