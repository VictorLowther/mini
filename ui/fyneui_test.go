package ui

import (
	"fyne.io/fyne/v2/container"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// waitFor polls a condition until the deadline, so an assertion never races
// the toolkit goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// The deferred work a test is waiting for arrives through the queue, so
		// polling without draining it would only ever see the state as of the
		// last synchronization point.
		drainPosts()
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// turnWithActions is an assistant message whose only content is one command:
// the shape a confirm-mode run hands the UI before executing anything.
func turnWithActions(cmd string) model.Message {
	return model.Message{
		"role":    "assistant",
		"content": "turn",
		"extra":   map[string]any{"actions": []model.Action{{Command: cmd, ToolCallID: "c"}}, "cost": 1.0},
	}
}

// normalizeTraj reduces a trajectory file to the bytes that must agree across
// frontends.
func normalizeTraj(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var traj map[string]any
	if err := json.Unmarshal(data, &traj); err != nil {
		t.Fatal(err)
	}
	stripVolatile(traj)
	if info, ok := traj["info"].(map[string]any); ok {
		if cfg, ok := info["config"].(map[string]any); ok {
			delete(cfg, "agent")
			delete(cfg, "environment")
		}
	}
	out, _ := json.Marshal(traj)
	return string(out)
}

// stripVolatile removes the fields that legitimately differ between runs.
func stripVolatile(v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"timestamp", "elapsed_seconds", "outputs", "response", "cwd", "base_url", "model", "output_path", "tcr_path"} {
			delete(t, k)
		}
		for _, child := range t {
			stripVolatile(child)
		}
	case []any:
		for _, child := range t {
			stripVolatile(child)
		}
	}
}

// guiPane is a test handle on the GUI. The headless driver executes fyne.Do
// work inline, so the handle serialises deliveries with one mutex: exactly
// one "update goroutine" at a time. mu is a pointer so copying the handle
// keeps every delivery and assertion on the same lock.
type guiPane struct {
	*GUI
	v  *guiView
	mu *sync.Mutex
	q  *postQueue
}

// postQueue is the harness's stand-in for the toolkit event loop.
//
// Fyne's real driver marshals fyne.Do onto the one thread that owns the canvas;
// the software test driver documents the same rule and then runs the closure
// wherever it was called from (test/driver.go:53-56). So a `post` that simply
// invokes fn hands the view's state to whatever goroutine posted it - and every
// deferred pass in the view is scheduled by time.AfterFunc, whose callbacks run
// on the Go timer goroutine. That is the whole race: not a production bug, but a
// test double that kept the ordering guarantee and silently dropped the thread
// affinity the code depends on. All 166 reports in this package had a timer
// goroutine on one side and the test goroutine on the other.
//
// So posts are queued rather than executed, and the test goroutine - the one the
// driver says owns the canvas - drains the queue at every synchronization point.
type postQueue struct {
	mu    sync.Mutex
	items []func()
}

func (q *postQueue) add(fn func()) {
	q.mu.Lock()
	q.items = append(q.items, fn)
	q.mu.Unlock()
}

// drain runs everything queued, in order, on the calling goroutine - including
// anything a queued closure posts itself, which is why this loops rather than
// popping a snapshot.
func (q *postQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			return
		}
		fn := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		fn()
	}
}

// activeQueue is the pane under test. One GUI per test, and this package runs no
// test in parallel, so the queue a timer posts into is never ambiguous.
var activeQueue *postQueue

func drainPosts() {
	if q := activeQueue; q != nil {
		q.drain()
	}
}

func (p *guiPane) do(fn func()) {
	// Anything already deferred happened before this call, so it runs first;
	// anything this call posts runs before it returns, so a test can assert on
	// the result immediately, as it always could.
	drainPosts()
	p.mu.Lock()
	fn()
	p.mu.Unlock()
	drainPosts()
}

// attachPane wraps a GUI in the test pane. Any GUI built directly by a test
// needs one, and it must share the queue that was injected at construction -
// assigning g.post afterwards is the race the Post option exists to remove.
func attachPane(g *GUI, q *postQueue) *guiPane {
	return &guiPane{GUI: g, v: g.v, mu: new(sync.Mutex), q: q}
}

// newTestQueue returns the harness's stand-in event loop and makes it current
// for the duration of the test, so a deferred pass lands on the goroutine that
// owns the canvas instead of the timer that scheduled it.
func newTestQueue(t *testing.T) *postQueue {
	t.Helper()
	q := &postQueue{}
	previous := activeQueue
	activeQueue = q
	t.Cleanup(func() { activeQueue = previous })
	return q
}

func newGUIForTest(t *testing.T, c *agent.Config, l *agent.Loop) *guiPane {
	t.Helper()
	q := newTestQueue(t)
	g := NewGUI(GUIOptions{
		Cfg: c, Loop: l,
		Model:      &model.Sushi{Model: "test-model", BaseURL: "http://127.0.0.1:1"},
		ConfigName: "mini",
		App:        test.NewApp(),
		Post:       q.add,
	})
	if l != nil {
		h, err := New(g, c, l)
		if err != nil {
			t.Fatal(err)
		}
		l.Hooks = h
	}
	p := attachPane(g, q)
	g.v.paintGap = 0 // tests read the live area right after each chunk
	return p
}

// transcriptText renders the whole transcript (committed areas plus the live
// pair) as plain text: the scrollback, give or take the pixels.
func transcriptText(o fyne.CanvasObject) string {
	switch t := o.(type) {
	case *area:
		return transcriptText(t.header) + "\n" + transcriptText(t.body)
	case *widget.Card:
		return transcriptText(t.Content)
	case *widget.RichText:
		var b strings.Builder
		for _, s := range t.Segments {
			switch seg := s.(type) {
			case *widget.TextSegment:
				b.WriteString(seg.Text)
			case *widget.CodeBlockSegment:
				b.WriteString(seg.Text)
			}
		}
		return b.String()
	case *widget.Label:
		return t.Text
	case *widget.TextGrid:
		return t.Text()
	case *container.Scroll:
		return transcriptText(t.Content)
	case *widget.Accordion:
		parts := make([]string, 0, len(t.Items)*2)
		for _, it := range t.Items {
			parts = append(parts, "["+it.Title+"]", transcriptText(it.Detail))
		}
		return strings.Join(parts, "\n")
	case *fyne.Container:
		parts := make([]string, 0, len(t.Objects))
		for _, c := range t.Objects {
			parts = append(parts, transcriptText(c))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func (p *guiPane) transcript() string {
	var s string
	p.do(func() {
		// body holds the materialized window - committed rows and, at the end,
		// the live streaming row - so one walk covers committed and active.
		parts := make([]string, 0, len(p.v.body.Objects))
		for _, o := range p.v.body.Objects {
			parts = append(parts, transcriptText(o))
		}
		s = strings.Join(parts, "\n")
	})
	return s
}

func (p *guiPane) panel() string {
	var s string
	p.do(func() {
		parts := make([]string, 0, len(p.v.panel.Objects))
		for _, o := range p.v.panel.Objects {
			parts = append(parts, transcriptText(o))
		}
		s = strings.Join(parts, "\n")
	})
	return s
}

func (p *guiPane) status() string { var s string; p.do(func() { s = p.v.status.Text }); return s }
func (p *guiPane) value() string  { var s string; p.do(func() { s = p.v.editor.Text }); return s }
func (p *guiPane) panelShown() bool {
	var b bool
	p.do(func() { b = p.v.card.Visible() })
	return b
}

func (p *guiPane) typeText(s string) {
	p.do(func() {
		e := p.v.editor
		e.SetText(s)
		// SetText leaves the caret where it was; typed text continues at the
		// end, so place the caret there before the next keystroke.
		lines := strings.Split(s, "\n")
		e.CursorRow = len(lines) - 1
		e.CursorColumn = utf8.RuneCountInString(lines[len(lines)-1])
	})
}

func (p *guiPane) key(name fyne.KeyName) {
	p.do(func() { p.v.editor.TypedKey(&fyne.KeyEvent{Name: name}) })
}

func (p *guiPane) enter() { p.key(fyne.KeyReturn) }

func (p *guiPane) shiftEnter() {
	p.do(func() {
		p.v.editor.KeyDown(&fyne.KeyEvent{Name: desktop.KeyShiftLeft})
		p.v.editor.TypedKey(&fyne.KeyEvent{Name: fyne.KeyReturn})
		p.v.editor.KeyUp(&fyne.KeyEvent{Name: desktop.KeyShiftLeft})
	})
}

func (p *guiPane) esc() { p.key(fyne.KeyEscape) }

func (p *guiPane) rune(r rune) { p.do(func() { p.v.editor.TypedRune(r) }) }

func (p *guiPane) shortcut(key fyne.KeyName, mods fyne.KeyModifier) {
	p.do(func() {
		p.v.editor.TypedShortcut(&desktop.CustomShortcut{KeyName: key, Modifier: mods})
	})
}

// askG runs a question from the test goroutine so the test can then answer it
// with synthetic key presses, exactly as the window would.
func askG(t *testing.T, p *guiPane, q Question) <-chan Answer {
	t.Helper()
	got := make(chan Answer, 1)
	go func() {
		ans, err := p.Ask(context.Background(), q)
		if err != nil {
			ans = Answer{}
		}
		got <- ans
	}()
	waitFor(t, "the question to reach the panel", func() bool {
		return p.HasQuestion() && p.panelShown()
	})
	return got
}

func TestGUIConfirmPanelDrivesTheLoop(t *testing.T) {
	dir := t.TempDir()
	c := newConfig("confirm")
	c.OutputPath = filepath.Join(dir, "gui.traj.json")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", "echo done", submit("done\n")), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = l.Run(ctx, "make a file then submit"); close(done) }()

	waitFor(t, "the first confirmation", func() bool {
		return p.HasQuestion() && p.panelShown()
	})
	q := p.PendingQuestion()
	if q.Kind != AskConfirm || len(q.Actions) != 1 || q.Actions[0].Command != "echo hi" {
		t.Fatalf("question = %+v", q)
	}
	panel := p.panel()
	for _, want := range []string{"echo hi", "Execute 1 action(s)?", "enter to confirm"} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel missing %q: %s", want, panel)
		}
	}
	p.enter() // empty editor + enter confirms

	waitFor(t, "the second confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskConfirm
	})
	p.enter()

	waitFor(t, "the exit confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskNewTask
	})
	if panel := p.panel(); !strings.Contains(panel, "Agent is ready") {
		t.Errorf("panel = %q", panel)
	}
	p.enter() // no-op: an empty enter re-asks, it never ends the run
	select {
	case <-done:
		t.Fatal("an empty enter at the exit prompt ended the run")
	case <-time.After(300 * time.Millisecond):
	}
	waitFor(t, "the exit prompt after the no-op enter", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskNewTask
	})
	p.typeText("/quit")
	p.enter()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished")
	}
	data, err := os.ReadFile(c.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	var traj map[string]any
	if err := json.Unmarshal(data, &traj); err != nil {
		t.Fatal(err)
	}
	info := traj["info"].(map[string]any)
	if info["exit_status"] != "Submitted" || info["submission"] != "done\n" {
		t.Errorf("info = %#v", info)
	}
	scroll := p.transcript()
	for _, want := range []string{"LLM output · $1.00", "exit 0", "hi"} {
		if !strings.Contains(scroll, want) {
			t.Errorf("transcript missing %q:\n%s", want, scroll)
		}
	}
}

