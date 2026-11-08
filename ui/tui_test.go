package ui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// pane is a test handle on the TUI that serialises access the way bubbletea's
// single update goroutine does in production.
type pane struct {
	*TUI
	m   *tuiModel
	log *printLog
}

// printLog records what the pane sent above itself, i.e. the lines that should
// be in the terminal's own scrollback.
type printLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *printLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}

func (l *printLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (p pane) press(msg tea.Msg) { p.kick(p.m, msg) }
func (p pane) enter()            { p.press(tea.KeyPressMsg{Code: tea.KeyEnter}) }
func (p pane) typeText(s string) { p.doOn(func() { p.m.editor.Reset(); p.m.editor.SetValue(s) }) }
func (p pane) value() string     { var s string; p.doOn(func() { s = p.m.editor.Value() }); return s }
func (p pane) view() string      { var s string; p.doOn(func() { s = p.m.View().Content }); return s }
func (p pane) dialog() string    { var s string; p.doOn(func() { s = p.m.dialog }); return s }

// scrollback returns the whole transcript - printed lines and pane lines are the
// same slice of m.lines - with lipgloss padding removed, so assertions read
// like the transcript instead of like a styled screen.
// flat returns the scrollback with whitespace collapsed, and dense with it
// removed: both let assertions ignore the pane's wrapping, which breaks long
// tokens (paths) mid-string on purpose rather than soft-wrapping at the
// terminal edge.
func (p pane) flat() string  { return strings.Join(strings.Fields(p.scrollback()), " ") }
func (p pane) dense() string { return strings.Join(strings.Fields(p.scrollback()), "") }

func (p pane) scrollback() string {
	var s string
	p.doOn(func() {
		stripped := ansi.Strip(strings.Join(p.m.lines, "\n"))
		lines := make([]string, 0, 32)
		for _, l := range strings.Split(stripped, "\n") {
			lines = append(lines, strings.TrimRight(l, " \t"))
		}
		s = strings.Join(lines, "\n")
	})
	return s
}
func (p pane) panel() string {
	var s string
	p.doOn(func() { s = ansi.Strip(p.m.pendingPanel()) })
	return s
}
func (p pane) thinking() bool     { var b bool; p.doOn(func() { b = p.m.t.showReasoning() }); return b }
func (p pane) helpExpanded() bool { var b bool; p.doOn(func() { b = p.m.showFullHelp }); return b }

func newTUIForTest(t *testing.T, c *agent.Config, l *agent.Loop) pane {
	t.Helper()
	tui, err := NewTUI(TUIOptions{
		Cfg: c, Loop: l,
		Model:      &model.Sushi{Model: "test-model", BaseURL: "http://127.0.0.1:1"},
		ConfigName: "mini",
		Width:      90, Height: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(tui, c, l)
	if err != nil {
		t.Fatal(err)
	}
	l.Hooks = h
	m := newTUIModel(tui, "", func() {})
	tui.fallback = func(msg tea.Msg) { tui.kick(m, msg) }
	m.width, m.height = 90, 30
	m.ready = true
	log := &printLog{}
	// Delivery is synchronous here: there is no event loop to hand off to.
	tui.printer = newSyncPrinter(log.add, func() {})
	return pane{TUI: tui, m: m, log: log}
}

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

// ask runs a question from the test goroutine so the test can then answer it
// with synthetic key presses, exactly as the program would.
func ask(t *testing.T, tui *TUI, q Question) <-chan Answer {
	t.Helper()
	got := make(chan Answer, 1)
	go func() {
		ans, err := tui.Ask(context.Background(), q)
		if err != nil {
			ans = Answer{}
		}
		got <- ans
	}()
	waitFor(t, "the question to reach the pane", tui.HasQuestion)
	return got
}

func press(r rune) tea.Msg { return tea.KeyPressMsg{Code: r} }

func TestTUIConfirmPanelDrivesTheLoop(t *testing.T) {
	dir := t.TempDir()
	c := newConfig("confirm")
	c.OutputPath = filepath.Join(dir, "tui.traj.json")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", submitCmd+"echo done\n"), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newTUIForTest(t, c, l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = l.Run(ctx, "make a file then submit"); close(done) }()

	waitFor(t, "the first confirmation", p.HasQuestion)
	q := p.PendingQuestion()
	if q.Kind != AskConfirm || len(q.Actions) != 1 || q.Actions[0].Command != "echo hi" {
		t.Fatalf("question = %+v", q)
	}
	if panel := p.panel(); !strings.Contains(panel, "echo hi") {
		t.Errorf("panel = %q", panel)
	}
	p.enter() // confirm

	waitFor(t, "the second confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskConfirm
	})
	p.enter() // confirm the submitting batch

	waitFor(t, "the exit confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskNewTask
	})
	if view := p.view(); !strings.Contains(view, "Agent wants to finish") {
		t.Errorf("view = %q", view)
	}
	p.enter() // Enter quits

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
	// The pane shows only the unprinted tail; m.lines holds the whole transcript.
	scrollback := p.scrollback()
	for _, want := range []string{"mini-swe-agent (step 1", "exit 0", "hi"} {
		if !strings.Contains(scrollback, want) {
			t.Errorf("scrollback missing %q:\n%s", want, scrollback)
		}
	}
}

func TestTUITypeToReject(t *testing.T) {
	c := newConfig("confirm")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo not-run", submitCmd), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newTUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()

	waitFor(t, "the confirmation", p.HasQuestion)
	p.typeText("use printf instead")
	p.enter()
	waitFor(t, "the second confirmation", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskConfirm
	})
	p.enter()
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

func TestTUIEscCancelsTheTurn(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "sleep 30"), Env: &env.Local{Timeout: 30 * time.Second}}
	p := newTUIForTest(t, c, l)
	done := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()

	waitFor(t, "the command to start", func() bool { return l.Snapshot().Calls == 1 })
	p.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	// The reference asks for a comment on the interruption before continuing.
	waitFor(t, "the interruption prompt", func() bool {
		return p.HasQuestion() && p.PendingQuestion().Kind == AskInterrup
	})
	p.typeText("that took too long")
	p.enter()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("esc did not interrupt the running command\nmessages:\n%s\nquestion pending: %v (%s)",
			contentsOf(l.Messages), p.HasQuestion(), p.PendingQuestion().Kind)
	}
	if !containsContent(l.Messages, "Interrupted by user: that took too long") {
		t.Errorf("comment missing:\n%s", contentsOf(l.Messages))
	}
}

