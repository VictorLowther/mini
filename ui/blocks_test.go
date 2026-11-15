package ui

import (
	"strings"
	"sync"
	"testing"

	"fyne.io/fyne/v2/test"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// The transcript header is where a session says which extent it ran against,
// so the window mini resolved at startup - and who reported it - has to reach
// it. It stays out of the line until something resolved one way or the other:
// "window unknown" is not news a user asked for.
func TestSessionHeaderLineStatesTheWindow(t *testing.T) {
	got := sessionHeaderLine("qwen", "http://127.0.0.1:1234", "mini.yaml", "01a1",
		"1048576 tok (server)")
	for _, want := range []string{"qwen @ http://127.0.0.1:1234", "config mini.yaml",
		"1048576 tok (server)", "session 01a1"} {
		if !strings.Contains(got, want) {
			t.Errorf("header %q missing %q", got, want)
		}
	}
	if strings.Index(got, "1048576") > strings.Index(got, "session") {
		t.Errorf("the window outranked the session id: %q", got)
	}

	if got := sessionHeaderLine("qwen", "http://h", "mini.yaml", "", ""); got !=
		"mini-go "+agent.Version+" · qwen @ http://h · config mini.yaml" {
		t.Errorf("header without window or session = %q", got)
	}
}

// TestGUIHeaderCarriesTheResolvedWindow keeps the wiring honest: the number
// rides from the model client through NewGUI into the transcript.
func TestGUIHeaderCarriesTheResolvedWindow(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	g := NewGUI(GUIOptions{
		Cfg: c, Loop: l, ConfigName: "mini", App: test.NewApp(),
		Model: &model.Sushi{
			Model: "test-model", BaseURL: "http://127.0.0.1:1",
			ContextWindow: 1048576, ContextWindowSource: model.ContextFromServer,
		},
	})
	p := &guiPane{GUI: g, v: g.v, mu: new(sync.Mutex)}
	g.post = func(fn func()) { p.do(fn) }

	p.do(func() { p.prepareTranscript() })
	all := p.transcript()
	if !strings.Contains(all, "1048576 tok (server)") {
		t.Errorf("the transcript lost the resolved window:\n%s", all)
	}
}

func TestRunStatsContextPercent(t *testing.T) {
	snap := agent.Snapshot{Calls: 2, Cost: 0.1, CtxTokens: 131072}
	if got := runStatsText(snap, "confirm", false, 262144); !strings.Contains(got, "ctx 131k (50%)") {
		t.Errorf("runStatsText = %q", got)
	}
	if got := runStatsText(snap, "confirm", false, 0); strings.Contains(got, "ctx") {
		t.Errorf("an unresolved window invented a percentage: %q", got)
	}
	if got := runStatsText(agent.Snapshot{Calls: 1, CtxTokens: 400000}, "yolo", true, 262144); !strings.Contains(got, "ctx 400k OVER") {
		t.Errorf("over the window reads %q", got)
	}
}
