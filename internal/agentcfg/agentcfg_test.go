package agentcfg_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/agentcfg"
)

func TestLoadAppliesDefaultsWhenFileIsAbsent(t *testing.T) {
	cfg, err := agentcfg.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load of a missing file must not fail: %v", err)
	}
	if cfg.Agent.PollInterval != "30s" {
		t.Errorf("poll interval = %q, want the 30s default", cfg.Agent.PollInterval)
	}
	if cfg.Agent.BatchMaxLines != 2000 {
		t.Errorf("batch size = %d, want the 2000 default", cfg.Agent.BatchMaxLines)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := agentcfg.Save(path, agentcfg.Config{
		Server: agentcfg.ServerConfig{URL: "https://from-file.invalid", Token: "file-token"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("AGENTIC_STATS_SERVER", "https://from-env.invalid")
	t.Setenv("AGENTIC_STATS_TOKEN", "env-token")

	cfg, err := agentcfg.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.URL != "https://from-env.invalid" {
		t.Errorf("URL = %q, want the environment value", cfg.Server.URL)
	}
	if cfg.Server.Token != "env-token" {
		t.Errorf("token = %q, want the environment value", cfg.Server.Token)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agent.toml")
	want := agentcfg.Config{
		Server:  agentcfg.ServerConfig{URL: "https://example.invalid", Token: "t"},
		Agent:   agentcfg.AgentConfig{PollInterval: "5m", BatchMaxLines: 42},
		Privacy: agentcfg.PrivacyConfig{ExcludePaths: []string{"/secret"}},
	}
	if err := agentcfg.Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := agentcfg.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Server != want.Server || got.Agent != want.Agent {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Privacy.ExcludePaths) != 1 || got.Privacy.ExcludePaths[0] != "/secret" {
		t.Errorf("exclude paths = %v, want [/secret]", got.Privacy.ExcludePaths)
	}
}

// The file holds a device token, so its mode is a security property.
func TestSaveWritesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := agentcfg.Save(path, agentcfg.Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %#o, want 0600: it holds a device token", perm)
	}
}