func TestTUIQuitDialog(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newTUIForTest(t, c, l)
	// Nothing running: esc opens a confirm dialog rather than quitting, and a
	// bare q must never do it (it is the first letter of plenty of prose).
	p.press(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if p.dialog() != "" {
		t.Fatalf("q opened the quit dialog: %q", p.dialog())
	}
	if p.value() != "q" {
		t.Errorf("q never reached the editor: %q", p.value())
	}
	p.typeText("")
	p.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.dialog() != "quit" {
		t.Fatalf("dialog = %q, want the quit confirmation", p.dialog())
	}
	if !strings.Contains(p.view(), "Quit?") {
		t.Errorf("view = %q", p.view())
	}
	p.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.dialog() != "" {
		t.Errorf("esc should close the dialog, got %q", p.dialog())
	}
}

func TestTUISlashCommandsWorkInPane(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newTUIForTest(t, c, l)

	got := ask(t, p.TUI, Question{Kind: AskConfirm, Title: confirmationTitle(1), Cfg: c})
	p.typeText("/y")
	p.enter()
	if c.Mode != "yolo" {
		t.Errorf("mode = %s, want yolo", c.Mode)
	}
	if ans := <-got; ans.Reject {
		t.Errorf("/y must confirm the batch, got %+v", ans)
	}

	got = ask(t, p.TUI, Question{Kind: AskConfirm, Title: confirmationTitle(1), Cfg: c})
	p.typeText("/h")
	p.enter()
	if !p.HasQuestion() {
		t.Error("/h answered the prompt")
	}
	found := false
	for _, line := range strings.Split(p.scrollback(), "\n") {
		if strings.Contains(line, "switch to yolo mode") {
			found = true
		}
	}
	if !found {
		t.Errorf("help text never reached the scrollback: %s", p.scrollback())
	}
	p.typeText("")
	p.enter()
	<-got
}

func TestTUILimitsDialogAsksTwice(t *testing.T) {
	c := newConfig("yolo")
	c.StepLimit, c.CostLimit = 3, 1.5
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newTUIForTest(t, c, l)
	got := ask(t, p.TUI, Question{Kind: AskLimits, Title: limitsBanner(c, 0, 0), Cfg: c})
	waitFor(t, "the limits dialog", func() bool { return p.dialog() == "limits" })
	if !strings.Contains(p.PendingQuestion().Title, "Limits exceeded") {
		t.Errorf("title = %q", p.PendingQuestion().Title)
	}
	p.typeText("12")
	p.enter()
	if p.dialog() != "limits" {
		t.Fatalf("dialog = %q, want the second limits input", p.dialog())
	}
	p.typeText("7.5")
	p.enter()
	ans := <-got
	if ans.Steps != 12 || ans.Cost != 7.5 || !ans.NewLimits {
		t.Errorf("answer = %+v", ans)
	}
}

func TestTUITaskQuestionUsesTheEditor(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newTUIForTest(t, c, l)
	got := ask(t, p.TUI, Question{Kind: AskTask, Title: TaskPrompt, Cfg: c})
	// An empty answer is refused.
	p.enter()
	if !p.HasQuestion() {
		t.Error("an empty task was accepted")
	}
	p.typeText("fix the tests")
	p.enter()
	if ans := <-got; ans.Text != "fix the tests" {
		t.Errorf("answer = %+v", ans)
	}
}

// TestTUITrajectoryMatchesPrintMode is the plan's proof that the two front ends
// are interchangeable: the same scripted model through the TUI and through
// --print must produce the same trajectory.
func TestTUITrajectoryMatchesPrintMode(t *testing.T) {
	commands := []string{"echo hi", submitCmd + "echo same\n"}

	tuiPath := filepath.Join(t.TempDir(), "tui.json")
	c1 := newConfig("confirm")
	c1.OutputPath = tuiPath
	l1 := &agent.Loop{Cfg: c1, Model: outputs(t, commands...), Env: &env.Local{Timeout: 10 * time.Second}}
	p := newTUIForTest(t, c1, l1)
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
	p.enter()
	<-done

	printPath := filepath.Join(t.TempDir(), "print.json")
	c2 := newConfig("confirm")
	c2.OutputPath = printPath
	l2, _ := pair(t, c2, commands, &env.Local{Timeout: 10 * time.Second}, "\n\n\n", true)
	if _, err := l2.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}

	a := normalizeTraj(t, tuiPath)
	b := normalizeTraj(t, printPath)
	if a != b {
		t.Errorf("trajectories differ\n tui: %s\nprint: %s", a, b)
	}
}

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
			// The two front ends differ only in where they saved the file.
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
		for _, k := range []string{"timestamp", "outputs", "response", "cwd", "base_url", "model", "output_path"} {
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

func TestTUIHelpAndThinkingKeys(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	p := newTUIForTest(t, c, l)
	// "?" with an empty editor expands the key help.
	p.press(tea.KeyPressMsg{Code: tea.KeyEscape}) // idle: esc opens the quit confirm
	p.press(tea.KeyPressMsg{Code: tea.KeyEscape}) // and closes it again
	p.press(tea.KeyPressMsg{Code: '?', Text: "?"})
	if !p.helpExpanded() {
		t.Error("? did not expand the help")
	}
	// "newline" only appears in the expanded help; scrolling is the terminal's
	// job now, so the pane no longer binds those keys.
	if !strings.Contains(p.view(), "newline") {
		t.Errorf("full help missing from the view:\n%s", p.view())
	}
	// ctrl+t toggles thinking without touching the editor.
	before := p.thinking()
	p.press(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if p.thinking() == before {
		t.Error("ctrl+t did not toggle thinking")
	}
	// ... and typing prose is never swallowed by a binding.
	p.typeText("")
	p.press(tea.KeyPressMsg{Code: 't', Text: "t"})
	p.press(tea.KeyPressMsg{Code: 'h', Text: "h"})
	p.press(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if p.value() != "the" {
		t.Errorf("typing was intercepted by a key binding: %q", p.value())
	}
}

// tuiThinking reports the pane's thinking-collapse state through the model.

// TestTUICommandAtPromptDoesNotDeadlock is the regression test for the pane
// freezing on /y: the slash feedback used to go through program.Send from
// inside Update, which blocks the event loop forever.
func TestTUICommandAtPromptDoesNotDeadlock(t *testing.T) {
	for _, cmd := range []string{"/y", "/c", "/u", "/h", "/t", "/m"} {
		t.Run(cmd, func(t *testing.T) {
			c := newConfig("confirm")
			c.ConfirmExit = false
			l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi", submitCmd), Env: &env.Local{Timeout: 10 * time.Second}}
			p := newTUIForTest(t, c, l)
			done := make(chan struct{})
			go func() { _, _ = l.Run(context.Background(), "T"); close(done) }()
			waitFor(t, "the confirmation", p.HasQuestion)
			p.typeText(cmd)
			p.enter()
			// Finish whatever the command started: /u hands control to human
			// mode, so the run only ends when a submit command is typed there.
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
				t.Fatalf("%q left the pane deadlocked", cmd)
			}
		})
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestTUISteeringLandsInTheHistory: typing while the agent is working must
// reach the model instead of being dropped.
func TestTUISteeringLandsInTheHistory(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second}}
	// /sleep is consumed inside the model call, so the steering typed during it
	// must land before the *next* query.
	l.Model = &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}|{{ .output.output }}", Outputs: []model.Message{
		turnWithActions("/sleep 0.4"),
		turnWithActions("echo first-turn"),
		turnWithActions(submitCmd + "echo done\n"),
	}}
	p := newTUIForTest(t, c, l)
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
	_ = p
}

