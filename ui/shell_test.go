package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
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

// openSession adds another session to the same window, through the same entry
// point a spawned subagent arrives by.
func openSession(t *testing.T, a *GUI, mode, modelName string) *GUI {
	t.Helper()
	cfg := newConfig(mode)
	l := &agent.Loop{Cfg: cfg, Model: &model.Deterministic{}, Env: &env.Local{}}
	g, err := a.sh.openSession(GUIOptions{
		Cfg: cfg, Loop: l, Model: &model.Sushi{Model: modelName}, ConfigName: "mini",
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	drainPosts()
	return g
}

// TestGUISessionsStreamIntoTheirOwnTranscripts is the hazard the step was named
// for: one shared streamState interleaves two turns, and the merged text lands in
// whichever tab repaints first.
func TestGUISessionsStreamIntoTheirOwnTranscripts(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openSession(t, a, "yolo", "beta")

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
	b := openSession(t, a, "confirm", "beta")

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
	b := openSession(t, a, "confirm", "beta")
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

// TestGUIChildSessionIsAttendedInItsOwnTab is step 7.4 from the front end's side:
// a spawn opens a tab, and from that moment the child's prompts are the child's
// own. It is also the ordering test - OpenSession is called from another
// goroutine, as a spawn calls it, and the child must be attendable before the
// window has had a turn to draw it.
func TestGUIChildSessionIsAttendedInItsOwnTab(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")

	cfg := newConfig("confirm")
	l := &agent.Loop{Cfg: cfg, Model: &model.Deterministic{}, Env: &env.Local{}}
	opened := make(chan error, 1)
	var kid *GUI
	go func() {
		g, err := a.OpenSession(GUIOptions{
			Cfg: cfg, Loop: l, Model: &model.Sushi{Model: "beta"}, TabLabel: "worker",
		})
		kid = g
		opened <- err
	}()
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	// The route to a human exists the instant the session does, and the widgets
	// have not been built yet: the tab is posted, the hooks are not.
	if kid == nil || kid.loop.Hooks == nil {
		t.Fatal("the child was opened without a route to a human")
	}
	if kid.item != nil {
		t.Error("the tab was built off the toolkit goroutine")
	}
	drainPosts()

	if n := len(a.sh.sessions); n != 2 {
		t.Fatalf("%d sessions in the window, want 2", n)
	}
	items := a.sh.tabs.Items
	if len(items) != 2 || items[1].Text != "worker" {
		t.Fatalf("strip = %v, want the child labelled by the name its parent gave it", labels(items))
	}

	// The child asks; the parent's transcript, panel and pending slot stay clean.
	asked := make(chan Answer, 1)
	go func() {
		ans, err := kid.Ask(context.Background(), Question{Kind: AskConfirm,
			Title: "run rm -rf build/?", Cfg: kid.cfg})
		if err != nil {
			asked <- Answer{Text: "ask failed: " + err.Error()}
			return
		}
		asked <- ans
	}()
	waitFor(t, "the child's tab to be marked as waiting",
		func() bool { return items[1].Text == "⚠ worker" })
	if a.HasQuestion() {
		t.Error("a child with its own tab bubbled its prompt into its parent")
	}
	if !kid.v.card.Visible() {
		t.Error("the child's prompt never reached the child's panel")
	}
	if got := transcriptText(a.v.root()); strings.Contains(got, "rm -rf") {
		t.Errorf("the parent's transcript carries a child's prompt:\n%s", got)
	}

	kid.resolve(Answer{})
	select {
	case ans := <-asked:
		if ans.Reject || ans.Text != "" {
			t.Errorf("the child's asker got %+v, want the plain approval it was given", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the child's asker never woke")
	}
	if got := items[1].Text; got != "worker" {
		t.Errorf("the marker outlived the answer: label is %q", got)
	}
}

// labels spells the strip out for a failure message; a []TabItem prints as
// pointers, which tells nobody which label went wrong.
func labels(items []*container.TabItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}