// TestGUITypeToReject: prose at the confirm prompt is a rejection comment,
// a bare y is the confirm shortcut.
func TestGUITypeToReject(t *testing.T) {
	c := newConfig("confirm")
	c.ConfirmExit = false
	// The sentinel echo is an ordinary command now, so a no-action turn must
	// follow it to end the run.
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo not-run", submitCmd, submit("done")), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()

	waitFor(t, "the confirmation", p.HasQuestion)
	p.typeText("use printf instead")
	p.enter()
	waitFor(t, "the second confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskConfirm
	})
	p.rune('y') // bare y on the empty editor confirms
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished")
	}
	if !containsContent(l.Messages(), "The user rejected your commands with the following message: use printf instead") {
		t.Errorf("rejection missing:\n%s", contentsOf(l.Messages()))
	}
	if containsContent(l.Messages(), "|not-run") {
		t.Error("the rejected command ran")
	}
}

// TestGUINextTurnAtTheReadyLineIsTheHuman is the other half of the report: the
// next thing typed when the agent comes back to rest is the conversation going
// on, not a discrete task the harness announces. A session's turns build on each
// other, and the transcript should not imply otherwise.
func TestGUINextTurnAtTheReadyLineIsTheHuman(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = true
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", submit("done"), "echo the follow-up", submit("done")),
		Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()

	waitFor(t, "the ready prompt", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskNewTask
	})
	p.typeText("now check it against the README")
	p.enter()
	waitFor(t, "the typed turn to be echoed", func() bool { return p.firstArea("user") != nil })
	if !containsContent(l.Messages(), "now check it against the README") {
		t.Errorf("the next turn never reached the history:\n%s", contentsOf(l.Messages()))
	}
	if a := p.firstArea("harness"); a != nil {
		t.Errorf("the human's next turn was attributed to the harness: %q", headerText(a))
	}
	waitFor(t, "the follow-up turn to run", func() bool { return l.Snapshot().Calls >= 2 })
	p.typeText("/quit")
	p.enter()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("/quit at the ready line did not end the run\ntranscript:\n%s", p.transcript())
	}
}

func TestGUIEscCancelsTheTurn(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "sleep 30"), Env: &env.Local{Timeout: 30 * time.Second}}
	p := newGUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()

	waitFor(t, "the command to start", func() bool { return l.Snapshot().Calls == 1 })
	p.esc()
	if scroll := p.transcript(); !strings.Contains(scroll, "cancelling this turn…") {
		t.Errorf("cancel note missing:\n%s", scroll)
	}
	waitFor(t, "the interruption prompt", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskInterrup
	})
	p.typeText("that took too long")
	p.enter()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("esc did not interrupt the running command\nmessages:\n%s", contentsOf(l.Messages()))
	}
	if !containsContent(l.Messages(), "Interrupted by user: that took too long") {
		t.Errorf("comment missing:\n%s", contentsOf(l.Messages()))
	}

	// And it is the human's own card. This is the reported bug: the comment
	// landed in a harness card because the view derived authorship from
	// interrupt_type with an allowlist of the human's kinds, so a kind that was
	// never enumerated defaulted to the harness's side.
	if p.firstArea("user") == nil {
		t.Errorf("the interruption comment did not render as a right-justified user card:\n%s", p.transcript())
	}
	if a := p.firstArea("harness"); a != nil {
		t.Errorf("the interruption comment was attributed to the harness: %q", headerText(a))
	}
}

// TestGUITranscriptSideFollowsAuthorship is the same rule as a table, because
// the interesting case is the one nobody has thought of yet: a human-typed
// message must read as the human's whether or not its interrupt_type appears
// below, and a harness report must not read as theirs however much it quotes
// them.
func TestGUITranscriptSideFollowsAuthorship(t *testing.T) {
	cases := []struct {
		name string
		b    block
		want string
	}{
		{"the first task", block{kind: "user", text: "do this"}, "user"},
		{"steering", block{kind: "user", subtype: "UserSteering", text: "check the README first"}, "user"},
		{"what gets typed at the ready line", block{kind: "user", subtype: "UserNewTask", text: "The user added a new task: fix it"}, "user"},
		{"a comment typed after an interrupt", block{kind: "user", subtype: "UserInterruption", typed: true, text: "Interrupted by user: that took too long"}, "user"},
		{"a kind invented after this test was written", block{kind: "user", subtype: "SomeFutureKind", typed: true, text: "hi"}, "user"},
		{"a rejection the harness words", block{kind: "user", subtype: "UserRejection", text: "Commands not executed."}, "harness"},
		{"a cancellation the human never commented on", block{kind: "user", subtype: "UserInterruption", text: "Temporary interruption caught."}, "harness"},
		{"a format error", block{kind: "user", subtype: "FormatError", text: "no action parsed"}, "harness"},
		{"a job update", block{kind: "user", subtype: "JobUpdate", text: "the training run finished"}, "harness"},
		{"a parent agent's message to a child", block{kind: "user", subtype: "ParentMessage", text: "stop and report"}, "harness"},
	}
	for _, tc := range cases {
		got, align := messageKindLabel(tc.b)
		if got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
		wantAlign := fyne.TextAlignLeading
		if tc.want == "user" {
			wantAlign = fyne.TextAlignTrailing
		}
		if align != wantAlign {
			t.Errorf("%s: align = %v, want %v", tc.name, align, wantAlign)
		}
	}
}

// TestGUIAuthorshipTravelsInTheMessage checks the carrier, not the reading: the
// marker has to ride the same model.Message the session store writes and replays,
// or the fix only covers the keystroke that made it.
func TestGUIAuthorshipTravelsInTheMessage(t *testing.T) {
	test.NewApp()

	spoken := userWords("Interrupted by user: that took too long", model.KindUserInterruption).(*model.Interrupt).Messages[0]
	b, show, _ := messageBlock(spoken, "h", "", func() {}, nil)
	if !show {
		t.Fatal("the interruption comment was not shown at all")
	}
	if !b.typed {
		t.Errorf("the message reached the block with no authorship: extra = %#v", spoken["extra"])
	}
	if got, _ := messageKindLabel(b); got != "user" {
		t.Errorf("label = %q, want user", got)
	}

	reported := interruptUser("Temporary interruption caught.", model.KindUserInterruption).(*model.Interrupt).Messages[0]
	b2, _, _ := messageBlock(reported, "h", "", func() {}, nil)
	if b2.typed {
		t.Error("a harness report about a cancellation claims the human spoke it")
	}
	if got, _ := messageKindLabel(b2); got != "harness" {
		t.Errorf("label = %q, want harness", got)
	}
}

// TestGUIReclaimsFocusAfterClickingElsewhere: tapping a non-focusable object
// clears canvas focus (Fyne's behaviour), and the entry must take it back
// immediately - the "input box stopped accepting input" regression.
func TestGUIReclaimsFocusAfterClickingElsewhere(t *testing.T) {
	c := newConfig("confirm")
	p := newGUIForTest(t, c, &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}})
	p.do(func() { p.prepareTranscript() })
	p.do(func() { p.v.start() })
	focused := func() bool {
		var ok bool
		p.do(func() { ok = p.v.win.Canvas().Focused() == p.v.editor })
		return ok
	}
	if !focused() {
		t.Fatal("start() must leave the input focused")
	}
	p.v.win.Canvas().Focus(nil) // a tap on a bubble / scrollbar / blank space
	waitFor(t, "the entry to reclaim focus", focused)
}

