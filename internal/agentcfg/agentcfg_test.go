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

// A container or CI run should need no file on disk.
func TestEnvironmentOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := agentcfg.Save(path, agentcfg.Config{
		Mesh: agentcfg.MeshConfig{PSK: "from-the-file"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("AGENTIC_STATS_PSK", "from-the-environment")

	cfg, err := agentcfg.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mesh.PSK != "from-the-environment" {
		t.Errorf("PSK = %q, want the environment value", cfg.Mesh.PSK)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	want := agentcfg.Config{
		Mesh:      agentcfg.MeshConfig{PSK: "a-key", Listen: ":8844", Discovery: true},
		Dashboard: agentcfg.DashboardConfig{Listen: "127.0.0.1:9000"},
		Agent:     agentcfg.AgentConfig{PollInterval: "5m", BatchMaxLines: 42},
		Privacy:   agentcfg.PrivacyConfig{ExcludePaths: []string{"/secret"}},
	}
	if err := agentcfg.Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := agentcfg.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Mesh.PSK != want.Mesh.PSK || got.Agent != want.Agent ||
		got.Dashboard != want.Dashboard {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Privacy.ExcludePaths) != 1 || got.Privacy.ExcludePaths[0] != "/secret" {
		t.Errorf("exclude paths = %v, want [/secret]", got.Privacy.ExcludePaths)
	}
}

// A node's name is optional, and absent is a meaningful value: it means "use
// the hostname", which is what every single-OS machine wants.
func TestMeshNameRoundTrips(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"unset stays unset", ""},
		{"a name survives the round trip", "darkleon-win"},
		{"so does one that differs only by suffix", "darkleon-linux"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := agentcfg.Save(path, agentcfg.Config{
				Mesh: agentcfg.MeshConfig{PSK: "a-key", Name: tt.want},
			}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := agentcfg.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Mesh.Name != tt.want {
				t.Errorf("name = %q, want %q", got.Mesh.Name, tt.want)
			}
		})
	}
}

// The file holds a device token, so its mode is a security property.
func TestSaveWritesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := agentcfg.Save(path, agentcfg.Config{
		Mesh: agentcfg.MeshConfig{PSK: "secret"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertOwnerOnly(t, path)

}

// A fleet where every machine must be told to upgrade does not converge, so
// the default is on -- and that means an omitted key must not read as false.
func TestUpdateIsEnabledUnlessTurnedOff(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"no config file at all", "", true},
		{"a config that never mentions updates", "[mesh]\npsk = \"k\"\n", true},
		{"a section with only an interval", "[update]\ninterval = \"2h\"\n", true},
		{"explicitly on", "[update]\nenabled = true\n", true},
		// The machine somebody is developing on.
		{"explicitly off", "[update]\nenabled = false\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tt.body != "" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			cfg, err := agentcfg.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Update.Enabled != tt.want {
				t.Errorf("Update.Enabled = %v, want %v", cfg.Update.Enabled, tt.want)
			}
		})
	}
}

func TestUpdateIntervalDefaults(t *testing.T) {
	cfg, err := agentcfg.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Update.Interval != "1h" {
		t.Errorf("Update.Interval = %q, want the 1h default", cfg.Update.Interval)
	}
}
