package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// Two sessions in one window is the whole point of the shell, and every test in
// this file asserts a collision that the pre-shell GUI could not avoid: the plan
// named it directly ("leave streamState shared and a background session writes
// into the transcript the human is reading", docs/parallel-sessions.md §7 step
// 7.2). They drive two GUIs that share one window, one queue and one tick, exactly
// as two open tabs do.

// newTestWindow builds the window's first session with a scripted model of its
// own; the second and later sessions come from openSession.
func newTestWindow(t *testing.T, mode, modelName string) *GUI {
	t.Helper()
	q := newTestQueue(t)
	cfg := newConfig(mode)
	l := &agent.Loop{Cfg: cfg, Model: &model.Deterministic{}, Env: &env.Local{}}
	g := NewGUI(GUIOptions{
		Cfg: cfg, Loop: l, Model: &model.Sushi{Model: modelName},
		ConfigName: "mini", App: test.NewApp(), Post: q.add,
	})
	attachPane(g, q)
	return g
}

// settle drains the post queue long enough for a deferred pass - the timer hop the
// focus reclaim uses - to have been queued and run. A negative assertion needs it:
// "nothing stole the focus" only means something once every reclaim that could
// have fired has had its turn.
func settle() {
	for range 20 {
		drainPosts()
		time.Sleep(5 * time.Millisecond)
	}
	drainPosts()
}

// openSession adds another session to the same window.
func openSession(a *GUI, mode, modelName string) *GUI {
	cfg := newConfig(mode)
	l := &agent.Loop{Cfg: cfg, Model: &model.Deterministic{}, Env: &env.Local{}}
	g := a.sh.open(GUIOptions{
		Cfg: cfg, Loop: l, Model: &model.Sushi{Model: modelName}, ConfigName: "mini",
	})
	drainPosts()
	return g
}

// TestGUISessionsStreamIntoTheirOwnTranscripts is the hazard the step was named
// for: one shared streamState interleaves two turns, and the merged text lands in
// whichever tab repaints first.
func TestGUISessionsStreamIntoTheirOwnTranscripts(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openSession(a, "yolo", "beta")

	a.Reasoning("alpha thinking")
	a.Content("alpha prose")
	b.Reasoning("beta thinking")
	b.Content("beta prose")
	drainPosts()

	if r, c := a.StreamText(); r != "alpha thinking" || c != "alpha prose" {
		t.Errorf("alpha's live buffers = %q/%q, want its own text only", r, c)
	}
	if r, c := b.StreamText(); r != "beta thinking" || c != "beta prose" {
		t.Errorf("beta's live buffers = %q/%q, want its own text only", r, c)
	}

	// The turn landing is where the mixing would become permanent: each message
	// supersedes its own session's live area and commits under its own header.
	a.Message(model.Message{"role": "assistant", "content": "alpha answer",
		"extra": map[string]any{"cost": 1.0}})
	b.Message(model.Message{"role": "assistant", "content": "beta answer",
		"extra": map[string]any{"cost": 2.0}})
	drainPosts()

	ta, tb := transcriptText(a.v.root()), transcriptText(b.v.root())
	if !strings.Contains(ta, "alpha answer") || strings.Contains(ta, "beta") {
		t.Errorf("alpha's transcript =\n%s\nwant its own answer and nothing of beta's", ta)
	}
	if !strings.Contains(tb, "beta answer") || strings.Contains(tb, "alpha") {
		t.Errorf("beta's transcript =\n%s\nwant its own answer and nothing of alpha's", tb)
	}
	// Two turns in flight report two different costs: the header and the footer
	// read the loop they belong to, not the process.
	if strings.Contains(ta, "2.00") || !strings.Contains(tb, "2.00") {
		t.Errorf("the cost rollups crossed: alpha=%q beta=%q", ta, tb)
	}
}