// TestGUIEscAtIdleNeverQuits: leaving the window is /quit or the native quit
// hotkey only - esc must not open a prompt that swallows keystrokes, and the
// input box must keep accepting text (that is where new tasks are typed).
func TestGUIEscAtIdleNeverQuits(t *testing.T) {
	c := newConfig("confirm")
	p := newGUIForTest(t, c, &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}})
	p.do(func() { p.prepareTranscript() })
	p.esc()
	// esc must not start leaving: that is /quit or the native hotkey.
	if strings.Contains(p.transcript(), "Quit?") {
		t.Errorf("esc opened a quit prompt:\n%s", p.transcript())
	}
	p.rune('q')
	if p.value() != "q" {
		t.Errorf("typing was swallowed after esc: %q", p.value())
	}
}

func TestGUISlashCommandsWorkAtThePanel(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newGUIForTest(t, c, l)

	got := askG(t, p, Question{Kind: AskConfirm, Title: confirmationTitle(1, ""), Cfg: c})
	p.typeText("/y")
	p.enter()
	if c.Mode != "yolo" {
		t.Errorf("mode = %s, want yolo", c.Mode)
	}
	if ans := <-got; ans.Reject {
		t.Errorf("/y must confirm the batch, got %+v", ans)
	}

	got = askG(t, p, Question{Kind: AskConfirm, Title: confirmationTitle(1, ""), Cfg: c})
	p.typeText("/h")
	p.enter()
	if !p.HasQuestion() {
		t.Error("/h answered the prompt")
	}
	if !strings.Contains(p.transcript(), "switch to yolo mode") {
		t.Errorf("help text never reached the transcript:\n%s", p.transcript())
	}
	p.typeText("")
	p.enter()
	<-got
}

func TestGUITaskQuestionUsesTheEditor(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newGUIForTest(t, c, l)
	got := askG(t, p, Question{Kind: AskTask, Title: TaskPrompt, Cfg: c})
	if panel := p.panel(); !strings.Contains(panel, "What do you want to do?") {
		t.Errorf("panel = %q", panel)
	}
	p.enter() // an empty answer is refused
	if !p.HasQuestion() {
		t.Error("an empty task was accepted")
	}
	if scroll := p.transcript(); !strings.Contains(scroll, "please describe the task") {
		t.Errorf("refusal note missing:\n%s", scroll)
	}
	p.typeText("fix the tests")
	p.enter()
	if ans := <-got; ans.Text != "fix the tests" {
		t.Errorf("answer = %+v", ans)
	}
}

// TestGUIMultilineIsAKeystroke: shift+enter inserts a newline, enter sends -
// there is no multi-line mode to be stuck in, and /m says so.
func TestGUIMultilineIsAKeystroke(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newGUIForTest(t, c, l)
	got := askG(t, p, Question{Kind: AskTask, Title: TaskPrompt, Cfg: c})

	p.typeText("one")
	p.shiftEnter()
	if p.value() != "one\n" {
		t.Fatalf("shift+enter did not insert a newline: %q", p.value())
	}
	if !p.HasQuestion() {
		t.Fatal("shift+enter must not submit")
	}
	p.typeText("one\ntwo")
	p.enter()
	if ans := <-got; ans.Text != "one\ntwo" {
		t.Errorf("answer = %+v, want the multi-line text sent by enter", ans)
	}

	// /m no longer switches modes; it points at the keystroke. (At a question
	// it is an answer; the affordance lives in the idle input path.)
	p.typeText("/m")
	p.enter()
	if !strings.Contains(p.transcript(), "shift+enter adds a newline") {
		t.Errorf("/m hint missing:\n%s", p.transcript())
	}
	if p.HasQuestion() {
		t.Error("/m must not open a question")
	}
}

// TestGUICommandAtPromptDoesNotDeadlock guards the regression that every
// slash command must leave the loop alive.
func TestGUICommandAtPromptDoesNotDeadlock(t *testing.T) {
	for _, cmd := range []string{"/y", "/c", "/u", "/h", "/t", "/m"} {
		t.Run(cmd, func(t *testing.T) {
			c := newConfig("confirm")
			c.ConfirmExit = false
			l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", submit("done")), Env: &env.Local{Timeout: 10 * time.Second}}
			p := newGUIForTest(t, c, l)
			done := make(chan struct{})
			go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()
			waitFor(t, "the confirmation", p.HasQuestion)
			p.typeText(cmd)
			p.enter()
			for i := 0; i < 6 && !isClosed(done); i++ {
				if p.HasQuestion() {
					if p.PendingQuestion().Kind == AskHuman && c.Mode == "human" {
						// Typed text no longer ends the run; switching out of
						// human mode lets the scripted no-action turn submit.
						p.typeText("/c")
					}
					p.enter()
				}
				time.Sleep(30 * time.Millisecond)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%q left the window deadlocked", cmd)
			}
		})
	}
}

func TestGUISteeringLandsInTheHistory(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second}}
	l.Model = &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}|{{ .output.output }}", Outputs: []model.Message{
		turnWithActions("/sleep 0.4"),
		turnWithActions("echo first-turn"),
		submit("done"),
	}}
	p := newGUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()
	waitFor(t, "the run to start", func() bool { return l.Running() })
	p.typeText("check the README first")
	p.enter()
	<-done
	if !containsContent(l.Messages(), "check the README first") {
		t.Errorf("steering never reached the history:\n%s", contentsOf(l.Messages()))
	}
	var idxUser, idxNext int
	for i, msg := range l.Messages() {
		if msg["role"] == "user" && strings.Contains(msg["content"].(string), "README") {
			idxUser = i
		}
		if i > idxUser && idxUser > 0 && msg["role"] == "assistant" {
			idxNext = i
			break
		}
	}
	if idxNext == 0 || idxNext < idxUser {
		t.Errorf("steering must be delivered before the next model call:\n%s", contentsOf(l.Messages()))
	}
	if !strings.Contains(p.transcript(), "(queued for the model's next turn)") {
		t.Errorf("steering echo missing:\n%s", p.transcript())
	}
	if got := strings.Count(p.transcript(), "check the README first"); got != 1 {
		t.Errorf("steering shows %d times, want the typed bubble only (commit must not duplicate it):\n%s", got, p.transcript())
	}
	if a := p.firstArea("user"); a == nil {
		t.Error("steering bubble is not a right-justified user area")
	}
}

// TestGUICardsLeaveScrollbarGutter: cards stop short of the scroll
// viewport's right edge. Fyne draws the transcript's vertical bar exactly
// there, and every scrollable card (tool grids, code tails, the docked live
// area) draws its own bar at its own right edge - without the inset they
// stack into one pixel column and grabbing the transcript bar is a coin
// flip on which scrollbar answers.
func TestGUICardsLeaveScrollbarGutter(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	g := scrollbarGutter()
	if g <= 0 {
		t.Fatal("theme reported no scrollbar size")
	}

	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("output line %d", i)
	}
	p.do(func() { p.v.appendBlock(block{kind: "tool", text: "out", transcript: lines}) })
	p.do(func() {
		bodyW := p.v.body.Size().Width
		if bodyW <= 0 {
			t.Fatal("the transcript body was never laid out")
		}
		for _, ref := range p.v.winRows {
			if w := ref.c.Size().Width; w > bodyW-g+0.5 {
				t.Errorf("row %d is %v wide in a %v body: no %v gutter for the scrollbar",
					ref.idx, w, bodyW, g)
			}
		}
	})

	// The live streaming row, now a row of the same scroll, keeps the same
	// clear strip.
	l.Handler.Content("streaming answer")
	p.do(func() {
		if !p.v.liveSeen || p.v.liveRow == nil {
			t.Fatal("the live row is not in the transcript scroll")
		}
		bodyW := p.v.body.Size().Width
		if w := p.v.liveRow.Size().Width; bodyW > 0 && w > bodyW-g+0.5 {
			t.Errorf("live row is %v wide in a %v body: no gutter", w, bodyW)
		}
	})
}

// TestGUIMockupLeavesScrollbarGutter holds the mockup window to the invariant
// above. The mockup renders through the production card code but lays its own
// column out, so it can put every card's right edge - and every scrollable
// card's own bar - exactly on top of the transcript scrollbar while the real app
// is fine. That is not cosmetic: the window exists so someone can judge the
// transcript's edges, and a reviewer who cannot tell which bar they are grabbing
// is judging the wrong thing. It got it wrong once, which is why it is pinned.
func TestGUIMockupLeavesScrollbarGutter(t *testing.T) {
	test.NewApp()

	m := newMockup()
	sc := container.NewVScroll(m.column)
	win := test.NewWindow(sc)
	win.Resize(fyne.NewSize(600, 800))

	g := scrollbarGutter()
	if g <= 0 {
		t.Fatal("theme reported no scrollbar size")
	}
	bodyW := sc.Size().Width
	if bodyW <= 0 {
		t.Fatal("the mockup's scroll was never laid out")
	}
	if len(m.cards) < 2 {
		t.Fatalf("the mockup rendered %d cards; it has stopped being a transcript", len(m.cards))
	}
	// A tool call is the card that grows a scrollbar of its own. If the samples
	// ever stop containing one, the whole test quietly stops proving anything.
	var sawScrolling bool
	for _, c := range m.cards {
		if c.label == "tool call" {
			sawScrolling = true
		}
	}
	if !sawScrolling {
		t.Error("no tool call card in the samples: nothing here has a scrollbar to collide with")
	}

	for i, c := range m.cards {
		if c.area.Size().Width <= 0 {
			t.Errorf("card %d (%s) was never sized", i, c.label)
			continue
		}
		if right := c.area.Position().X + c.area.Size().Width; right > bodyW-g+0.5 {
			t.Errorf("card %d (%s) reaches x=%v in a %v viewport: it intrudes on the transcript scrollbar's %v gutter",
				i, c.label, right, bodyW, g)
		}
	}
}