func turnWithActions(cmd string) model.Message {
	return model.Message{
		"role":    "assistant",
		"content": "turn",
		"extra":   map[string]any{"actions": []model.Action{{Command: cmd, ToolCallID: "c"}}, "cost": 1.0},
	}
}

func has(t *TUI) bool { return t.HasQuestion() }

func TestTUITranscriptIsAShellSessionNotJSON(t *testing.T) {
	dir := t.TempDir()
	c := newConfig("yolo")
	c.ConfirmExit = false
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo one; echo two; exit 3", submitCmd),
		Env: &env.Local{CWD: dir, Timeout: 10 * time.Second}}
	p := newTUIForTest(t, c, l)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	scroll := p.scrollback()
	for _, want := range []string{"$ echo one; echo two; exit 3", "exit 3"} {
		if !strings.Contains(scroll, want) {
			t.Errorf("transcript missing %q:\n%s", want, scroll)
		}
	}
	if !strings.Contains(scroll, "\none\n") || !strings.Contains(scroll, "\ntwo\n") {
		t.Errorf("output lines missing:\n%s", scroll)
	}
	// The directory is shown, though a temp path longer than the pane is
	// wrapped, so compare ignoring line breaks.
	if !strings.Contains(p.dense(), "in"+dir) {
		t.Errorf("transcript missing the directory %q:\n%s", dir, scroll)
	}
	for _, notWant := range []string{`"returncode"`, `"output"`, `"raw_output"`} {
		if strings.Contains(scroll, notWant) {
			t.Errorf("transcript leaks the model's JSON view (%s):\n%s", notWant, scroll)
		}
	}
}

