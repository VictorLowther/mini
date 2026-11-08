package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// A resumed pane replays the stored transcript through the same blockFor path
// a live message takes: prompt plumbing stays hidden, the observation renders
// from its result file, and the header/note name the session.
func TestTUIResumeReplaysStoredTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.log")
	if err := os.WriteFile(path, []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	tui, err := NewTUI(TUIOptions{
		Cfg: c, Loop: l,
		Model:      &model.Sushi{Model: "test-model", BaseURL: "http://127.0.0.1:1"},
		ConfigName: "mini",
		Width:      90, Height: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	tui.SetSessionID("01a1dead-beef-7000-8000-000000000000")
	tui.Resume([]model.Message{
		{"role": "system", "content": "sys prompt"},
		{"role": "user", "content": "rendered instance"},
		{"role": "assistant", "content": "I will echo", "extra": map[string]any{"turn": int64(2)}},
		{"role": "tool", "content": `{"returncode": 0, "output": "hi\n"}`, "tool_call_id": "c",
			"extra": map[string]any{"command": "echo hi", "returncode": 0, "tcr_path": path, "turn": int64(3)}},
	}, "resumed session 01a1dead (4 turns)")
	m := newTUIModel(tui, "", func() {})

	var parts []string
	for _, b := range m.blocks {
		parts = append(parts, b.kind+":"+b.text+"|"+strings.Join(b.transcript, "|"))
	}
	all := strings.Join(parts, "\n")
	if strings.Contains(all, "sys prompt") || strings.Contains(all, "rendered instance") {
		t.Errorf("prompt plumbing replayed into the pane:\n%s", all)
	}
	if !strings.Contains(all, "I will echo") {
		t.Errorf("assistant turn not replayed:\n%s", all)
	}
	if !strings.Contains(all, "$ echo hi") || !strings.Contains(all, "hi") {
		t.Errorf("observation not replayed from its result file:\n%s", all)
	}
	if !strings.Contains(all, "resumed session 01a1dead (4 turns)") {
		t.Errorf("resume note missing:\n%s", all)
	}
	if !strings.Contains(all, "session 01a1dead-beef-7000-8000-000000000000") {
		t.Errorf("session header missing the session id:\n%s", all)
	}
}