// TestGUIStreamedTurnRendersLikeTheCommittedTurn: the live preview carries the
// header from its first chunk and the commit replaces it exactly once.
func TestGUIStreamedTurnRendersLikeTheCommittedTurn(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	msg := model.Message{
		"role":              "assistant",
		"content":           "I will list the files.",
		"reasoning_content": "the user wants a listing of the current directory",
		"extra":             map[string]any{"actions": []model.Action{{Command: "echo hi", ToolCallID: "c"}}, "cost": 1.0},
	}
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second},
		Model: &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}", Outputs: []model.Message{msg}}}
	p := newGUIForTest(t, c, l)
	p.do(func() { p.prepareTranscript() })

	l.Handler.Reasoning("the user wants a ")
	l.Handler.Reasoning("listing of the current directory")
	l.Handler.Content("I will list the files.")

	mid := p.transcript()
	if got := strings.Count(mid, "LLM output"); got != 1 {
		t.Errorf("the in-flight turn should carry the area it will commit with, got %d:\n%s", got, mid)
	}
	if !strings.Contains(mid, hiddenThinkingMarker("the user wants a listing of the current directory")) {
		t.Errorf("in-flight thinking state not announced:\n%s", mid)
	}
	if !strings.Contains(mid, "I will list the files.") {
		t.Errorf("streamed content missing:\n%s", mid)
	}

	p.Message(msg)
	after := p.transcript()
	for _, want := range []string{"I will list the files.", "LLM output · $1.00"} {
		if got := strings.Count(after, want); got != 1 {
			t.Errorf("%q appears %d times after committing, want 1:\n%s", want, got, after)
		}
	}
	if strings.Contains(after, "listing of the current directory") {
		t.Errorf("the live thinking area survived the commit:\n%s", after)
	}
}

// TestGUITranscriptVirtualizesToTheViewport is the idle-burn regression:
// Fyne walks the live object tree every repaint frame and the driver
// repaints at frame rate regardless, so a long replayed session must not
// materialize its whole history as widgets. Only blocks near the viewport
// may exist; scrolling moves the window and releases what it leaves, while
// the scrollbar still describes the whole session.
func TestGUITranscriptVirtualizesToTheViewport(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)

	p.do(func() {
		for i := 0; i < 40; i++ {
			lines := make([]string, 8)
			for j := range lines {
				lines[j] = fmt.Sprintf("row %d.%d", i, j)
			}
			p.v.appendBlock(block{kind: "tool", text: fmt.Sprintf("block %d", i), transcript: lines})
		}
		if len(p.v.committed) != 40 {
			t.Fatalf("committed = %d", len(p.v.committed))
		}
		if rows := len(p.v.body.Objects) - 2; rows >= 40 {
			t.Errorf("%d rows materialized for 40 blocks: the tree never shrinks below the archive", rows)
		}
		if p.v.winFirst == 0 {
			t.Errorf("window starts at the top while pinned to the tail (first=%d last=%d)", p.v.winFirst, p.v.winLast)
		}
		if got := p.v.body.MinSize().Height; got <= p.v.scroll.Size().Height {
			t.Errorf("virtual content height %v does not exceed the viewport %v: no session to scroll", got, p.v.scroll.Size().Height)
		}
	})
	view := p.transcript()
	if strings.Contains(view, "row 0.0") {
		t.Error("the top of a long history is still materialized while viewing the tail")
	}
	if !strings.Contains(view, "row 39.") {
		t.Errorf("the newest block is missing from the pinned view:\n%s", view)
	}

	// Scrolling to the top moves the window and releases the tail. Use the
	// wheel path, not ScrollToTop: fyne's ScrollToOffset assigns Offset
	// directly and never fires OnScrolled (ScrollToBottom and real input do).
	p.do(func() { p.v.scroll.Scrolled(&fyne.ScrollEvent{Scrolled: fyne.NewDelta(0, 100000)}) })
	p.do(func() {
		if p.v.winFirst != 0 {
			t.Errorf("after ScrollToTop, window first = %d, want 0", p.v.winFirst)
		}
		if p.v.winLast >= 40 {
			t.Errorf("the tail never left the window (last=%d)", p.v.winLast)
		}
	})
	view = p.transcript()
	if !strings.Contains(view, "row 0.") {
		t.Errorf("top blocks missing after scrolling up:\n%s", view)
	}
	if strings.Contains(view, "row 39.") {
		t.Error("bottom blocks still in view after scrolling to the top")
	}

	// Scrolling moves the window; it does not rebuild it. Back near the
	// bottom, then a bounded scroll-up: the rows that stay in view must be
	// the same widget objects - wholesale re-materialization is exactly the
	// churn that turned the first virtualizer's memory profile into 725MB.
	p.do(func() { p.v.scroll.ScrollToBottom() })
	var tail fyne.CanvasObject
	p.do(func() {
		last := p.v.winRows[len(p.v.winRows)-1]
		if last.idx != 39 {
			t.Fatalf("window tail = %d after ScrollToBottom, want 39", last.idx)
		}
		tail = last.c
	})
	p.do(func() { p.v.scroll.Scrolled(&fyne.ScrollEvent{Scrolled: fyne.NewDelta(0, 300)}) })
	p.do(func() {
		last := p.v.winRows[len(p.v.winRows)-1]
		if last.c != tail {
			t.Error("a 300px scroll rebuilt the rows still in view: the window must move at its edges")
		}
	})

	// And back down: the archive re-materializes on demand, intact.
	p.do(func() { p.v.scroll.ScrollToBottom() })
	if view := p.transcript(); !strings.Contains(view, "row 39.") {
		t.Errorf("the tail did not return on scroll-down:\n%s", view)
	}
}

// TestGUITallContentNeverGrowsTheWindow is the regression for the serious
// one: a turn taller than the window - streaming or committed - must not
// resize it. The streaming turn is a row of the transcript scroll itself:
// it grows the scrollable content, the pinned view follows its tail, and the
// content tree's MinSize (what the glfw driver's fitContent would grow the
// window toward) stays far below the window's height.
func TestGUITallContentNeverGrowsTheWindow(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	tall := strings.Repeat("a streamed line of prose that keeps coming\n", 200)
	msg := model.Message{
		"role":    "assistant",
		"content": tall,
		"extra":   map[string]any{"actions": []model.Action{{Command: "echo hi", ToolCallID: "c"}}, "cost": 1.0},
	}
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second},
		Model: &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}", Outputs: []model.Message{msg}}}
	p := newGUIForTest(t, c, l)
	open := p.v.rootC.Size()
	if open.Height < 600 {
		t.Fatalf("the content must be laid out at the window's size, got %v", open)
	}
	height := func() float32 { return p.v.rootC.Size().Height }

	// A turn far taller than the whole window streams into the transcript
	// scroll itself: it grows the content the region scrolls, never the
	// region, and the pinned view follows its tail.
	l.Handler.Content(tall)
	p.do(func() {
		if !p.v.liveSeen {
			t.Error("the tall streaming row is not materialized in the transcript window")
		}
		if h := p.v.body.MinSize().Height; h <= p.v.scroll.Size().Height {
			t.Errorf("scroll content height %v does not exceed the viewport %v: the tall turn vanished",
				h, p.v.scroll.Size().Height)
		}
		if p.v.scroll.Offset.Y <= 0 {
			t.Errorf("the pinned transcript did not follow the stream (offset %v)", p.v.scroll.Offset.Y)
		}
		if min := p.v.rootC.MinSize().Height; min > height() {
			t.Errorf("the content tree's minimum is %v px, over the window's %v: fitContent would force it open", min, height())
		}
	})

	// Committing the same tall turn as a transcript card changes nothing:
	// the scroll region already absorbed it, window min still bounded.
	p.Message(msg)
	p.do(func() {
		if min := p.v.rootC.MinSize().Height; min > height() {
			t.Errorf("after the commit, content min height %v px exceeds the window's %v", min, height())
		}
	})
	if got := p.v.rootC.Size(); got != open {
		t.Errorf("the window's content resized to %v from %v without the user touching it", got, open)
	}
}

// TestGUILiveRowDoesNotResizeTranscript: the size of the scroll region is a
// function of the window, not of what streams through it. The dock era let a
// growing live area push the transcript viewport's top edge around mid-turn -
// the scrollbar jumping under the reader. Stream a tall turn (thinking and
// output) and commit it: the viewport height never moves while the scrollable
// content grows.
func TestGUILiveRowDoesNotResizeTranscript(t *testing.T) {
	c := newConfig("yolo")
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second}, Model: &model.Deterministic{}}
	p := newGUIForTest(t, c, l)
	p.setTask("T")
	p.do(func() { p.prepareTranscript() })
	var vp0, content0 float32
	p.do(func() {
		vp0 = p.v.scroll.Size().Height
		content0 = p.v.body.MinSize().Height
	})
	if vp0 <= 0 {
		t.Fatal("no transcript viewport geometry to hold constant")
	}

	l.Handler.Reasoning("pondering\n" + strings.Repeat("x\n", 60))
	l.Handler.Content("answering\n" + strings.Repeat("y\n", 60))
	p.do(func() {
		if got := p.v.scroll.Size().Height; got != vp0 {
			t.Errorf("streaming resized the transcript viewport from %v to %v", vp0, got)
		}
		if h := p.v.body.MinSize().Height; h <= content0 {
			t.Errorf("scroll content did not grow with the stream: %v -> %v", content0, h)
		}
		if !p.v.liveSeen {
			t.Error("the live row is not in the transcript window while streaming")
		}
	})

	p.Message(model.Message{"role": "assistant", "content": "answering\n" + strings.Repeat("y\n", 60),
		"reasoning_content": "pondering\n" + strings.Repeat("x\n", 60),
		"extra":             map[string]any{"cost": 1.0}})
	p.do(func() {
		if got := p.v.scroll.Size().Height; got != vp0 {
			t.Errorf("the commit resized the transcript viewport from %v to %v", vp0, got)
		}
		if p.v.liveSeen {
			t.Error("the live row survived its commit")
		}
	})
}