// TestGUISessionAnswersItsOwnQuestion is the safety half of the split. Before it,
// one pending slot meant a keystroke at one session could approve another's
// command - the exact failure §5.5 exists to prevent - and one ask queue meant an
// unrelated repository's confirmation waited behind a prompt nobody was looking at.
func TestGUISessionAnswersItsOwnQuestion(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openSession(a, "confirm", "beta")

	asked := make(chan Answer, 1)
	go func() {
		ans, err := b.Ask(context.Background(), Question{Kind: AskConfirm,
			Title: "run rm -rf build/?", Cfg: b.cfg})
		if err != nil {
			asked <- Answer{Text: "ask failed: " + err.Error()}
			return
		}
		asked <- ans
	}()
	waitFor(t, "beta's question to reach beta's panel", func() bool { return b.HasQuestion() })

	if a.HasQuestion() {
		t.Error("alpha reports beta's question as its own")
	}
	if q := a.PendingQuestion(); q.Title != "" {
		t.Errorf("alpha's pending question = %q, want none", q.Title)
	}
	// The panel is per session: a prompt raised in the background tab must not
	// cover the transcript the human is reading, and must not be invisible in its
	// own tab either.
	if a.v.card.Visible() {
		t.Error("the visible session shows a prompt it was never asked")
	}
	if !b.v.card.Visible() {
		t.Error("beta's question never reached beta's panel")
	}

	// Typing at the visible session runs through *its* state machine: alpha has no
	// run in flight, so it answers for itself in its own transcript, and the
	// answer beta's asker is parked on stays unanswered - which is §5.5's safety
	// property, previously one shared slot away from being void.
	a.v.handleInput("check the README first")
	if b.PendingQuestion().Title != "run rm -rf build/?" {
		t.Fatalf("beta's question changed to %q", b.PendingQuestion().Title)
	}
	if got := transcriptText(a.v.root()); !strings.Contains(got, "the run has ended") {
		t.Errorf("alpha did not answer for itself, its transcript is:\n%s", got)
	}

	b.resolve(Answer{Text: "not that way", Reject: true})
	select {
	case ans := <-asked:
		if !ans.Reject || ans.Text != "not that way" {
			t.Errorf("beta's asker got %+v, want the rejection it was given", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("beta's asker never woke - its answer went to the wrong session")
	}
	if b.HasQuestion() || a.HasQuestion() {
		t.Error("a question outlived its answer")
	}
}

// TestGUIWindowChromeFollowsTheVisibleTab covers the two per-window side effects a
// background session used to be free to cause: retitling the window with its mode,
// and taking the keyboard back for its own input.
func TestGUIWindowChromeFollowsTheVisibleTab(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openSession(a, "confirm", "beta")
	win := a.sh.window

	a.v.refreshStatus()
	if got := win.Title(); got != "mini · yolo" {
		t.Fatalf("title = %q, want the visible session's mode", got)
	}

	// Beta's tick runs while alpha is on screen: its own footer updates, the window
	// title and alpha's footer do not.
	mine := a.v.status.Text
	b.v.refreshStatus()
	if got := win.Title(); got != "mini · yolo" {
		t.Errorf("a background session retitled the window to %q", got)
	}
	if a.v.status.Text != mine {
		t.Errorf("beta's tick rewrote alpha's footer: %q", a.v.status.Text)
	}
	if b.v.status.Text == "" {
		t.Error("beta's own footer never updated")
	}

	a.sh.selectTab(b)
	if got := win.Title(); got != "mini · confirm" {
		t.Errorf("after selecting beta the title is %q, want its mode", got)
	}
	if win.Canvas().Focused() != fyne.Focusable(b.v.editor) {
		t.Errorf("focus after selecting beta = %v, want beta's input", win.Canvas().Focused())
	}

	// And a background input may not reclaim the caret when its own focus was
	// legitimately dropped - that is how a hidden tab eats keystrokes. The human
	// clicks tab B's label, the click lands on nothing focusable, focus goes nil,
	// and both tabs' deferred reclaims are now in the queue. B's may fire (it is
	// the visible session); A's must not.
	//
	// This is a net rather than a proof: Fyne's own FocusManager already declines
	// to focus content under a hidden ancestor, so the driver cannot reproduce the
	// transition window the isCurrent clause covers. What it can assert is that no
	// sequence of legitimate clicks leaves the caret in an unseen transcript.
	a.sh.selectTab(a)
	a.v.editor.FocusLost()
	a.sh.tabs.Select(b.item)
	win.Canvas().Unfocus()
	settle()
	if f := win.Canvas().Focused(); f == fyne.Focusable(a.v.editor) {
		t.Error("the hidden tab reclaimed the caret; the visible one cannot be typed into")
	}
}
