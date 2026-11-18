package host

import (
	"fmt"
	"os"
	"testing"
)

// TestMain parks the global config directory in a temp dir for the whole package
// run. The state directory was already redirected per test by newProcess, but the
// *config* file was not, and one test writes it with os.WriteFile - which
// truncates. Pointed at the real $HOME, that deleted the developer's saved
// MINI_MODEL_NAME / MINI_BASE_URL / MINI_CONFIGURED on every `go test ./host/`,
// so the CLI forgot its configuration and re-ran the first-run wizard each start.
//
// This is the package-level guard rather than a per-test line, because the
// failure mode is invisible from inside the test: it passes either way, and the
// only symptom is someone else's editor session losing its settings.
//
// A test that needs to observe an unset config dir can still t.Setenv it;
// t.Setenv's value wins and is restored afterwards.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mini-host-test-config")
	if err != nil {
		fmt.Fprintln(os.Stderr, "host tests: could not create a temp config dir:", err)
		os.Exit(1)
	}
	if os.Getenv("MINI_GLOBAL_CONFIG_DIR") == "" {
		_ = os.Setenv("MINI_GLOBAL_CONFIG_DIR", dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