// TestGUILiveMarkdownRendersAsItStreams: the whole point of the incremental
// parse - a streamed reply shows rendered markdown, not raw syntax. Headings
// and emphasis never show their markers, and an unterminated fence previews
// as code until it closes into a folded card.
func TestGUILiveMarkdownRendersAsItStreams(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)

	l.Handler.Content("## Plan\n\nIntro **bold** text\n\n```go\nfmt.Println(1)\n")
	mid := p.transcript()
	for _, raw := range []string{"## ", "**", "```"} {
		if strings.Contains(mid, raw) {
			t.Errorf("raw markdown %q visible mid-stream:\n%s", raw, mid)
		}
	}
	for _, want := range []string{"Plan", "bold", "fmt.Println(1)"} {
		if !strings.Contains(mid, want) {
			t.Errorf("live render missing %q:\n%s", want, mid)
		}
	}

	// Closing the fence turns the preview into the folded card; opening and
	// closing do not double-render.
	l.Handler.Content("```\n\nAll done.")
	after := p.transcript()
	if !strings.Contains(after, "[go · 1 line]") {
		t.Errorf("closed fence did not become a code card:\n%s", after)
	}
	if !strings.Contains(after, "All done.") || strings.Contains(after, "```") {
		t.Errorf("tail after fence close wrong:\n%s", after)
	}
	if n := strings.Count(after, "fmt.Println(1)"); n != 1 {
		t.Errorf("code line rendered %d times, want 1:\n%s", n, after)
	}
}

// TestGUIThinkingStateIsLegible: hidden by default with its size named,
// expanded by the toggle, always labelled.
func TestGUIThinkingStateIsLegible(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	reasoning := strings.Repeat("pondering ", 30)
	// The turn makes no tool call, which is the contract's way of ending the
	// run; the test only cares how the committed thinking renders.
	msg := model.Message{
		"role":              "assistant",
		"content":           "the plan",
		"reasoning_content": reasoning,
		"extra":             map[string]any{"cost": 1.0},
	}
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second},
		Model: &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}", Outputs: []model.Message{msg}}}
	p := newGUIForTest(t, c, l)
	p.do(func() { p.prepareTranscript() })
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.status(), "thinking: off") {
		t.Errorf("footer does not report the thinking state:\n%s", p.status())
	}
	if !strings.Contains(p.transcript(), hiddenThinkingMarker(reasoning)) {
		t.Errorf("hidden thinking is not announced:\n%s", p.transcript())
	}
	if strings.Contains(p.transcript(), "pondering") {
		t.Error("thinking text leaked while hidden")
	}

	p.shortcut(fyne.KeyT, thinkingMods())
	if !strings.Contains(p.status(), "thinking: on") {
		t.Errorf("footer still says thinking is off:\n%s", p.status())
	}
	scroll := p.transcript()
	if !strings.Contains(scroll, "thinking · $1.00") || !strings.Contains(scroll, "pondering") {
		t.Errorf("toggling did not re-render the turn:\n%s", scroll)
	}
	if strings.Contains(scroll, "hidden") {
		t.Errorf("the hidden marker survived the toggle:\n%s", scroll)
	}
}

// TestGUIResumeReplaysStoredTranscript: the replay shares messageBlock with
// live turns, so prompt plumbing stays hidden and the observation renders
// from its result file.
func TestGUIResumeReplaysStoredTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.log")
	if err := os.WriteFile(path, []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newGUIForTest(t, c, l)
	p.SetSessionID("01a1dead-beef-7000-8000-000000000000")
	p.Resume([]model.Message{
		{"role": "system", "content": "sys prompt"},
		{"role": "user", "content": "rendered instance"},
		{"role": "assistant", "content": "I will echo", "extra": map[string]any{"turn": int64(2)}},
		{"role": "tool", "content": `{"returncode": 0, "output": "hi\n"}`, "tool_call_id": "c",
			"extra": map[string]any{"command": "echo hi", "returncode": 0, "tcr_path": path, "turn": int64(3)}},
	}, "resumed session 01a1dead (4 turns)")
	p.do(func() { p.prepareTranscript() })

	all := p.transcript()
	if strings.Contains(all, "sys prompt") || strings.Contains(all, "rendered instance") {
		t.Errorf("prompt plumbing replayed into the transcript:\n%s", all)
	}
	for _, want := range []string{
		"I will echo", "$ echo hi", "resumed session 01a1dead (4 turns)",
		"session 01a1dead-beef-7000-8000-000000000000",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("replay missing %q:\n%s", want, all)
		}
	}
}

// TestGUITrajectoryMatchesPrintMode: the same scripted model through the GUI
// and through --print must produce the same trajectory.
func TestGUITrajectoryMatchesPrintMode(t *testing.T) {
	commands := []any{"echo hi", "echo same", submit("done")}

	guiPath := filepath.Join(t.TempDir(), "gui.json")
	c1 := newConfig("confirm")
	c1.OutputPath = guiPath
	l1 := &agent.Loop{Cfg: c1, Model: outputs(t, commands...), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c1, l1)
	done := make(chan struct{})
	go func() { _, _ = l1.Run(context.Background(), "T"); close(done) }()
	waitFor(t, "the first confirmation", p.HasQuestion)
	p.enter()
	waitFor(t, "the second confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskConfirm
	})
	p.enter()
	waitFor(t, "the exit confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskNewTask
	})
	p.enter() // no-op on every frontend; the print side's last line is EOF
	p.typeText("/quit")
	p.enter()
	<-done

	printPath := filepath.Join(t.TempDir(), "print.json")
	c2 := newConfig("confirm")
	c2.OutputPath = printPath
	l2, _ := pair(t, c2, commands, &env.Local{Timeout: 10 * time.Second}, "\n\n\n", true)
	if _, err := l2.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}

	a := normalizeTraj(t, guiPath)
	b := normalizeTraj(t, printPath)
	if a != b {
		t.Errorf("trajectories differ\n gui: %s\nprint: %s", a, b)
	}
}

// TestGUITranscriptIsAShellSessionNotJSON: command output renders as exit
// code plus text, never the model's JSON view.
func TestGUITranscriptIsAShellSessionNotJSON(t *testing.T) {
	dir := t.TempDir()
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo one; echo two; exit 3", submit("done")),
		Env: &env.Local{CWD: dir, Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	p.do(func() { p.prepareTranscript() })
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	scroll := p.transcript()
	for _, want := range []string{"$ echo one; echo two; exit 3", "  in " + dir, "exit 3", "\none\n", "\ntwo\n"} {
		if !strings.Contains(scroll, want) {
			t.Errorf("transcript missing %q:\n%s", want, scroll)
		}
	}
	for _, notWant := range []string{`"returncode"`, `"output"`, `"raw_output"`} {
		if strings.Contains(scroll, notWant) {
			t.Errorf("transcript leaks the model's JSON view (%s):\n%s", notWant, scroll)
		}
	}
}

// TestGUIToolCardScrollsPastTenLines: a tool card grows to about ten lines
// and no more - a taller result scrolls inside its card instead of pushing
// the transcript around it. A short result is exactly its content.
func TestGUIToolCardScrollsPastTenLines(t *testing.T) {
	lineH := float32(0)
	{
		one := transcriptGrid([]string{"a"}).(*container.Scroll).Content.(*widget.RichText)
		lineH = one.MinSize().Height
	}
	short := transcriptGrid([]string{"1", "2", "3"}).(*container.Scroll)
	if got := short.MinSize().Height; got > 3*lineH+1 {
		t.Errorf("3-line card min height = %v, want ~%v: short results must not pad to the cap", got, 3*lineH)
	}

	long := transcriptGrid(make([]string, 40)).(*container.Scroll)
	if got, capH := long.MinSize().Height, 10*lineH; got > capH+1 {
		t.Errorf("40-line card min height = %v, want the 10-line cap %v", got, capH)
	}
	if content := long.Content.(*widget.RichText).MinSize().Height; content <= long.MinSize().Height {
		t.Errorf("40-line content height %v is not above its %v viewport: nothing would scroll",
			content, long.MinSize().Height)
	}
	// The single-widget promise behind the cap: the body is one text widget, so
	// capping is one MinSize, not a walk over materialized rows - and it is the
	// selectable RichText the cards need, so the cap never costs the reader a
	// copy. See TestGUICardTextSelectsAndCopies, which checks the promise that
	// matters rather than this type assertion.
	if _, ok := long.Content.(*widget.RichText); !ok {
		t.Errorf("card content = %T, want a single selectable RichText", long.Content)
	}
}

// setTask mirrors drive()'s publication of the task before loop.Run, which is
// what messageBlock's task note reads.
func (p *guiPane) setTask(task string) {
	p.GUI.mu.Lock()
	p.GUI.task = task
	p.GUI.mu.Unlock()
}

// TestGUILiveCardsAreCreatedAndGrownPerTurn: each turn grows its own live
// area, and each commit leaves those cards behind and starts the next turn
// fresh - never one pair that accumulates the whole session.
func TestGUILiveCardsAreCreatedAndGrownPerTurn(t *testing.T) {
	c := newConfig("yolo")
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second}, Model: &model.Deterministic{}}
	p := newGUIForTest(t, c, l)
	p.setTask("T")
	p.do(func() { p.prepareTranscript() })

	msg1 := model.Message{"role": "assistant", "content": "first answer",
		"extra": map[string]any{"actions": []model.Action{{Command: "echo hi", ToolCallID: "c"}}, "cost": 1.0}}
	l.Handler.Content("first ans")
	if got := strings.Count(p.transcript(), "LLM output"); got != 1 {
		t.Fatalf("turn 1 streaming: output areas = %d, want 1:\n%s", got, p.transcript())
	}
	p.Message(msg1)
	after1 := p.transcript()
	if !strings.Contains(after1, "first answer") || strings.Contains(after1, "first ans\n") {
		t.Errorf("commit must replace the live text, not append:\n%s", after1)
	}

	p.Message(model.Message{"role": "tool", "content": "ignored", "tool_call_id": "c",
		"extra": map[string]any{"command": "echo hi", "returncode": 0}})
	if got := strings.Count(p.transcript(), "tool call"); got != 1 {
		t.Fatalf("tool areas = %d, want 1:\n%s", got, p.transcript())
	}

	msg2 := model.Message{"role": "assistant", "content": "second answer",
		"extra": map[string]any{"actions": []model.Action{{Command: "echo done", ToolCallID: "c"}}, "cost": 1.0}}
	l.Handler.Content("second ans")
	mid := p.transcript()
	if got := strings.Count(mid, "LLM output"); got != 2 {
		t.Errorf("turn 2 streaming: output areas = %d, want committed + live:\n%s", got, mid)
	}
	if !strings.Contains(mid, "first answer") {
		t.Errorf("turn 1's committed card vanished when turn 2 streamed:\n%s", mid)
	}
	p.Message(msg2)
	end := p.transcript()
	if got := strings.Count(end, "LLM output"); got != 2 {
		t.Errorf("after the last commit output areas = %d, want 2 (no accumulation):\n%s", got, end)
	}
	if !strings.Contains(end, "second answer") || strings.Contains(end, "second ans\n") {
		t.Errorf("turn 2 did not commit in place of its live area:\n%s", end)
	}
}

