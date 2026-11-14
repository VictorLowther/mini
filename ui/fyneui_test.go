package ui

import (
	"context"
	"encoding/json"
	"errors"
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
}

func (p *guiPane) do(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn()
}

func newGUIForTest(t *testing.T, c *agent.Config, l *agent.Loop) *guiPane {
	t.Helper()
	g := NewGUI(GUIOptions{
		Cfg: c, Loop: l,
		Model:      &model.Sushi{Model: "test-model", BaseURL: "http://127.0.0.1:1"},
		ConfigName: "mini",
		App:        test.NewApp(),
	})
	if l != nil {
		h, err := New(g, c, l)
		if err != nil {
			t.Fatal(err)
		}
		l.Hooks = h
	}
	p := &guiPane{GUI: g, v: g.v, mu: new(sync.Mutex)}
	g.post = func(fn func()) { p.do(fn) }
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
			if ts, ok := s.(*widget.TextSegment); ok {
				b.WriteString(ts.Text)
			}
		}
		return b.String()
	case *widget.Label:
		return t.Text
	case *widget.TextGrid:
		return t.Text()
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
		parts := make([]string, 0, len(p.v.body.Objects)+len(p.v.dock.Objects))
		for _, o := range p.v.body.Objects {
			parts = append(parts, transcriptText(o))
		}
		// The live streaming areas render below the committed transcript.
		for _, o := range p.v.dock.Objects {
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
func (p *guiPane) dialog() string { var s string; p.do(func() { s = p.v.dialog }); return s }
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
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", submitCmd+"echo done\n"), Env: &env.Local{Timeout: 10 * time.Second}}
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
	if panel := p.panel(); !strings.Contains(panel, "Agent wants to finish") {
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
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo not-run", submitCmd), Env: &env.Local{Timeout: 10 * time.Second}}
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
	if !containsContent(l.Messages, "The user rejected your commands with the following message: use printf instead") {
		t.Errorf("rejection missing:\n%s", contentsOf(l.Messages))
	}
	if containsContent(l.Messages, "|not-run") {
		t.Error("the rejected command ran")
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
		t.Fatalf("esc did not interrupt the running command\nmessages:\n%s", contentsOf(l.Messages))
	}
	if !containsContent(l.Messages, "Interrupted by user: that took too long") {
		t.Errorf("comment missing:\n%s", contentsOf(l.Messages))
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
	if p.dialog() != "" {
		t.Errorf("esc opened dialog %q; leaving is /quit or %s only", p.dialog(), quitKeyName())
	}
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

	got := askG(t, p, Question{Kind: AskConfirm, Title: confirmationTitle(1), Cfg: c})
	p.typeText("/y")
	p.enter()
	if c.Mode != "yolo" {
		t.Errorf("mode = %s, want yolo", c.Mode)
	}
	if ans := <-got; ans.Reject {
		t.Errorf("/y must confirm the batch, got %+v", ans)
	}

	got = askG(t, p, Question{Kind: AskConfirm, Title: confirmationTitle(1), Cfg: c})
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

func TestGUILimitsDialogAsksTwice(t *testing.T) {
	c := newConfig("yolo")
	c.StepLimit, c.CostLimit = 3, 1.5
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newGUIForTest(t, c, l)
	got := askG(t, p, Question{Kind: AskLimits, Title: limitsBanner(c, 0, 0), Cfg: c})
	if p.dialog() != "limits" {
		t.Fatalf("dialog = %q, want limits", p.dialog())
	}
	panel := p.panel()
	if !strings.Contains(panel, "Limits exceeded") || !strings.Contains(panel, "New step limit: ") {
		t.Errorf("panel = %q", panel)
	}
	p.typeText("12")
	p.enter()
	if p.dialog() != "limits" {
		t.Fatalf("dialog = %q, want the second limits input", p.dialog())
	}
	if panel := p.panel(); !strings.Contains(panel, "New cost limit: ") {
		t.Errorf("second limits label missing: %q", panel)
	}
	p.typeText("7.5")
	p.enter()
	ans := <-got
	if ans.Steps != 12 || ans.Cost != 7.5 || !ans.NewLimits {
		t.Errorf("answer = %+v", ans)
	}
}

// TestGUILimitsEscKeepsTheQuestion: esc hides the prompt, but there is no
// give-up answer; the next submit reopens it.
func TestGUILimitsEscKeepsTheQuestion(t *testing.T) {
	c := newConfig("yolo")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newGUIForTest(t, c, l)
	got := askG(t, p, Question{Kind: AskLimits, Title: limitsBanner(c, 0, 0), Cfg: c})
	p.esc()
	if p.dialog() != "" {
		t.Errorf("esc should hide the limits prompt, dialog = %q", p.dialog())
	}
	if !p.HasQuestion() {
		t.Fatal("esc must not answer the limits question")
	}
	p.typeText("anything")
	p.enter() // swallowed: reopens the prompt with cleared input
	if p.dialog() != "limits" || p.value() != "" {
		t.Errorf("dialog = %q editor = %q, want limits reopened empty", p.dialog(), p.value())
	}
	p.typeText("9")
	p.enter()
	p.typeText("2.5")
	p.enter()
	ans := <-got
	if ans.Steps != 9 || ans.Cost != 2.5 || !ans.NewLimits {
		t.Errorf("answer = %+v", ans)
	}
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
			l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", submitCmd), Env: &env.Local{Timeout: 10 * time.Second}}
			p := newGUIForTest(t, c, l)
			done := make(chan struct{})
			go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()
			waitFor(t, "the confirmation", p.HasQuestion)
			p.typeText(cmd)
			p.enter()
			for i := 0; i < 6 && !isClosed(done); i++ {
				if p.HasQuestion() {
					if p.PendingQuestion().Kind == AskHuman {
						p.typeText(submitCmd)
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
		turnWithActions(submitCmd + "echo done\n"),
	}}
	p := newGUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()
	waitFor(t, "the run to start", func() bool { return l.Running() })
	p.typeText("check the README first")
	p.enter()
	<-done
	if !containsContent(l.Messages, "check the README first") {
		t.Errorf("steering never reached the history:\n%s", contentsOf(l.Messages))
	}
	var idxUser, idxNext int
	for i, msg := range l.Messages {
		if msg["role"] == "user" && strings.Contains(msg["content"].(string), "README") {
			idxUser = i
		}
		if i > idxUser && idxUser > 0 && msg["role"] == "assistant" {
			idxNext = i
			break
		}
	}
	if idxNext == 0 || idxNext < idxUser {
		t.Errorf("steering must be delivered before the next model call:\n%s", contentsOf(l.Messages))
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

// TestGUIStreamedTurnRendersLikeTheCommittedTurn: the live preview carries the
// header from its first chunk and the commit replaces it exactly once.
func TestGUIStreamedTurnRendersLikeTheCommittedTurn(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	msg := model.Message{
		"role":              "assistant",
		"content":           "I will list the files.",
		"reasoning_content": "the user wants a listing of the current directory",
		"extra":             map[string]any{"actions": []model.Action{{Command: submitCmd, ToolCallID: "c"}}, "cost": 1.0},
	}
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second},
		Model: &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}", Outputs: []model.Message{msg}}}
	p := newGUIForTest(t, c, l)
	p.do(func() { p.prepareTranscript() })

	l.Cfg.CostLimit = 10
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

// TestGUITallContentNeverGrowsTheWindow is the regression for the serious
// one: a turn taller than the window - streaming or committed - must not
// resize it. The live card grows to the space the window has left and its
// content scrolls inside the card, and the content tree's MinSize (what the
// glfw driver's fitContent would grow the window toward) stays far below the
// window's height.
func TestGUITallContentNeverGrowsTheWindow(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	tall := strings.Repeat("a streamed line of prose that keeps coming\n", 200)
	msg := model.Message{
		"role":    "assistant",
		"content": tall,
		"extra":   map[string]any{"actions": []model.Action{{Command: submitCmd, ToolCallID: "c"}}, "cost": 1.0},
	}
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second},
		Model: &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}", Outputs: []model.Message{msg}}}
	p := newGUIForTest(t, c, l)
	open := p.v.rootC.Size()
	if open.Height < 600 {
		t.Fatalf("the content must be laid out at the window's size, got %v", open)
	}
	height := func() float32 { return p.v.rootC.Size().Height }

	// A turn far taller than the whole window streams into the live card.
	l.Handler.Content(tall)
	p.do(func() {
		if n, want := p.v.dockScroll.Size().Height, p.v.liveContentHeight(); n >= want {
			t.Errorf("the live card got %v px for content of %v px: it was not capped to the window", n, want)
		}
		if p.v.dockScroll.Offset.Y <= 0 {
			t.Errorf("the capped live card does not follow the stream (offset %v)", p.v.dockScroll.Offset.Y)
		}
		if min := p.v.rootC.MinSize().Height; min > height() {
			t.Errorf("the content tree's minimum is %v px, over the window's %v: fitContent would force it open", min, height())
		}
	})

	// Committing the same tall turn as a transcript card changes nothing:
	// the transcript scroll already absorbs unbounded content.
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

// TestGUIThinkingStateIsLegible: hidden by default with its size named,
// expanded by the toggle, always labelled.
func TestGUIThinkingStateIsLegible(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	reasoning := strings.Repeat("pondering ", 30)
	msg := model.Message{
		"role":              "assistant",
		"content":           "the plan",
		"reasoning_content": reasoning,
		"extra":             map[string]any{"actions": []model.Action{{Command: submitCmd, ToolCallID: "c"}}, "cost": 1.0},
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
	commands := []string{"echo hi", submitCmd + "echo same\n"}

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
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo one; echo two; exit 3", submitCmd),
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
		"extra": map[string]any{"actions": []model.Action{{Command: submitCmd, ToolCallID: "c"}}, "cost": 1.0}}
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
	l := &agent.Loop{Cfg: c, Model: outputs(t, submitCmd), Env: &env.Local{Timeout: 10 * time.Second}}
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
// message kind gets its own opaque speech-bubble area under a header naming
// the kind and the message's tokens / generation time / cost (zeros omitted),
// in the body's own font with a kind colour; text the human typed trails on
// the right, harness-authored feedback and everything else stays left.
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
	if _, _, _, a := fillRGBA(uc); a != 0xffff {
		t.Errorf("user bubble fill alpha = %#x, want opaque (translucent fills rendered invisible in the real driver)", a)
	}
	rt := uc.body.(*widget.RichText)
	if rt.Wrapping != fyne.TextWrapWord {
		t.Error("user body must wrap words")
	}
	if !uc.bubble.mine {
		t.Error("user bubble must point its tail to our side")
	}
	for _, s := range rt.Segments {
		if ts, ok := s.(*widget.TextSegment); ok && ts.Style.Alignment != fyne.TextAlignTrailing {
			t.Error("user text is not right-justified")
		}
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
	hr, hg, hbl, _ := fillRGBA(hc)
	ur, ug, ub, _ := fillRGBA(uc)
	if hr == ur && hg == ug && hbl == ub {
		t.Error("harness and user bubbles need distinguishable fills")
	}
	for _, s := range hc.body.(*widget.RichText).Segments {
		if ts, ok := s.(*widget.TextSegment); ok && ts.Style.Alignment != fyne.TextAlignLeading {
			t.Error("harness feedback must be left-justified")
		}
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
	for _, s := range asst[1].(*area).body.(*widget.RichText).Segments {
		if ts, ok := s.(*widget.TextSegment); ok && ts.Style.Alignment != fyne.TextAlignLeading {
			t.Error("LLM output must stay left-justified")
		}
	}

	tool := blockCards(block{kind: "tool", transcript: []string{"$ echo hi", "exit 0"}}, false)
	if got := headerText(tool[0].(*area)); got != "tool call" {
		t.Errorf("tool header = %q", got)
	}

	// The header is the body's plain font: no bold, no size override.
	if seg := uc.header.Segments[0].(*widget.TextSegment); seg.Style.TextStyle.Bold || seg.Style.SizeName != "" {
		t.Error("area header must use the body's font, unstyled")
	}
}

func headerText(a *area) string {
	return a.header.Segments[0].(*widget.TextSegment).Text
}

func headerColor(a *area) fyne.ThemeColorName {
	return a.header.Segments[0].(*widget.TextSegment).Style.ColorName
}

// fillRGBA returns a bubble's paint colour; opaque is the property that made
// the first cut invisible, so tests read it directly.
func fillRGBA(a *area) (r, g, b, alpha uint32) {
	return a.bubble.shape.FillColor.RGBA()
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
	g := NewGUI(GUIOptions{App: test.NewApp()})
	p := &guiPane{GUI: g, v: g.v, mu: new(sync.Mutex)}
	g.post = func(fn func()) { p.do(fn) }
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