func TestTUIThinkingStateIsLegible(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	msg := model.Message{
		"role":              "assistant",
		"content":           "the plan",
		"reasoning_content": strings.Repeat("pondering ", 30),
		"extra":             map[string]any{"actions": []model.Action{{Command: submitCmd, ToolCallID: "c"}}, "cost": 1.0},
	}
	l := &agent.Loop{Cfg: c, Env: &env.Local{Timeout: 10 * time.Second},
		Model: &model.Deterministic{CostPerCall: 1, ObsTemplate: "{{ .output.returncode }}", Outputs: []model.Message{msg}}}
	p := newTUIForTest(t, c, l)

	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	// Default: off, and the pane says so twice - in the status line and on the
	// turn itself, with the size of what is hidden.
	if !strings.Contains(p.view(), "thinking: off") {
		t.Errorf("status line does not report the thinking state:\n%s", p.view())
	}
	if !strings.Contains(p.scrollback(), "[thinking hidden: 300 chars · ctrl+t]") {
		t.Errorf("hidden thinking is not announced:\n%s", p.scrollback())
	}
	if strings.Contains(p.flat(), "pondering") {
		t.Error("thinking text leaked while hidden")
	}

	// Toggle on: the same turn now shows it, without a new message arriving.
	p.press(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !strings.Contains(p.view(), "thinking: on") {
		t.Errorf("status line still says thinking is off:\n%s", p.view())
	}
	flat := p.flat()
	if !strings.Contains(flat, "thinking: pondering") {
		t.Errorf("toggling did not re-render the turn:\n%s", p.scrollback())
	}
	if strings.Contains(flat, "hidden") {
		t.Errorf("the hidden marker survived the toggle:\n%s", p.scrollback())
	}
}

// TestStreamedTurnRendersLikeTheCommittedTurn is the regression test for the
// pane showing one shape while a turn streams and another when it lands: there
// is now a single rendering path, so the in-flight block already carries the
// header and thinking state, and committing replaces it instead of appending a
// second copy.
// A turn streaming more than one screen of thinking must not push the prompt
// off the bottom: the pane keeps the newest lines and says how many it hid.
func TestStreamingTurnStaysInsideTheScreen(t *testing.T) {
	c := newConfig("yolo")
	l := &agent.Loop{Cfg: c, Model: &model.Deterministic{}, Env: &env.Local{}}
	p := newTUIForTest(t, c, l)
	p.m.height = 24
	p.press(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}) // thinking on: it has to be rendered
	l.Handler.Reasoning(strings.Repeat("reasoning about the repository layout. ", 40))
	l.Handler.Content("now the answer")
	raw := ansi.Strip(p.view())
	flat := strings.Join(strings.Fields(raw), " ")
	if !strings.Contains(flat, "earlier lines of this turn above") {
		t.Errorf("hidden streaming lines were not acknowledged:\n%s", raw)
	}
	if !strings.Contains(flat, "now the answer") {
		t.Errorf("the pane lost the newest streamed text:\n%s", raw)
	}
	if n := len(strings.Split(strings.TrimRight(raw, "\n"), "\n")); n > 24 {
		t.Errorf("the pane alone is %d lines tall, larger than the 24 line screen:\n%s", n, raw)
	}
}