// TestGUITaskIsARightJustifiedUserEntry: the un-built instance row never
// shows; the task typed into the input box reads as the first user bubble.
func TestGUITaskIsARightJustifiedUserEntry(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, submit("done")), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	p.setTask("fix the flaky test")
	if _, err := l.Run(context.Background(), "fix the flaky test"); err != nil {
		t.Fatal(err)
	}
	scroll := p.transcript()
	if !strings.Contains(scroll, "fix the flaky test") {
		t.Errorf("task entry missing:\n%s", scroll)
	}
	if strings.Contains(scroll, "task=fix") {
		t.Errorf("the rendered instance template leaked into the transcript:\n%s", scroll)
	}
	if card := p.firstArea("user"); card == nil {
		t.Fatal("no user area")
	}
}

// TestGUIEmptyTurnHasNoBubble: a tool-call-only assistant turn carries no
// prose, so it must not render an empty bubble - its result is the tool call.
func TestGUIEmptyTurnHasNoBubble(t *testing.T) {
	if cards := blockCards(block{kind: "assistant", text: "", reasoning: ""}, false); len(cards) != 0 {
		t.Errorf("empty assistant turn rendered %d bubbles, want 0", len(cards))
	}
	// with reasoning only, the thinking bubble shows but not an empty output.
	one := blockCards(block{kind: "assistant", text: "", reasoning: "hmm"}, false)
	if len(one) != 1 || one[0].(*area).label != "thinking" {
		t.Errorf("reasoning-only turn rendered %d bubbles, want just thinking", len(one))
	}
}

// TestGUIAreaHeadersAndAlignment is the design spec in one test: every
// message kind gets its own outlined speech-bubble area under a header naming
// the kind and the message's tokens / generation time / cost (zeros omitted),
// in the body's own font; text the human typed trails on the right,
// harness-authored feedback and everything else stays left.
func TestGUIAreaHeadersAndAlignment(t *testing.T) {
	if got := cardHeaderText("LLM output", metrics{Tokens: 254, Elapsed: 1.5, Cost: 0.03}); got != "LLM output · 254 tok · 1.5s · $0.03" {
		t.Errorf("header = %q", got)
	}
	if got := cardHeaderText("user", metrics{}); got != "user" {
		t.Errorf("zero-metric header = %q, want just the kind", got)
	}

	user := blockCards(block{kind: "user", text: "do this"}, false)
	if len(user) != 1 {
		t.Fatalf("user entry became %d areas", len(user))
	}
	uc := user[0].(*area)
	if got := headerText(uc); got != "user" {
		t.Errorf("user header = %q", got)
	}
	if _, outline, _ := bubblePaint(uc); !opaque(outline) {
		t.Error("user bubble outline must be opaque: a hue with no alpha paints an invisible frame")
	}
	if fill, _, _ := bubblePaint(uc); !sameColor(fill, theme.Color(theme.ColorNameBackground)) {
		t.Error("a bubble's interior must be the plain theme surface - the outline carries the kind now")
	}
	wrap, align := bodyTextProps(uc.body)
	if wrap != fyne.TextWrapWord {
		t.Error("user body must wrap words")
	}
	if !uc.bubble.mine {
		t.Error("user bubble must point its tail to our side")
	}
	if align != fyne.TextAlignTrailing {
		t.Error("user text is not right-justified")
	}

	// Harness-authored feedback with the same role stays on the left under a
	// distinct kind name, colour and bubble side.
	hb := blockCards(block{kind: "user", text: "Commands not executed. The user rejected your commands with the following message: use printf", subtype: "UserRejection"}, false)
	hc := hb[0].(*area)
	if got := headerText(hc); got != "harness" {
		t.Errorf("harness header = %q", got)
	}
	if hc.bubble.mine {
		t.Error("harness feedback must not sit on the user's side")
	}
	_, ho, _ := bubblePaint(hc)
	_, uo, _ := bubblePaint(uc)
	if sameColor(ho, uo) {
		t.Error("harness and user bubbles need distinguishable outlines")
	}
	// The interiors must NOT differ: one surface for every kind is the whole
	// point of moving the colour into the frame.
	hf, _, _ := bubblePaint(hc)
	uf, _, _ := bubblePaint(uc)
	if !sameColor(hf, uf) {
		t.Error("kind colours must not leak back into the bubble interiors")
	}
	if _, al := bodyTextProps(hc.body); al != fyne.TextAlignLeading {
		t.Error("harness feedback must be left-justified")
	}

	asst := blockCards(block{kind: "assistant", text: "out", reasoning: "why", stats: metrics{Tokens: 7}}, true)
	if len(asst) != 2 {
		t.Fatalf("assistant turn became %d areas, want thinking + LLM output", len(asst))
	}
	if got := headerText(asst[0].(*area)); got != "thinking · 7 tok" {
		t.Errorf("thinking header = %q", got)
	}
	if got := headerText(asst[1].(*area)); got != "LLM output · 7 tok" {
		t.Errorf("output header = %q", got)
	}
	if _, al := bodyTextProps(asst[1].(*area).body); al != fyne.TextAlignLeading {
		t.Error("LLM output must stay left-justified")
	}

	tool := blockCards(block{kind: "tool", transcript: []string{"$ echo hi", "exit 0"}}, false)
	if got := headerText(tool[0].(*area)); got != "tool call" {
		t.Errorf("tool header = %q", got)
	}

	// The header is the body's plain font: no bold, no size override.
	if seg := uc.header.Segments[0].(*widget.TextSegment); seg.Style.TextStyle.Bold || seg.Style.SizeName != "" {
		t.Error("area header must use the body's font, unstyled")
	}

	// And no kind colour either, for every kind: the outline is the marker, and
	// a coloured header would repeat it where it competes with the text. Pinned
	// because areaColor takes a kind label and ignores it - which reads as
	// unfinished work unless a test says it is a decision. The day a per-kind
	// header hue is wanted, that function is the one place to make it and this
	// assertion is the thing that has to change on purpose.
	for _, a := range []*area{uc, hc, asst[0].(*area), asst[1].(*area), tool[0].(*area)} {
		if got := headerColor(a); got != theme.ColorNameForeground {
			t.Errorf("header of %q is %q, want the plain foreground", headerText(a), got)
		}
	}
}

// TestGUIBubbleOutlinesPaint is the only check that a colour decision became a
// painted line. Setting StrokeColor on a polygon is not the same as drawing a
// frame, and there are two ways to get it wrong with every field looking right:
// a hue with no alpha (the accent literals are three-field composites, so A
// silently defaults to 0), and a stroke width that rounds under a pixel. The
// first cut of tinted fills rendered effectively invisible for exactly this
// class of reason, so this renders a real area through a painter and reads the
// pixels back - and it also pins the redesign itself: the hue must form a ring
// around the card, never a surface inside it.
func TestGUIBubbleOutlinesPaint(t *testing.T) {
	test.NewApp()

	uc := blockCards(block{kind: "user", text: "hello there my friend"}, false)[0].(*area)
	win := test.NewWindow(uc)
	win.Resize(fyne.NewSize(260, 70))
	img := win.Canvas().Capture()
	bd := img.Bounds()

	if w := uc.bubble.shape.StrokeWidth; w < 1 {
		t.Errorf("stroke width = %v, want at least a point (a hairline frame is the invisible bubble again)", w)
	}

	want := bubbleAccent["user"]
	edge, centre := 0, 0
	x1, x2 := bd.Min.X+bd.Dx()*2/5, bd.Min.X+bd.Dx()*3/5
	y1, y2 := bd.Min.Y+bd.Dy()*2/5, bd.Min.Y+bd.Dy()*3/5
	for y := bd.Min.Y; y < bd.Max.Y; y++ {
		for x := bd.Min.X; x < bd.Max.X; x++ {
			if !sameColor(img.At(x, y), want) {
				continue
			}
			if x >= x1 && x < x2 && y >= y1 && y < y2 {
				centre++
			} else {
				edge++
			}
		}
	}
	if edge == 0 {
		t.Error("the outline painted nothing: not one pixel of the kind's accent hue reached the canvas")
	}
	if centre > 0 {
		t.Errorf("%d accent pixels sit in the middle of the card: the interior is filled with the kind colour again", centre)
	}
	// A frame is a small fraction of the card's pixels. If the hue ever covers
	// most of the area, whoever did it put the colour back in the background.
	if total := bd.Dx() * bd.Dy(); edge*4 > total {
		t.Errorf("%d accent pixels is more than a quarter of the card (%d): that is a fill, not an outline", edge, total)
	}
}

