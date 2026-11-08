package ui

import (
	"bytes"
	"testing"
)

// The pane mutes the logger while it is up, so the terminal is only ever written
// by the renderer. Forgetting to restore is the failure that would leave the user
// with no log output for the rest of the session (print mode included, since the
// writer is shared), and a lost write is the one that silences a real warning.
func TestLogTargetMuteRestores(t *testing.T) {
	var buf bytes.Buffer
	target := NewLogTarget(&buf)
	if _, err := target.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}
	undo := target.Mute()
	if _, err := target.Write([]byte("hidden\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "before\n" {
		t.Fatalf("a muted logger still reached the terminal: %q", got)
	}
	undo()
	if _, err := target.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "before\nafter\n" {
		t.Fatalf("writes after unmuting: %q", got)
	}
	// A nil target is what a TUI built without a log writer has; muting it must
	// be a no-op rather than a panic.
	var none *LogTarget
	none.Mute()()
	if _, err := none.Write([]byte("x")); err != nil {
		t.Fatalf("nil target write: %v", err)
	}
}
