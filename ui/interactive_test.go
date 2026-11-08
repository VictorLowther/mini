package ui

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// harness drives the real Print front end with canned stdin lines, so these
// tests exercise the slash-command state machine itself rather than a copy of
// it, and record which questions were asked.
type harness struct {
	ui  *printUI
	out *lockedBuffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// printUI is the Print UI plus the question log the assertions read.
type printUI struct {
	*Print
	kinds []string
}

func (u *printUI) Ask(ctx context.Context, q Question) (Answer, error) {
	u.kinds = append(u.kinds, q.Kind)
	return u.Print.Ask(ctx, q)
}

func newConfig(mode string, whitelist ...string) *agent.Config {
	return &agent.Config{
		Mode:             mode,
		WhitelistActions: whitelist,
		CostLimit:        10,
		StepLimit:        0,
		ConfirmExit:      true,
		SystemTemplate:   "sys",
		InstanceTemplate: "task={{.task}}",
	}
}

func newHarness(t *testing.T, c *agent.Config, in string, terminal bool) *harness {
	t.Helper()
	out := &lockedBuffer{}
	p := NewPrint(out, strings.NewReader(in), c)
	p.Terminal = terminal
	return &harness{ui: &printUI{Print: p}, out: out}
}

// pair wires a loop, the interactive hooks and the scripted UI together.
func pair(t *testing.T, c *agent.Config, cmds []string, e env.Environment, in string, terminal bool) (*agent.Loop, *harness) {
	t.Helper()
	h := newHarness(t, c, in, terminal)
	l := &agent.Loop{Cfg: c, Model: outputs(t, cmds...), Env: e}
	h.ui.Print.Loop = l
	i, err := New(h.ui, c, l)
	if err != nil {
		t.Fatal(err)
	}
	l.Hooks = i
	return l, h
}

func outputs(t *testing.T, cmds ...string) *model.Deterministic {
	var msgs []model.Message
	for i, cmd := range cmds {
		msgs = append(msgs, model.Message{
			"role":    "assistant",
			"content": "turn " + string(rune('a'+i)),
			"extra":   map[string]any{"actions": []model.Action{{Command: cmd, ToolCallID: "c"}}, "cost": 1.0},
		})
	}
	return &model.Deterministic{CostPerCall: 1, Outputs: msgs, ObsTemplate: "{{ .output.returncode }}|{{ .output.output }}"}
}

const submitCmd = "echo COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n"

func kinds(h *harness) string     { return strings.Join(h.ui.kinds, ",") }
func (h *harness) output() string { return h.out.String() }

// containsExceptionInfo looks at the observation extra, where the padding
// marker lives rather than in the rendered content.
func containsExceptionInfo(msgs []model.Message, want string) bool {
	for _, m := range msgs {
		if e, ok := m["extra"].(map[string]any); ok {
			if s, _ := e["exception_info"].(string); strings.Contains(s, want) {
				return true
			}
		}
	}
	return false
}

func containsContent(msgs []model.Message, want string) bool {
	for _, m := range msgs {
		if c, ok := m["content"].(string); ok && strings.Contains(c, want) {
			return true
		}
	}
	return false
}

func contentsOf(msgs []model.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if i := strings.IndexByte(content, '\n'); i >= 0 {
			content = content[:i]
		}
		b.WriteString(role + "|" + content + "\n")
	}
	return b.String()
}

func TestEnterConfirms(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []string{"echo hi", submitCmd}, &env.Local{}, "\n\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if kinds(h) != "confirm,confirm,new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
	if !strings.Contains(h.output(), "Execute 1 action(s)?") {
		t.Errorf("output = %q", h.output())
	}
	if !containsContent(l.Messages, "hi") {
		t.Errorf("messages = %s", contentsOf(l.Messages))
	}
}

func TestTypedTextRejectsAndReappearsInHistory(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []string{"echo not-run", submitCmd + "echo first\n"}, &env.Local{},
		"no, use printf instead\n\n\n", true)
	info, err := l.Run(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	want := "Commands not executed. The user rejected your commands with the following message: no, use printf instead"
	if !containsContent(l.Messages, want) {
		t.Errorf("rejection missing:\n%s", contentsOf(l.Messages))
	}
	if kinds(h) != "confirm,confirm,new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
	if containsContent(l.Messages, "|not-run") {
		t.Error("the rejected command ran anyway")
	}
	if !containsExceptionInfo(l.Messages, "action was not executed") {
		t.Errorf("padded observation missing:\n%s", contentsOf(l.Messages))
	}
	if info["exit_status"] != "Submitted" || info["submission"] != "first\n" {
		t.Errorf("info = %#v", info)
	}
	// The rejection reaches the model as a user message, not just the log.
	var asUser bool
	for _, m := range l.Messages {
		if m["role"] == "user" && strings.Contains(m["content"].(string), want) {
			asUser = true
		}
	}
	if !asUser {
		t.Error("the rejection text never reached the model")
	}
}

