//go:build windows

package service

import "testing"

// The elevated child re-enters the same command, so these arguments have to
// survive being flattened into one string and re-parsed by the shell. A path
// with a space in it is the ordinary case on Windows, not the exotic one:
// %APPDATA% under a user whose name has a space, or anything under Program
// Files. Getting this wrong installs a task pointing at a truncated path, which
// then fails at every login with nothing to explain it.
func TestInstallArgsSurviveSpacesInPaths(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "no paths",
			cfg:  Config{},
			want: `install`,
		},
		{
			name: "plain config path",
			cfg:  Config{ConfigPath: `C:\Users\chris\config.toml`},
			want: `install --config C:\Users\chris\config.toml`,
		},
		{
			name: "config path with a space",
			cfg:  Config{ConfigPath: `C:\Users\Ada Lovelace\config.toml`},
			want: `install --config "C:\Users\Ada Lovelace\config.toml"`,
		},
		{
			name: "config and state, both with spaces",
			cfg: Config{
				ConfigPath: `C:\Program Files\agentic-stats\config.toml`,
				StatePath:  `D:\My Archive\archive.db`,
			},
			want: `install --config "C:\Program Files\agentic-stats\config.toml" ` +
				`--state "D:\My Archive\archive.db"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := installArgs(tc.cfg); got != tc.want {
				t.Errorf("installArgs() =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// Whatever installArgs emits has to be flags runInstall actually accepts, or
// the elevated child exits on an unknown flag and the only symptom is that the
// task never appears.
func TestInstallArgsUseOnlyKnownFlags(t *testing.T) {
	got := installArgs(Config{ConfigPath: `c:\a.toml`, StatePath: `c:\b.db`})
	for _, want := range []string{"install", "--config", "--state"} {
		if !contains(got, want) {
			t.Errorf("installArgs() = %q, missing %q", got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
