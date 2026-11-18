package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

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
	return openWithOptions(t, a, GUIOptions{
		Cfg: newConfig(mode), Model: &model.Sushi{Model: modelName},
	})
}

// openWithOptions opens a session with the caller's own wiring; the "+" and a
// spawn both arrive here, so a test that needs a directory, a closer or a busy job
// registry can ask for one. Zero fields get the same scripted defaults every other
// test session uses.
func openWithOptions(t *testing.T, a *GUI, o GUIOptions) *GUI {
	t.Helper()
	if o.Cfg == nil {
		o.Cfg = newConfig("yolo")
	}
	if o.Model == nil {
		o.Model = &model.Sushi{Model: "beta"}
	}
	if o.Loop == nil {
		o.Loop = &agent.Loop{Cfg: o.Cfg, Model: &model.Deterministic{}, Env: &env.Local{}}
	}
	g, err := a.sh.openSession(o)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	drainPosts()
	return g
}

// busyJobs is a registry with work still burning in it, which is how a test says
// "this session is not safe to close" without running a turn for five seconds.
type busyJobs struct{ live int }

func (b *busyJobs) JobCard(string) (JobCard, bool) { return JobCard{}, false }
func (b *busyJobs) LiveJobs() (int, error)         { return b.live, nil }

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

