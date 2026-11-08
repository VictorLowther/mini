package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ConfigDir returns the directory holding the global .env and the default
// trajectory file: $MINI_GLOBAL_CONFIG_DIR, else the platform user config dir.
func ConfigDir() string {
	if d := os.Getenv("MINI_GLOBAL_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "mini")
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "mini")
		}
		return filepath.Join(home, "AppData", "Roaming", "mini")
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "mini")
		}
		return filepath.Join(home, ".config", "mini")
	}
}

// GlobalConfigFile is the .env inside ConfigDir.
func GlobalConfigFile() string { return filepath.Join(ConfigDir(), ".env") }

// EnsureConfigDir creates ConfigDir if needed and returns its path.
func EnsureConfigDir() (string, error) {
	dir := ConfigDir()
	return dir, os.MkdirAll(dir, 0o700)
}

// StateDir is where cross-session state lives: the session database and the
// tool-call result logs. $MINI_STATE_DIR wins so tests can redirect it.
func StateDir() string {
	if d := os.Getenv("MINI_STATE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".mini")
}

// SessionDB is the global session database inside StateDir.
func SessionDB() string { return filepath.Join(StateDir(), "sessions.db") }

// ToolCallResultsDir is where per-session tool-call result logs live.
func ToolCallResultsDir() string { return filepath.Join(StateDir(), "tcr") }

// EnsureStateDir creates StateDir if needed and returns its path.
func EnsureStateDir() (string, error) {
	dir := StateDir()
	return dir, os.MkdirAll(dir, 0o700)
}

// FirstEnv returns the first non-empty value among the given environment keys.
func FirstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