func headerText(a *area) string {
	return a.header.Segments[0].(*widget.TextSegment).Text
}

func headerColor(a *area) fyne.ThemeColorName {
	return a.header.Segments[0].(*widget.TextSegment).Style.ColorName
}

// bubblePaint returns how an area's bubble will paint: interior colour,
// outline colour and outline weight. Tests used to read FillColor alone,
// because the fill *was* the kind marker; now the fill is the theme surface for
// every kind and reading it proves nothing, so the assertions sit on the stroke
// - which is also where an invisible bubble would come from this time.
func bubblePaint(a *area) (fill, outline color.Color, stroke float32) {
	sh := a.bubble.shape
	return sh.FillColor, sh.StrokeColor, sh.StrokeWidth
}

// sameColor compares two colours by their painted values, not their types: a
// theme lookup and a literal NRGBA of the same hue must read as equal.
func sameColor(x, y color.Color) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	xr, xg, xb, xa := x.RGBA()
	yr, yg, yb, ya := y.RGBA()
	return xr == yr && xg == yg && xb == yb && xa == ya
}

// opaque reports whether a colour will actually paint: the failure this file
// keeps recording is a colour that is set correctly and drawn invisibly.
func opaque(c color.Color) bool {
	if c == nil {
		return false
	}
	_, _, _, a := c.RGBA()
	return a == 0xffff
}

// firstArea finds a committed area by kind label.
func (p *guiPane) firstArea(label string) *area {
	var found *area
	p.do(func() {
		var walk func(o fyne.CanvasObject) bool
		walk = func(o fyne.CanvasObject) bool {
			if a, ok := o.(*area); ok {
				if a.label == label {
					found = a
					return true
				}
				o = a.stack
			}
			if c, ok := o.(*fyne.Container); ok {
				for _, ch := range c.Objects {
					if walk(ch) {
						return true
					}
				}
			}
			return false
		}
		for _, o := range p.v.body.Objects {
			if walk(o) {
				return
			}
		}
	})
	return found
}

// TestGUIShowErrorPresentsTheFailure: a setup failure still gets a window -
// the only surface the user chose - instead of an unexplained exit.
func TestGUIShowErrorPresentsTheFailure(t *testing.T) {
	q := newTestQueue(t)
	g := NewGUI(GUIOptions{App: test.NewApp(), Post: q.add})
	p := attachPane(g, q)
	if code := g.ShowError(errors.New("no model set. Pass -m NAME or run `mini config setup`.")); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	scroll := p.transcript()
	if !strings.Contains(scroll, "mini: no model set") {
		t.Errorf("error missing from the window:\n%s", scroll)
	}
	if !strings.Contains(scroll, "close the window or type /quit to leave") {
		t.Errorf("leaving instructions missing:\n%s", scroll)
	}
}

// TestGUIShowRunDoneLifecycle covers the two endings: clean finishes append
// "run finished", failures keep the window with its leaving instructions.
func TestGUIShowRunDoneLifecycle(t *testing.T) {
	c := newConfig("yolo")
	p := newGUIForTest(t, c, &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}})
	// finish posts showRunDone: with the serialized post that is a direct
	// call, so never invoke it while holding the pane lock (fyne.Do would
	// merely queue it in production; sync.Mutex is not reentrant).
	p.finish(map[string]any{"exit_status": "Submitted"}, nil)
	if !strings.Contains(p.transcript(), "run finished: Submitted") {
		t.Errorf("clean finish note missing:\n%s", p.transcript())
	}

	c2 := newConfig("yolo")
	p2 := newGUIForTest(t, c2, &agent.Loop{Cfg: c2, Model: &model.Deterministic{}, Env: &env.Local{}})
	p2.finish(nil, context.Canceled)
	scroll := p2.transcript()
	if !strings.Contains(scroll, "run ended: context canceled") {
		t.Errorf("failed run must show the error:\n%s", scroll)
	}
	if !strings.Contains(scroll, "type /quit or press "+quitKeyName()+" to leave") {
		t.Errorf("leaving instructions missing:\n%s", scroll)
	}
}

// TestGUIRunningCardIsPaintedAtIssueTime is the regression the user hit: a long
// foreground command produced nothing on screen until it returned, so an
// ordinary two-minute build was indistinguishable from a hung UI and they
// stopped a session that was working. The card has to exist while the command
// runs, not when its result lands.
// toolCards is the committed tool cards, with their lifecycle states, which is
// what this test asserts on: the transcript's rendered text cannot tell a card that
// was upgraded from one that was duplicated.
func toolCards(p *guiPane) []block {
	var out []block
	p.do(func() {
		for _, b := range p.v.committed {
			if b.kind == "tool" {
				out = append(out, b)
			}
		}
	})
	return out
}

// TestGUIToolCardIsPaintedAtIssueTime is the promise the running card existed to
// keep, in its new shape: a call is visible from the moment it is issued, before
// the job backing it exists, and the same card carries the result when it lands.
// It used to live in a panel at the bottom of the window, where a long command's
// output could fill the screen and cover the tab strip; now it is one card in the
// column, and one card only.
func TestGUIToolCardIsPaintedAtIssueTime(t *testing.T) {
	c := newConfig("yolo")
	// No exit prompt: this test wants the run to finish at the submission, and
	// confirm_exit would park the loop on a question nobody is answering.
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "sleep 0.7 && echo late", submit("done")),
		Env: &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()

	waitFor(t, "the tool card while the command is still in flight", func() bool {
		for _, b := range toolCards(p) {
			if strings.Contains(strings.Join(b.transcript, "\n"), "sleep 0.7") &&
				(b.state == "created" || b.state == "running") {
				return true
			}
		}
		return false
	})
	// And nothing else came up to say it: the panel belongs to questions now.
	if p.panelShown() {
		t.Error("an in-flight call put a panel in the window")
	}

	<-done
	waitFor(t, "the result to land in that card", func() bool {
		cards := toolCards(p)
		return len(cards) == 1 && cards[0].state == "finished"
	})
	if cards := toolCards(p); len(cards) != 1 {
		t.Errorf("%d tool cards for one call: %+v", len(cards), cards)
	}
	for _, b := range toolCards(p) {
		if strings.Contains(strings.Join(b.transcript, "\n"), "issued") {
			t.Errorf("the card never left its created state: %v", b.transcript)
		}
	}
}

// TestGUIRunningCardYieldsToAPrompt pins the priority: the running card and a
// question share the panel, and a prompt the human has not answered yet is
// strictly more urgent than a status. Silently replacing a confirm question with
// a running card would strand the run behind a panel that no longer asks.
func TestGUIRunningCardYieldsToAPrompt(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}}
	p := newGUIForTest(t, c, l)
	p.do(func() { p.prepareTranscript() })

	asked := make(chan struct{})
	go func() {
		_, _ = p.Ask(context.Background(), Question{Kind: AskConfirm, Title: "confirm one command"})
		close(asked)
	}()
	waitFor(t, "the prompt to reach the panel", p.HasQuestion)
	// The issued step now tries to paint its running card over the prompt.
	p.IssueCalls([]model.Action{{Command: "echo later"}})
	p.do(func() {}) // drain the posted closures, both ways

	if got := p.panel(); !strings.Contains(got, "confirm one command") {
		t.Errorf("the running card displaced the prompt; panel reads:\n%s", got)
	} else if strings.Contains(got, "running") {
		t.Errorf("the prompt was overwritten:\n%s", got)
	}
	p.enter() // empty answer confirms a pending AskConfirm
	<-asked
}

// selector is the part of Fyne's internal selection a test has to drive by hand:
// widget.selectable is unexported, so the only way in from outside package widget
// is the child object every selectable text widget puts in its own renderer.
type selector interface {
	MouseDown(*desktop.MouseEvent)
	Dragged(*fyne.DragEvent)
	DragEnd()
	TypedShortcut(fyne.Shortcut)
	SelectedText() string
}

// selectionOf finds a selectable text widget's selection child.
func selectionOf(t *testing.T, w fyne.CanvasObject) selector {
	t.Helper()
	for _, o := range test.WidgetRenderer(w.(fyne.Widget)).Objects() {
		if s, ok := o.(selector); ok {
			return s
		}
	}
	t.Fatalf("%T has no selectable child: Selectable was not set, or Fyne moved the selection widget", w)
	return nil
}