// TestGUIPickerTabOpensASessionWhereTheHumanAsked is step 7.3: the "+" cannot
// build a session on the canvas thread, so it puts a form in the strip and the
// session opens - and takes its tab with it - after the commit. Enter in the entry
// is the path a keyboard-first user takes, so that is what the test presses.
func TestGUIPickerTabOpensASessionWhereTheHumanAsked(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	// The opener is called off the canvas thread, so its answer travels by channel:
	// a captured string here is the race, not the code under test.
	asked := make(chan string, 1)
	a.sh.opener = func(dir string) (GUIOptions, error) {
		asked <- dir
		return GUIOptions{Dir: dir, TabLabel: "second repo"}, nil
	}

	// DocTabs' "+" button appends what CreateTab returns and selects it; the
	// contract is reproduced here because the button itself is renderer-internal.
	item := a.sh.createTab()
	if item == nil {
		t.Fatal("the + gave no tab")
	}
	a.sh.tabs.Append(item)
	a.sh.tabs.SelectIndex(len(a.sh.tabs.Items) - 1)
	if len(a.sh.sessions) != 1 {
		t.Fatalf("a picker is not a session, but %d sessions are registered", len(a.sh.sessions))
	}

	entry := findEntry(t, item.Content)
	// The entry holds what the human typed and Enter submits it; open() reads the
	// field rather than the callback's argument, exactly as a real keystroke does.
	entry.SetText("/tmp/second/checkout")
	entry.OnSubmitted(entry.Text)
	// The opening runs on a goroutine, so wait for the session and then let the
	// posted widgets land.
	select {
	case got := <-asked:
		if got != "/tmp/second/checkout" {
			t.Fatalf("opener got %q, want the directory that was typed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the typed directory never reached the opener")
	}
	waitFor(t, "the new session's tab", func() bool { return len(a.sh.sessions) == 2 })
	if got := labels(a.sh.tabs.Items); len(got) != 2 || got[1] != "second repo" {
		t.Errorf("strip = %v, want the picker gone and the new session in its place", got)
	}
	if a.sh.selected() != a.sh.sessions[1] {
		t.Error("the tab the human asked for is not the one on screen")
	}
}

// TestGUITabCloseStopsItsOwnSessionOnly is decision 13's per-session authority
// surface in the strip: × ends one conversation, its jobs and its children, and
// never a sibling.
func TestGUITabCloseStopsItsOwnSessionOnly(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	closed := make(chan string, 2)
	b := openWithOptions(t, a, GUIOptions{
		TabLabel: "beta",
		Closer:   func() error { closed <- "beta"; return nil },
	})
	if len(a.sh.tabs.Items) != 2 {
		t.Fatalf("strip = %v, want two tabs", labels(a.sh.tabs.Items))
	}

	a.sh.closeTab(b.item)
	select {
	case got := <-closed:
		if got != "beta" {
			t.Errorf("closed %q, want beta", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the closed session's own closer never ran")
	}
	if len(a.sh.sessions) != 1 || a.sh.selected() != a {
		t.Errorf("%d sessions left, selected %v; want alpha alone", len(a.sh.sessions), a.sh.selected())
	}
	if got := labels(a.sh.tabs.Items); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("strip = %v, want the closed tab removed", got)
	}

	// The last tab is the window: closing it leaves nothing to type into.
	a.sh.closeTab(a.item)
	if len(a.sh.sessions) != 0 || len(a.sh.tabs.Items) != 0 {
		t.Errorf("after the last close: %d sessions, %d tabs", len(a.sh.sessions), len(a.sh.tabs.Items))
	}
}

// TestGUITabCloseGuardsAWorkingSession is why × is not simply a removal: closing
// cascades to a session's jobs and children, so a stray click must not end a run
// that is twenty minutes into a training job. The second press inside the window
// is the human meaning it.
func TestGUITabCloseGuardsAWorkingSession(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	closed := make(chan string, 1)
	b := openWithOptions(t, a, GUIOptions{
		TabLabel: "busy", Jobs: &busyJobs{live: 1},
		Closer: func() error { closed <- "busy"; return nil },
	})

	a.sh.closeTab(b.item)
	if len(a.sh.sessions) != 2 {
		t.Fatal("a working session closed on the first press")
	}
	select {
	case got := <-closed:
		t.Fatalf("the session was stopped (%q) while the close was still being confirmed", got)
	default:
	}
	if got := transcriptText(b.v.root()); !strings.Contains(got, "still working") {
		t.Errorf("the guard said nothing to the session being closed:\n%s", got)
	}

	a.sh.closeTab(b.item)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the second press did not close the session")
	}
	if len(a.sh.sessions) != 1 {
		t.Errorf("%d sessions left after the confirming press", len(a.sh.sessions))
	}
}

// TestGUISelectOffsetCycles: the chords exist because a narrow window scrolls tab
// labels out of sight, and a cycle that dead-ends at the edge is not a cycle.
func TestGUISelectOffsetCycles(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openSession(t, a, "yolo", "beta")
	c := openSession(t, a, "yolo", "gamma")

	a.sh.selectOffset(1)
	if a.sh.selected() != b {
		t.Errorf("+1 from alpha selected %v", a.sh.selected().label())
	}
	a.sh.selectOffset(1)
	if a.sh.selected() != c {
		t.Errorf("+1 from beta selected %v", a.sh.selected().label())
	}
	a.sh.selectOffset(1)
	if a.sh.selected() != a {
		t.Errorf("+1 from the last tab did not wrap to the first")
	}
	a.sh.selectOffset(-1)
	if a.sh.selected() != c {
		t.Errorf("-1 from the first tab did not wrap to the last")
	}
}

// findEntry digs the directory entry out of a widget tree the way the keyboard
// would reach it.
func findEntry(t *testing.T, o fyne.CanvasObject) *widget.Entry {
	t.Helper()
	if e, ok := o.(*widget.Entry); ok {
		return e
	}
	if c, ok := o.(*fyne.Container); ok {
		for _, ch := range c.Objects {
			if e := findEntry(t, ch); e != nil {
				return e
			}
		}
	}
	if v, ok := o.(interface{ CreateRenderer() fyne.WidgetRenderer }); ok {
		for _, ch := range v.CreateRenderer().Objects() {
			if e := findEntry(t, ch); e != nil {
				return e
			}
		}
	}
	return nil
}

// TestGUIQuitGuardsWorkInFlight: with a tab per session, one ⌘Q can end several
// conversations, so the batched cancel is armed like a tab's × - the first press
// reports what is in flight, in the transcript of every session that has work, and
// the second within the window goes through.
func TestGUIQuitGuardsWorkInFlight(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openWithOptions(t, a, GUIOptions{TabLabel: "busy", Jobs: &busyJobs{live: 2}})

	a.sh.quit()
	if a.sh.quitting {
		t.Fatal("the window quit on the first press with a session working")
	}
	if len(a.sh.sessions) != 2 {
		t.Fatalf("%d sessions after the refusal, want both left alone", len(a.sh.sessions))
	}
	if got := transcriptText(b.v.root()); !strings.Contains(got, "still working") ||
		!strings.Contains(got, "1 of 2 sessions working") {
		t.Errorf("the working session was not told what is waiting on it:\n%s", got)
	}
	if got := transcriptText(a.v.root()); strings.Contains(got, "still working") {
		t.Errorf("an idle session was warned about work it has none of:\n%s", got)
	}

	a.sh.quit()
	if !a.sh.quitting {
		t.Error("the second press did not leave")
	}
}

// TestGUITabLabelCarriesWorkInFlight is the roster's virtue kept: decision 15 moved
// "see every session at once" into the labels, so a subagent with a twenty-minute
// handoff running must not read the same as one that has gone quiet.
func TestGUITabLabelCarriesWorkInFlight(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	jobs := &busyJobs{}
	b := openWithOptions(t, a, GUIOptions{TabLabel: "worker", Jobs: jobs})
	if got := b.item.Text; got != "worker" {
		t.Fatalf("label = %q, want the plain name while nothing runs", got)
	}

	jobs.live = 2
	b.refreshLabel(b.liveJobs())
	if got := b.item.Text; got != "worker · 2 jobs" {
		t.Errorf("label = %q, want the work in flight", got)
	}
	jobs.live = 1
	b.refreshLabel(b.liveJobs())
	if got := b.item.Text; got != "worker · 1 job" {
		t.Errorf("label = %q, want the singular", got)
	}

	// A prompt waiting still outranks the count: the marker is the thing that has
	// to be seen, and the jobs are already going.
	waiting := make(chan Answer, 1)
	go func() {
		ans, _ := b.Ask(context.Background(), Question{Kind: AskConfirm, Title: "continue?", Cfg: b.cfg})
		waiting <- ans
	}()
	waitFor(t, "the worker's tab to be marked", func() bool { return strings.HasPrefix(b.item.Text, "⚠") })
	if got := b.item.Text; got != "⚠ worker · 1 job" {
		t.Errorf("label = %q, want the marker and the count together", got)
	}
	b.resolve(Answer{})
	<-waiting
	if got := b.item.Text; got != "worker · 1 job" {
		t.Errorf("label = %q after the answer, want the marker gone and the count kept", got)
	}
}

// TestGUIPauseHoldsItsOwnSession is the pause surface, and the per-session half of
// it: holding one conversation must not silently park the tab next to it, which is
// the same class of bug as one shared streamState. That the hold *works* - the loop
// stopping at its boundary and carrying on when released - is agent's
// TestPauseHoldsAtTheStepBoundary.
func TestGUIPauseHoldsItsOwnSession(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	b := openWithOptions(t, a, GUIOptions{TabLabel: "worker"})

	a.setPause(true)
	if !a.loop.Paused() {
		t.Fatal("alpha is not held")
	}
	if b.loop.Paused() {
		t.Error("holding alpha parked the session in the next tab")
	}
	if got := a.item.Text; got != "⏸ alpha" {
		t.Errorf("tab = %q, want the hold visible", got)
	}
	a.v.refreshStatus()
	if !strings.Contains(a.v.status.Text, "paused") {
		t.Errorf("footer = %q, want it to say so too", a.v.status.Text)
	}

	// esc cannot interrupt a step that is not running; saying it cancelled one
	// would be a lie, and the hold would vanish with the lie.
	a.v.esc()
	if !a.loop.Paused() {
		t.Error("esc quietly released the hold")
	}
	if got := transcriptText(a.v.root()); !strings.Contains(got, "held at its step boundary") {
		t.Errorf("esc said nothing about the hold:\n%s", got)
	}

	if !a.v.commandOrSteer("/resume") {
		t.Error("/resume was not handled as a command")
	}
	if a.loop.Paused() {
		t.Error("/resume did not release the session")
	}
	if got := a.item.Text; got != "alpha" {
		t.Errorf("tab = %q after resuming, want the marker gone", got)
	}
	if !a.v.commandOrSteer("/pause") || !a.loop.Paused() {
		t.Error("/pause did not hold the session")
	}
	a.setPause(false)
	drainPosts()
}

// jobLister is a registry holding one handed-off run, still burning until a card's
// End control stops it.
//
// The live flag is guarded because the ender runs on the goroutine the button
// spawns - which is how it must run, since Kill waits through the unwind - while the
// card and the tick read it from the toolkit goroutine. An unguarded bool here is
// the race, not the code under test.
type jobLister struct {
	id      string
	mu      sync.Mutex
	live    bool
	elapsed time.Duration // what the registry reports; a running job's card text
}

func (j *jobLister) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.live
}