func TestWhitelistedActionsNeverPrompt(t *testing.T) {
	c := newConfig("confirm", `echo .*`, `cat \S+`)
	l, h := pair(t, c, []string{"echo safe", submitCmd}, &env.Local{}, "\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(kinds(h), "confirm") {
		t.Errorf("echo .* was whitelisted yet a confirmation was asked: %s", kinds(h))
	}
}

func TestWhitelistIsPrefixAnchored(t *testing.T) {
	// Python matches with re.match; Go's MatchString is unanchored, so a bare
	// "ls" must not approve "echo leak; ls /tmp".
	c := newConfig("confirm", "ls")
	c.ConfirmExit = false
	l, h := pair(t, c, []string{"echo leak; ls /tmp", submitCmd}, &env.Local{}, "\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kinds(h), "confirm") {
		t.Fatal("an unanchored whitelist pattern approved a command that merely contains the pattern")
	}
}

func TestSlashYConfirmsAndStopsLaterPrompts(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []string{"echo one", "echo two", submitCmd}, &env.Local{}, "/y\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if c.Mode != "yolo" {
		t.Errorf("mode = %s, want yolo", c.Mode)
	}
	if kinds(h) != "confirm,new-task" {
		t.Errorf("questions = %s, want one confirm prompt then the exit prompt", kinds(h))
	}
	if !containsContent(l.Messages, "one") {
		t.Error("/y must also confirm the batch it was typed at")
	}
	if !strings.Contains(h.output(), "Switched to yolo mode") {
		t.Errorf("output = %q", h.output())
	}
}

func TestSlashURejectsBatchAndSwitchesToHuman(t *testing.T) {
	c := newConfig("confirm")
	c.ConfirmExit = false
	l, _ := pair(t, c, []string{"echo not-run", submitCmd}, &env.Local{}, "/u\necho COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !containsContent(l.Messages, "Commands not executed. Switching to human mode") {
		t.Errorf("switch message missing:\n%s", contentsOf(l.Messages))
	}
	if !containsContent(l.Messages, "User command: \n```bash\necho COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n```") {
		t.Errorf("human command message missing:\n%s", contentsOf(l.Messages))
	}
}

func TestHumanModeSkipsTheModel(t *testing.T) {
	c := newConfig("human")
	c.ConfirmExit = false
	l, _ := pair(t, c, []string{"echo never"}, &env.Local{}, "echo COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if l.NCalls != 0 {
		t.Errorf("human mode must not call the model, n_calls = %d", l.NCalls)
	}
	if !containsContent(l.Messages, "User command: \n```bash\necho COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n```") {
		t.Errorf("messages:\n%s", contentsOf(l.Messages))
	}
}

func TestAlreadyInModeRePrompts(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []string{"echo hi", submitCmd}, &env.Local{}, "/c\n\n\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.output(), "Already in confirm mode") {
		t.Errorf("output = %q", h.output())
	}
	if kinds(h) != "confirm,confirm,new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
}

func TestSlashHShowsHelpWithoutAnswering(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []string{"echo hi", submitCmd}, &env.Local{}, "/h\n\n\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	out := h.output()
	for _, want := range []string{"Current mode: confirm", "/y to switch to yolo mode", "/m to enter multiline comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("help text missing %q:\n%s", want, out)
		}
	}
	if kinds(h) != "confirm,confirm,new-task" {
		t.Errorf("/h should not have answered the prompt: %s", kinds(h))
	}
}

func TestSubmittedWithTypedTaskContinuesTheRun(t *testing.T) {
	c := newConfig("yolo")
	l, _ := pair(t, c, []string{submitCmd, submitCmd + "echo second\n"}, &env.Local{}, "and now do it again\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !containsContent(l.Messages, "The user added a new task: and now do it again") {
		t.Errorf("new task message missing:\n%s", contentsOf(l.Messages))
	}
	if !containsContent(l.Messages, "second") {
		t.Error("the run did not continue after the new task")
	}
}

func TestSubmittedWithEnterFinishes(t *testing.T) {
	c := newConfig("yolo")
	l, h := pair(t, c, []string{submitCmd + "echo first\n"}, &env.Local{}, "\n", true)
	info, err := l.Run(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" || info["submission"] != "first\n" {
		t.Errorf("info = %#v", info)
	}
	if kinds(h) != "new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
}

