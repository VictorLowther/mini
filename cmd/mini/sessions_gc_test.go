package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/session"
)

// gcStore points the store at a temp state dir and plants count sessions in one
// project, each last touched ageDays ago, each with a result directory.
func gcStore(t *testing.T, ageDays []float64) (dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("MINI_STATE_DIR", dir)
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", t.TempDir())
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range ageDays {
		ms := int64(a * 24 * float64(time.Hour))
		id := uuid.NewV7()
		if _, err := st.Create(session.Row{
			ID: id, Kind: session.KindTop, Workdir: "/proj", Project: "/proj", ModelName: "m",
			CreatedAt: time.Now().Add(-time.Duration(ms)).UnixMilli() - 1,
			UpdatedAt: time.Now().Add(-time.Duration(ms)).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(dir, "tcr", id.String())
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "1.log"), make([]byte, 2048+i), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	return dir
}

func countRows(t *testing.T, dir string) int {
	t.Helper()
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Counted through the exported listing rather than a test-only handle on the
	// database: the point is what the store reports as retained, which is what
	// `mini sessions` would show the user.
	rows, err := st.List("", true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// A preview must not delete and must not rewrite the stored policy. Both halves
// matter: the flag is how a user learns what the numbers would do, and the
// numbers are global.
func TestSessionsGCDryRunIsReadOnly(t *testing.T) {
	dir := gcStore(t, []float64{0, 1, 2, 3, 60})
	before := countRows(t, dir)

	var out bytes.Buffer
	if err := runSessionsGC([]string{"-n", "-keep", "2", "-idle-days", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if countRows(t, dir) != before {
		t.Errorf("dry run deleted rows; %d before, %d after", before, countRows(t, dir))
	}
	for _, want := range []string{"Dry run", "/proj", "1 tree, 1 session", "5 days idle"} {
		if !strings.Contains(got, want) && !(want == "/proj" && strings.Contains(got, "proj")) {
			t.Errorf("preview output missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "run only") {
		t.Error("an override without -save must announce that it is not persisted")
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := st.GCConfigFor()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keep != session.DefaultGCKeep || cfg.IdleDays != session.DefaultGCIdleDay {
		t.Errorf("dry run persisted %+v, want the untouched defaults", cfg)
	}
}

// -save is how the knobs move, and the run then collects what the policy says.
func TestSessionsGCSavesAndCollects(t *testing.T) {
	dir := gcStore(t, []float64{0, 1, 2, 3, 60})
	var out bytes.Buffer
	if err := runSessionsGC([]string{"-keep", "2", "-idle-days", "5", "-save"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Saved: keep 2") {
		t.Errorf("no confirmation of the saved knobs: %q", got)
	}
	// keep 2 recent, sweep the 60-day one; the 3-day tree is past keep but fresh.
	if n := countRows(t, dir); n != 4 {
		t.Errorf("rows = %d, want 4 after collecting the one idle tree", n)
	}
	// The swept tree's result directory is gone too.
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dirs, _ := os.ReadDir(filepath.Join(dir, "tcr"))
	if len(dirs) != 4 {
		t.Errorf("result directories left = %d, want 4", len(dirs))
	}
	cfg, _ := st.GCConfigFor()
	if cfg.Keep != 2 || cfg.IdleDays != 5 {
		t.Errorf("stored config = %+v, want keep 2 idle 5", cfg)
	}
}