func (j *jobLister) stop() {
	j.mu.Lock()
	j.live = false
	j.mu.Unlock()
}

func (j *jobLister) JobCard(id string) (JobCard, bool) {
	if id != j.id {
		return JobCard{}, false
	}
	state := "killed"
	if j.running() {
		state = "running"
	}
	j.mu.Lock()
	elapsed := j.elapsed
	j.mu.Unlock()
	return JobCard{ID: id, Running: j.running(), State: state, Label: "train", Elapsed: elapsed}, true
}

func (j *jobLister) LiveJobs() (int, error) {
	if j.running() {
		return 1, nil
	}
	return 0, nil
}

// committedForTest gives a session's view one block to render, exactly as
// appendBlock would have left it.
func (g *GUI) committedForTest(b block) {
	g.v.committed = []block{b}
	g.v.blockH = []float32{estimateBlockHeight(b)}
}

// newRowForTest materializes row i the way the virtualizer does.
func (g *GUI) newRowForTest(i int) fyne.CanvasObject { return g.v.newRow(i) }

// findArea digs the first card out of a materialized row, the way the strip
// reaches one.
func findArea(o fyne.CanvasObject) *area {
	if a, ok := o.(*area); ok {
		return a
	}
	if c, ok := o.(*fyne.Container); ok {
		for _, ch := range c.Objects {
			if a := findArea(ch); a != nil {
				return a
			}
		}
	}
	return nil
}