func TestConfirmExitFalseNeverPrompts(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []string{submitCmd}, &env.Local{}, "", true)
	info, _ := l.Run(context.Background(), "T")
	if info["exit_status"] != "Submitted" {
		t.Errorf("info = %#v", info)
	}
	if len(h.ui.kinds) != 0 {
		t.Errorf("unexpected prompts: %s", kinds(h))
	}
}

func TestLimitsExceededDoesNotBlockWithoutATerminal(t *testing.T) {
	c := newConfig("yolo")
	c.CostLimit = 0.5
	l, h := pair(t, c, []string{"echo one", "echo two"}, &env.Local{}, "", false)
	done := make(chan struct{})
	var info map[string]any
	go func() { info, _ = l.Run(context.Background(), "T"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the limits prompt blocked a non-interactive run")
	}
	if info["exit_status"] != "LimitsExceeded" {
		t.Errorf("info = %#v", info)
	}
	if strings.Contains(kinds(h), "limits") {
		t.Error("must not prompt for limits without a terminal")
	}
}

func TestLimitsPromptLiftsTheCeiling(t *testing.T) {
	c := newConfig("yolo")
	c.CostLimit = 0.5
	l, h := pair(t, c, []string{"echo one", submitCmd}, &env.Local{}, "10\n10\n\n", true)
	info, err := l.Run(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" {
		t.Errorf("info = %#v", info)
	}
	if c.CostLimit != 10 || c.StepLimit != 10 {
		t.Errorf("limits not lifted: %+v", c)
	}
	if !strings.Contains(h.output(), "Limits exceeded") {
		t.Errorf("banner missing:\n%s", h.output())
	}
}

func TestInterruptIsRecordedWhenCancelled(t *testing.T) {
	c := newConfig("yolo")
	l, _ := pair(t, c, []string{"sleep 20"}, &env.Local{Timeout: 30 * time.Second}, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err := l.Run(ctx, "T"); err == nil {
		t.Fatal("expected the cancelled run to end with an error")
	}
	if !containsContent(l.Messages, "Temporary interruption caught") {
		t.Errorf("interruption message missing:\n%s", contentsOf(l.Messages))
	}
}

func TestMultilineCommentViaSlashM(t *testing.T) {
	c := newConfig("confirm")
	c.ConfirmExit = false
	l, _ := pair(t, c, []string{"echo hi", submitCmd}, &env.Local{}, "/m\nfirst line\nsecond line\n\n\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !containsContent(l.Messages, "first line\nsecond line") {
		t.Errorf("multiline rejection text missing:\n%s", contentsOf(l.Messages))
	}
}

func TestPrintHeadersMatchTheReferenceFormat(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []string{"echo hi", submitCmd}, &env.Local{}, "\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	out := h.output()
	// The header is printed from add_messages, after the call is counted
	// and billed, exactly like the Python agent.
	for _, want := range []string{"mini-swe-agent (step 1, $1.00):", "mini-swe-agent (step 2, $2.00):", "\nTool:", "\nExit:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStreamedContentIsPrintedOnce(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []string{"echo hi"}, &env.Local{}, "", true)
	h.ui.Print.Loop = l
	// Simulate a streaming model: the UI sees chunks, then the finished message.
	l.Handler = &model.Handler{
		Content:   func(s string) { h.ui.Content(s) },
		Reasoning: func(s string) { h.ui.Reasoning(s) },
	}
	l.Messages = nil
	l.Handler.Content("turn a")
	l.Handler.Reasoning("thinking a")
	h.ui.Message(model.Message{"role": "assistant", "content": "turn a"})
	out := h.output()
	if got := strings.Count(out, "turn a"); got != 1 {
		t.Errorf("content appeared %d times:\n%s", got, out)
	}
	if !strings.Contains(out, "thinking a") {
		t.Errorf("reasoning missing:\n%s", out)
	}
}

func TestUnanswerablePromptEndsTheRunInsteadOfLooping(t *testing.T) {
	// A piped run with no -y reaches a confirmation nobody can answer. It must
	// stop with an error, not reject the same batch forever.
	c := newConfig("confirm")
	l, _ := pair(t, c, []string{"echo hi", submitCmd}, &env.Local{}, "", true)
	_, err := l.Run(context.Background(), "T")
	if err == nil || !strings.Contains(err.Error(), "no input available") {
		t.Fatalf("err = %v", err)
	}
	if l.NCalls > 1 {
		t.Errorf("n_calls = %d, want the run to stop at the first prompt", l.NCalls)
	}
}