// dragSelect selects text across an already-laid-out text widget with the mouse
// and returns what the widget says is selected.
//
// Two traps this sidesteps, both of which reported the feature broken when it was
// fine: the widget must be sized and refreshed first, because a RichText with
// stale row bounds collapses every position onto one character; and Dragged must
// carry the pointer's *position*, not just the delta, because the selection tracks
// the pointer and an anchor-to-anchor drag selects nothing.
func dragSelect(t *testing.T, w fyne.CanvasObject, width, fromX, toX float32) string {
	t.Helper()
	w.Resize(fyne.NewSize(width, 60))
	w.Refresh()
	s := selectionOf(t, w)
	s.MouseDown(&desktop.MouseEvent{Button: desktop.MouseButtonPrimary,
		PointEvent: fyne.PointEvent{Position: fyne.NewPos(fromX, 4)}})
	s.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(toX, 4)},
		Dragged: fyne.Delta{DX: toX - fromX, DY: 0}})
	s.DragEnd()
	return s.SelectedText()
}

// TestGUICardTextSelectsAndCopies drives a real selection through the widget the
// transcript cards actually build.
//
// It exists because the first attempt at this feature was wrong in a way its own
// test could not see: it swapped monoBlock to a selectable Label, asserted that
// monoBlock returned a selectable Label, and passed - while the cards kept
// rendering through transcriptGrid, which built its own RichText. Selection did
// not exist on RichText then at all, so nothing the user clicked could be copied
// and every test stayed green. A test that checks the widget a helper returns is a
// test of the helper; this one goes to the card, selects in it the way a mouse
// does, and asks the clipboard what arrived.
func TestGUICardTextSelectsAndCopies(t *testing.T) {
	c := newConfig("yolo")
	p := newGUIForTest(t, c, &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}})
	// A span of real length, not the whole line: the point is that a mouse
	// gesture produces a selection at all and that copy moves it. The exact
	// column a headless layout resolves x=112 to depends on the scroll's
	// measured width, and pinning it would make this test about Fyne's metrics.
	want := "echo"

	p.do(func() {
		sc, ok := transcriptGrid([]string{"$ echo path/to/the/thing", "path/to/the/thing"}).(*container.Scroll)
		if !ok {
			t.Fatal("transcriptGrid is not a scroller")
		}
		rt, ok := sc.Content.(*widget.RichText)
		if !ok {
			t.Fatalf("card body is %T, want a selectable *widget.RichText", sc.Content)
		}
		if !rt.Selectable {
			t.Fatal("card body RichText is not Selectable")
		}
		if !rt.Segments[0].(*widget.TextSegment).Style.TextStyle.Monospace {
			t.Error("card body lost monospace")
		}

		got := dragSelect(t, rt, 320, 2, 72)
		if len([]rune(got)) < 5 {
			t.Fatalf("selection was %q: a one-character span means the gesture is not reaching the text", got)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("mouse selection gave %q, want it to contain %q", got, want)
		}
		// And copy moves it: the same handler ⌘C reaches once mouse-down has taken
		// focus onto the selection.
		selectionOf(t, rt).TypedShortcut(&fyne.ShortcutCopy{})
		if cb := p.sh.app.Clipboard().Content(); !strings.Contains(cb, want) {
			t.Errorf("clipboard holds %q, want %q", cb, want)
		}
	})
}

// TestGUIMarkdownCardSelectsRenderedText is the point of the selectable RichText:
// a styled paragraph used to be either its formatting or the clipboard, and the
// mouse now gives up the text as rendered - emphasis included, markers excluded.
func TestGUIMarkdownCardSelectsRenderedText(t *testing.T) {
	c := newConfig("yolo")
	p := newGUIForTest(t, c, &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}})
	text := "the build passed cleanly and `make test` ran every suite in the repo today"

	p.do(func() {
		cards := blockCards(block{kind: "assistant", text: text}, false)
		a, ok := cards[0].(*area)
		if !ok {
			t.Fatalf("assistant card is %T, want an area", cards[0])
		}
		rt, ok := a.body.(*widget.RichText)
		if !ok {
			t.Fatalf("body is %T, want the markdown RichText", a.body)
		}
		if !rt.Selectable {
			t.Fatal("the markdown body is not Selectable")
		}
		got := dragSelect(t, rt, 320, 2, 300)
		if !strings.Contains(got, "build passed cleanly") {
			t.Fatalf("selection gave %q, want the paragraph's prose", got)
		}
		for _, marker := range []string{"`", "*", "_"} {
			if strings.Contains(got, marker) {
				t.Errorf("selection carried the markdown marker %q instead of the rendered text: %q", marker, got)
			}
		}
	})
}

// TestGUICopyMenuCopiesAndReturnsFocus covers Edit ▸ Copy on a live card, which is
// the only copy path that works once a card's selection has taken the focus: glfw
// hands a shortcut to the focused widget and stops, so neither the input nor a
// canvas-level handler would ever see ⌘C here.
func TestGUICopyMenuCopiesAndReturnsFocus(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c,
		Model: outputs(t, submit("the **build** passed cleanly after the flaky test was rewritten today")),
		Env:   &env.Local{Timeout: 10 * time.Second}}
	p := newGUIForTest(t, c, l)
	p.setTask("T")
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(p.transcript(), "build passed cleanly") {
		t.Fatalf("the assistant card never reached the transcript:\n%s", p.transcript())
	}

	var copyItem *fyne.MenuItem
	for _, m := range p.v.win.MainMenu().Items {
		for _, it := range m.Items {
			if it.Label == "Copy" {
				copyItem = it
			}
		}
	}
	if copyItem == nil {
		t.Fatal("no Edit > Copy menu item: ⌘C cannot reach a card holding the focus")
	}
	if copyItem.Shortcut == nil || copyItem.Shortcut.ShortcutName() != "Copy" {
		t.Errorf("Copy's chord is %v, want the platform copy shortcut", copyItem.Shortcut)
	}

	// firstArea posts, so it must run outside the do() below: the pane's mutex is
	// not reentrant, and calling it while holding the lock self-deadlocks.
	a := p.firstArea("LLM output")
	if a == nil {
		t.Fatal("no LLM output area to select in")
	}
	p.do(func() {
		rt, ok := a.body.(*widget.RichText)
		if !ok {
			t.Fatalf("live card body is %T, want the markdown RichText", a.body)
		}
		p.v.win.Canvas().Focus(p.v.editor) // typing works before the drag
		if got := dragSelect(t, rt, 300, 2, 220); !strings.Contains(got, "build passed cleanly") {
			t.Fatalf("live card selection gave %q", got)
		}
		copyItem.Action()

		if cb := p.sh.app.Clipboard().Content(); !strings.Contains(cb, "build passed cleanly") {
			t.Errorf("Copy put %q on the clipboard", cb)
		} else if strings.Contains(cb, "**") {
			t.Errorf("Copy took the markers, not the rendering: %q", cb)
		}
		// The keyboard has to come back: a card holding focus swallows every
		// keystroke, and the input being typeable is not optional.
		if p.v.win.Canvas().Focused() != fyne.Focusable(p.v.editor) {
			t.Errorf("focus stayed on %v after Copy; typing would be swallowed", p.v.win.Canvas().Focused())
		}
		// The promise behind handing the focus back: the next keystroke is a
		// message, not a swallowed one.
		p.v.editor.TypedRune('x')
		if !strings.HasSuffix(p.v.editor.Text, "x") {
			t.Errorf("the input did not take the next keystroke after Copy: %q", p.v.editor.Text)
		}
	})
}

// bodyTextProps reads the two properties every card body must carry: word
// wrapping, and the alignment that keeps a user's own message right-justified.
// Every body is a RichText now, so alignment lives on its text segments.
func bodyTextProps(o fyne.CanvasObject) (fyne.TextWrap, fyne.TextAlign) {
	rt, ok := o.(*widget.RichText)
	if !ok {
		return 0, 0
	}
	for _, seg := range rt.Segments {
		if ts, ok := seg.(*widget.TextSegment); ok {
			return rt.Wrapping, ts.Style.Alignment
		}
	}
	return rt.Wrapping, fyne.TextAlignLeading
}

// TestGUICardBodiesAreSelectable is the assertion the user asked for, and the
// one the first fix could not make: every card in the transcript has to yield its
// text to the mouse, not only the running card. It goes through blockCards - the
// function that decides what each card is built from - rather than through the
// helpers, because the last attempt failed precisely by testing a helper that no
// card called.
func TestGUICardBodiesAreSelectable(t *testing.T) {
	kinds := []struct {
		what string
		blk  block
	}{
		{"tool output", block{kind: "tool", transcript: []string{"$ echo hi", "hi", "exit 0"}}},
		{"assistant prose", block{kind: "assistant", text: "the build passed cleanly"}},
		{"thinking", block{kind: "assistant", text: "out", reasoning: "because the flag was set", stats: metrics{Tokens: 3}}},
		{"user message", block{kind: "user", text: "please rerun the tests"}},
		{"system message", block{kind: "system", text: "context compacted"}},
		{"note", block{kind: "note", text: "cancelling this turn"}},
		{"error", block{kind: "error", text: "panic: something broke"}},
		{"compaction", block{kind: "compaction", title: "compacted", text: "summary of earlier turns"}},
	}
	for _, k := range kinds {
		objs := blockCards(k.blk, true)
		if len(objs) == 0 {
			t.Fatalf("%s rendered nothing", k.what)
		}
		var body fyne.CanvasObject
		switch o := objs[0].(type) {
		case *area:
			body = o.body
		case fyne.CanvasObject:
			body = o
		}
		// Cards whose body is a scroll or an accordion hold the text inside;
		// dig one level so the check is about the text, not the container.
		if sc, ok := body.(*container.Scroll); ok {
			body = sc.Content
		}
		rt, ok := body.(*widget.RichText)
		if !ok {
			t.Errorf("%s: body is %T, want a *widget.RichText", k.what, body)
			continue
		}
		if !rt.Selectable {
			t.Errorf("%s: body is not Selectable, so its text cannot be copied out", k.what)
		}
	}
}