// jobCardBlock is the committed shape a handed-off call leaves behind: a tool card
// whose body is the live job's lines, tagged with the registry id it tracks.
func jobCardBlock(id string) block {
	return block{kind: "tool", title: "train", job: id,
		transcript: []string{"$ python train.py", "job " + id + " is still running (10s)  out=train.log"}}
}

// TestGUIJobCardEndsItsOwnJob is the human's way out of §5.10's deliberate residue:
// a job that outlived its turn is visible on a card, and now it is also endable
// without ending the session or arguing the model into calling job_kill.
func TestGUIJobCardEndsItsOwnJob(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	reg := &jobLister{id: "j1", live: true}
	ended := make(chan string, 4)
	b := openWithOptions(t, a, GUIOptions{
		TabLabel: "worker", Jobs: reg,
		JobEnd: func(id string) error { ended <- id; reg.stop(); return nil },
	})
	b.committedForTest(jobCardBlock("j1"))

	row := b.newRowForTest(0)
	ar := findArea(row)
	if ar == nil {
		t.Fatal("the job card materialized as no card at all")
	}
	if len(ar.actions.Objects) != 1 {
		t.Fatalf("a running job's card carries %d controls, want its End button", len(ar.actions.Objects))
	}
	btn := ar.actions.Objects[0].(*widget.Button)
	if btn.Text != "End" {
		t.Fatalf("button reads %q", btn.Text)
	}

	// First press arms and touches nothing: the run is not ours to end by accident.
	btn.OnTapped()
	if btn.Text != "End it?" {
		t.Errorf("after the first press the button reads %q, want the confirmation", btn.Text)
	}
	select {
	case id := <-ended:
		t.Fatalf("the job was ended on the first press (%q)", id)
	default:
	}

	btn.OnTapped()
	select {
	case id := <-ended:
		if id != "j1" {
			t.Errorf("ended job %q, want the id this card tracks", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second press never reached the registry")
	}
	drainPosts()

	// A job that is no longer running gets no control, so the card cannot offer an
	// action that would do nothing.
	if got := findArea(b.newRowForTest(0)); len(got.actions.Objects) != 0 {
		t.Errorf("a finished job's card still carries %d controls", len(got.actions.Objects))
	}
}

// TestGUIExportWritesEachSessionsOwnTrajectory is the export surface: /export acts
// on the session in the tab it was typed in, takes an optional path, and reports
// where the file went. A window with no exporter says so rather than writing
// something to a path nobody chose.
func TestGUIExportWritesEachSessionsOwnTrajectory(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	a.sessionID = "11111111-1111-7000-8000-000000000000"
	dir := t.TempDir()
	type call struct{ id, path string }
	requests := make(chan call, 4)
	a.sh.exporter = func(id, path string) (string, error) {
		requests <- call{id, path}
		return filepath.Join(dir, "mini-"+id[:8]+".traj.json"), nil
	}

	if !a.v.commandOrSteer("/export") {
		t.Fatal("/export was not handled as a command")
	}
	sel := <-requests
	if sel.id != a.sessionID || sel.path != "" {
		t.Errorf("export asked for %+v, want this session with no path of its own", sel)
	}
	if got := transcriptText(a.v.root()); !strings.Contains(got, "mini-11111111.traj.json") {
		t.Errorf("the tab never said where the trajectory went:\n%s", got)
	}

	// A second tab exports itself, not the session whose tab the port was made on.
	b := openWithOptions(t, a, GUIOptions{TabLabel: "worker"})
	b.sessionID = "22222222-2222-7000-8000-000000000000"
	chosen := filepath.Join(dir, "chosen.json")
	if !b.v.commandOrSteer("/export " + chosen) {
		t.Fatal("/export with a path was not handled as a command")
	}
	sel = <-requests
	if sel.id != b.sessionID || sel.path != chosen {
		t.Errorf("worker's export asked for %+v, want its own id and the given path", sel)
	}

	// A window that cannot export says so, and writes nothing.
	quiet := newTestWindow(t, "yolo", "beta")
	quiet.sessionID = "33333333"
	quiet.export("")
	if got := transcriptText(quiet.v.root()); !strings.Contains(got, "cannot export") {
		t.Errorf("an unbacked export said:\n%s", got)
	}
}

// NOTE: a foreground card whose file the loop just named still takes the rebuild
// path once per tick - liveCardText does not find its text widget through the
// scroll the transcript grid builds, so rewriteCardLines reports it cannot rewrite
// in place. The streamed content is right; the second-by-second rebuild that §8.11
// removed for job cards is still there for these, and fixing the handle lookup is
// the next step, not something to pretend this test covers.

// TestGUIRunningCardStreamsTheCallsOutput is the "running" state for a foreground
// command: the loop names the result file before executing, the card that call was
// issued into starts tailing it, and each tick that brings new output rewrites that
// one card in place. A two-minute build should show what it has printed, not a
// label that says something is happening.
func TestGUIRunningCardStreamsTheCallsOutput(t *testing.T) {
	a := newTestWindow(t, "yolo", "alpha")
	v := a.v
	v.rootC.Resize(fyne.NewSize(420, 320))
	log := filepath.Join(t.TempDir(), "1.log")
	if err := os.WriteFile(log, []byte("compiling alpha.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	v.issueCard(block{kind: "tool", title: "make", tcid: "c1", cmd: "make -j8",
		state: "created", transcript: []string{"$ make -j8", "issued"}})
	v.markCardRunning(model.Action{Command: "make -j8", ToolCallID: "c1"}, log)
	if got := transcriptText(v.root()); !strings.Contains(got, "compiling alpha.go") {
		t.Errorf("the running card never showed the output:\n%s", got)
	}
	if got := transcriptText(v.root()); strings.Contains(got, "issued") {
		t.Errorf("the card stayed in its created state:\n%s", got)
	}

	// More output arrives; the tick follows it without rebuilding the window.
	if err := os.WriteFile(log, []byte("compiling alpha.go\nlinking beta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The transition (a card gaining a file to tail, and so a handle for its
	// text) may rebuild once; what must not happen again is the *steady state*
	// rebuilding every second.
	drainPosts()
	if !v.refreshJobCards() {
		t.Error("a card with a live result file reported nothing running")
	}
	drainPosts()
	if got := transcriptText(v.root()); !strings.Contains(got, "linking beta") {
		t.Errorf("the second tail never reached the card:\n%s", got)
	}

}

// TestGUISubagentCardReportsProgress is what a parent needs to see about a child it
// cannot read: how far it has got. The transcript is one click away in the child's
// own tab; the card answers "is it working, and what is it costing".
func TestGUISubagentCardReportsProgress(t *testing.T) {
	card := JobCard{ID: "j7", Kind: "subagent", Running: true, Elapsed: 90 * time.Second,
		Label: "survey", OutputPath: "/dev/null", Steps: 12, Tokens: 48213, Cost: 0.14}
	out := strings.Join(jobCardLines(card, true), "\n")
	if !strings.Contains(out, "12 steps") || !strings.Contains(out, "48213 tokens") || !strings.Contains(out, "$0.14") {
		t.Errorf("the subagent card says nothing useful about progress:\n%s", out)
	}
	// A bash job's card keeps its own shape: no invented step counts.
	bash := JobCard{ID: "j8", Kind: "bash", Running: true, Elapsed: 5 * time.Second, OutputPath: "/dev/null"}
	if got := strings.Join(jobCardLines(bash, true), "\n"); strings.Contains(got, "steps") {
		t.Errorf("a plain command's card claims step counts it cannot know:\n%s", got)
	}
}
