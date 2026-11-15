package session

import (
	"path/filepath"
	"testing"

	"github.com/VictorLowther/mini/model"
)

func TestSettingsRoundTrip(t *testing.T) {
	s, dir := openStore(t)
	if _, ok, err := s.Setting("thinking_shown"); err != nil || ok {
		t.Fatalf("unset key = %v, %v; want absent with no error", ok, err)
	}
	for _, kv := range [][2]string{
		{"thinking_shown", "true"},
		{"window_width", "1440.00"},
		{"window_height", "900.00"},
		{"unicode_value", "Ångström 漢字 ✅"},
		{"empty_value", ""},
	} {
		if err := s.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, kv := range [][2]string{
		{"thinking_shown", "true"},
		{"window_width", "1440.00"},
		{"window_height", "900.00"},
		{"unicode_value", "Ångström 漢字 ✅"},
		{"empty_value", ""},
	} {
		got, ok, err := s.Setting(kv[0])
		if err != nil || !ok || got != kv[1] {
			t.Errorf("Setting(%q) = %q, %v, %v; want %q", kv[0], got, ok, err, kv[1])
		}
	}

	// A rewrite replaces; it does not accumulate a second row.
	if err := s.SetSetting("thinking_shown", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Setting("thinking_shown"); got != "false" {
		t.Errorf("after rewrite = %q", got)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM settings`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 5 {
		t.Errorf("%d rows for 5 keys; a rewrite must replace, not accumulate", rows)
	}

	// The settings table is global state: it outlives every session, so
	// deleting all the history must not forget the interface preferences.
	if err := s.DeleteSetting("window_height"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Setting("window_height"); ok {
		t.Error("DeleteSetting left the key readable")
	}
	if err := s.DeleteSetting("never_written"); err != nil {
		t.Errorf("deleting an unknown key is a no-op, not an error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// And it survives reopening the file, which is the entire feature.
	reopened, err := Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got, ok, err := reopened.Setting("thinking_shown"); err != nil || !ok || got != "false" {
		t.Errorf("after reopen = %q, %v, %v", got, ok, err)
	}
}

// TestSettingsSurviveSessionDelete: the sticky half is not history, so
// `mini sessions` cleanup cannot take it with it.
func TestSettingsSurviveSessionDelete(t *testing.T) {
	s, _ := openStore(t)
	sess := newSession(t, s)
	if err := s.SetSetting("window_width", "1024"); err != nil {
		t.Fatal(err)
	}
	record(t, sess, model.Message{"role": "user", "content": "hi"})
	if err := s.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Setting("window_width"); err != nil || !ok || got != "1024" {
		t.Errorf("deleting the session took the settings with it: %q %v %v", got, ok, err)
	}
}

// TestSettingsOnAnExistingDatabase: a store created before the settings table
// existed gains it on the next Open, which is what a CREATE IF NOT EXISTS
// migration is for at this version.
func TestSettingsOnAnExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	first, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetSetting("thinking_shown", "true"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	if got, ok, err := second.Setting("thinking_shown"); err != nil || !ok || got != "true" {
		t.Errorf("reopened = %q, %v, %v", got, ok, err)
	}
}