func TestStreamedTurnRendersLikeTheCommittedTurn(t *testing.T) {
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
	p := newTUIForTest(t, c, l)

	l.Cfg.CostLimit = 10
	l.Handler.Reasoning("the user wants a ")
	l.Handler.Reasoning("listing of the current directory")
	l.Handler.Content("I will list the files.")

	mid := p.scrollback()
	if got := ansi.Strip(p.log.all()); got != "" {
		t.Errorf("an in-flight turn must not be printed yet, got:\n%s", got)
	}
	if got := strings.Count(mid, "mini-swe-agent (step "); got != 1 {
		t.Errorf("the in-flight turn should carry the header it will commit with, got %d:\n%s", got, mid)
	}
	if !strings.Contains(mid, "[thinking hidden:") {
		t.Errorf("in-flight thinking state not announced:\n%s", mid)
	}
	if !strings.Contains(mid, "I will list the files.") {
		t.Errorf("streamed content missing:\n%s", mid)
	}

	// Commit the same turn: it must replace the live block, not duplicate it.
	p.Message(msg)
	after := p.scrollback()
	printed := ansi.Strip(p.log.all())
	if !strings.Contains(printed, "I will list the files.") {
		t.Errorf("the committed turn should have been printed into the scrollback, got:\n%s", printed)
	}
	if view := strings.Join(strings.Fields(ansi.Strip(p.view())), " "); strings.Contains(view, "list the files") {
		t.Errorf("printed lines must leave the pane, which cannot show them twice:\n%s", view)
	}
	for _, want := range []string{"I will list the files.", "mini-swe-agent (step"} {
		if got := strings.Count(after, want); got != 1 {
			t.Errorf("%q appears %d times after committing, want 1:\n%s", want, got, after)
		}
	}

	// A second turn appends below the first rather than reusing its slot.
	finished := make(chan struct{})
	go func() { _, _ = l.Run(context.Background(), "T"); close(finished) }()
	<-finished
	all := p.scrollback()
	if !strings.Contains(all, "✓ Submitted") {
		t.Errorf("run did not finish into the pane:\n%s", all)
	}
}
