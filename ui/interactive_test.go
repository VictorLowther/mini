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
		ConfirmExit:      true,
		SystemTemplate:   "sys",
		InstanceTemplate: "task={{.task}}",
	}
}

func newHarness(t *testing.T, c *agent.Config, in string, terminal bool) *harness {
	t.Helper()
	out := &lockedBuffer{}
	p := NewPrint(out, strings.NewReader(in), c, nil)
	p.Terminal = terminal
	return &harness{ui: &printUI{Print: p}, out: out}
}

// pair wires a loop, the interactive hooks and the scripted UI together.
func pair(t *testing.T, c *agent.Config, turns []any, e env.Environment, in string, terminal bool) (*agent.Loop, *harness) {
	t.Helper()
	h := newHarness(t, c, in, terminal)
	l := &agent.Loop{Cfg: c, Model: outputs(t, turns...), Env: e}
	h.ui.Print.Loop = l
	i, err := New(h.ui, c, l)
	if err != nil {
		t.Fatal(err)
	}
	l.Hooks = i
	return l, h
}

// outputs replays the canned turns: a string becomes one assistant turn with
// a single bash action running that command, and a model.Message (built with
// submit, for instance) is replayed verbatim.
func outputs(t *testing.T, turns ...any) *model.Deterministic {
	var msgs []model.Message
	for i, turn := range turns {
		switch v := turn.(type) {
		case string:
			msgs = append(msgs, model.Message{
				"role":    "assistant",
				"content": "turn " + string(rune('a'+i)),
				"extra":   map[string]any{"actions": []model.Action{{Command: v, ToolCallID: "c"}}, "cost": 1.0},
			})
		case model.Message:
			msgs = append(msgs, v)
		default:
			t.Fatalf("turn %d has unsupported type %T", i, turn)
		}
	}
	return &model.Deterministic{CostPerCall: 1, Outputs: msgs, ObsTemplate: "{{ .output.returncode }}|{{ .output.output }}"}
}

// submit returns the canned turn that, under the prompt contract, ends the
// run: an assistant message that makes no tool call. Its text is the
// submission.
func submit(text string) model.Message {
	return model.Message{
		"role":    "assistant",
		"content": text,
		"extra":   map[string]any{"cost": 1.0},
	}
}

// submitCmd is the old sentinel echo. Under the no-tool-call contract it is
// just an ordinary command; tests still type or run it to prove nothing
// special happens to it.
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
	l, h := pair(t, c, []any{"echo hi", submit("done")}, &env.Local{}, "\n/quit\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if kinds(h) != "confirm,new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
	if !strings.Contains(h.output(), "Execute 1 action(s)?") {
		t.Errorf("output = %q", h.output())
	}
	if !containsContent(l.Messages(), "hi") {
		t.Errorf("messages = %s", contentsOf(l.Messages()))
	}
}

func TestTypedTextRejectsAndReappearsInHistory(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []any{"echo not-run", submit("first\n")}, &env.Local{},
		"no, use printf instead\n\n/quit\n", true)
	info, err := l.Run(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	want := "no, use printf instead"
	if !containsContent(l.Messages(), want) {
		t.Errorf("rejection comment missing:\n%s", contentsOf(l.Messages()))
	}
	// The refusal is recorded as the call's own result *before* the human's
	// comment, not after it: a user message landing between tool_results breaks
	// provider replay for some backends. (The reason text itself is asserted in
	// TestRefusedPutsTheReasonInTheCallResult and the agent's
	// TestRefusedObservationsReachTheHistory - this scripted model's observation
	// template renders rc and output only, so asserting it here would only
	// prove that a template can print what it does not include.)
	msgs := l.Messages()
	comment, refusal := -1, -1
	for i, m := range msgs {
		txt, _ := m["content"].(string)
		switch {
		case m["role"] == "user" && strings.Contains(txt, "no, use printf instead"):
			comment = i
		case m["role"] == "tool" && strings.HasPrefix(txt, "-1"):
			refusal = i
		}
	}
	if comment < 0 || refusal < 0 {
		t.Fatalf("missing refusal or comment in\n%s", contentsOf(msgs))
	}
	if refusal > comment {
		t.Errorf("the refusal observation (%d) lands after the user message (%d): replay would break", refusal, comment)
	}
	if kinds(h) != "confirm,new-task,new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
	if containsContent(l.Messages(), "|not-run") {
		t.Error("the rejected command ran anyway")
	}
	if !containsExceptionInfo(l.Messages(), "action was not executed") {
		t.Errorf("padded observation missing:\n%s", contentsOf(l.Messages()))
	}
	if info["exit_status"] != "Submitted" || info["submission"] != "first\n" {
		t.Errorf("info = %#v", info)
	}
	// The rejection comment reaches the model as the human's own user message,
	// not as a sentence the harness wrote on their behalf.
	var asUser bool
	for _, m := range l.Messages() {
		if m["role"] == "user" && strings.Contains(m["content"].(string), want) {
			asUser = true
		}
	}
	if !asUser {
		t.Error("the rejection text never reached the model")
	}
}

func TestWhitelistedActionsNeverPrompt(t *testing.T) {
	// Both entries are written to match a whole command, which is what a
	// whitelist entry now has to do. `echo .*` used to be the example here, and
	// it is the shape that makes a whole-command rule meaningless - it matches
	// any command that merely starts with "echo ".
	c := newConfig("confirm", `echo \w+`, `cat \S+`)
	l, h := pair(t, c, []any{"echo safe", submit("done")}, &env.Local{}, "\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(kinds(h), "confirm") {
		t.Errorf("echo .* was whitelisted yet a confirmation was asked: %s", kinds(h))
	}
}

func TestWhitelistDoesNotMatchMidCommand(t *testing.T) {
	// The left edge. Go's MatchString is unanchored, so without a leading "^"
	// a bare "ls" would approve "echo leak; ls /tmp" on the strength of its
	// last word.
	//
	// This test used to be called TestWhitelistIsPrefixAnchored and to fail
	// with the words "approved a command that merely contains the pattern",
	// which together read as though the whole command had to match. It did not,
	// and the right edge was never tested (A-1). The right edge now lives in
	// TestWhitelistApprovesOnlyWhatTheHumanAuthorized, and the old rule is kept
	// honest by TestOldGateApprovedEveryBypassRow.
	c := newConfig("confirm", "ls")
	c.ConfirmExit = false
	l, h := pair(t, c, []any{"echo leak; ls /tmp", submit("done")}, &env.Local{}, "\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kinds(h), "confirm") {
		t.Fatal("a whitelist pattern matched a command mid-string that it does not describe")
	}
}

func TestSlashYConfirmsAndStopsLaterPrompts(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []any{"echo one", "echo two", submit("done")}, &env.Local{}, "/y\n/quit\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if c.Mode != "yolo" {
		t.Errorf("mode = %s, want yolo", c.Mode)
	}
	if kinds(h) != "confirm,new-task" {
		t.Errorf("questions = %s, want one confirm prompt then the exit prompt", kinds(h))
	}
	if !containsContent(l.Messages(), "one") {
		t.Error("/y must also confirm the batch it was typed at")
	}
	if !strings.Contains(h.output(), "Switched to yolo mode") {
		t.Errorf("output = %q", h.output())
	}
}

func TestSlashURejectsBatchAndSwitchesToHuman(t *testing.T) {
	c := newConfig("confirm")
	c.ConfirmExit = false
	l, _ := pair(t, c, []any{"echo not-run", submit("done")}, &env.Local{}, "/u\necho COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !containsContent(l.Messages(), "Commands not executed. Switching to human mode") {
		t.Errorf("switch message missing:\n%s", contentsOf(l.Messages()))
	}
	if !containsContent(l.Messages(), "User command: \n```bash\necho COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n```") {
		t.Errorf("human command message missing:\n%s", contentsOf(l.Messages()))
	}
}

func TestHumanModeSkipsTheModel(t *testing.T) {
	c := newConfig("human")
	c.ConfirmExit = false
	// Human mode answers the typed command itself; the model is consulted only
	// once stdin runs out, for the scripted no-action turn that ends the run.
	l, _ := pair(t, c, []any{submit("done")}, &env.Local{}, "echo COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if l.NCalls != 1 {
		t.Errorf("the model must be reached only for the final submission turn, n_calls = %d", l.NCalls)
	}
	if !containsContent(l.Messages(), "User command: \n```bash\necho COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT\n```") {
		t.Errorf("messages:\n%s", contentsOf(l.Messages()))
	}
}

func TestAlreadyInModeRePrompts(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []any{"echo hi", submit("done")}, &env.Local{}, "/c\n\n/quit\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.output(), "Already in confirm mode") {
		t.Errorf("output = %q", h.output())
	}
	if kinds(h) != "confirm,new-task" {
		t.Errorf("questions = %s", kinds(h))
	}
}

func TestSlashHShowsHelpWithoutAnswering(t *testing.T) {
	c := newConfig("confirm")
	l, h := pair(t, c, []any{"echo hi", submit("done")}, &env.Local{}, "/h\n\n/quit\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	out := h.output()
	for _, want := range []string{"Current mode: confirm", "/y to switch to yolo mode", "/m to enter multiline comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("help text missing %q:\n%s", want, out)
		}
	}
	if kinds(h) != "confirm,new-task" {
		t.Errorf("/h should not have answered the prompt: %s", kinds(h))
	}
}

func TestSubmittedWithTypedTaskContinuesTheRun(t *testing.T) {
	c := newConfig("yolo")
	l, _ := pair(t, c, []any{submit("done"), submit("second")}, &env.Local{}, "and now do it again\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !containsContent(l.Messages(), "and now do it again") {
		t.Errorf("new task message missing:\n%s", contentsOf(l.Messages()))
	}
	if containsContent(l.Messages(), "The user added a new task") {
		t.Error("the next turn is being announced as a discrete task again")
	}
	if !containsContent(l.Messages(), "second") {
		t.Error("the run did not continue after the new task")
	}
}

func TestEnterAtExitPromptReprompts(t *testing.T) {
	// Enter at the end of a turn is a no-op, not a quit signal: the run
	// survived both empty enters and only the explicit /quit ended it.
	c := newConfig("yolo")
	l, h := pair(t, c, []any{submit("first\n")}, &env.Local{}, "\n\n/quit\n", true)
	info, err := l.Run(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" || info["submission"] != "first\n" {
		t.Errorf("info = %#v", info)
	}
	if kinds(h) != "new-task,new-task,new-task" {
		t.Errorf("questions = %s, want a re-prompt per empty enter", kinds(h))
	}
	if !strings.Contains(h.output(), newTaskTitle) {
		t.Errorf("the exit prompt was not re-asked:\n%s", h.output())
	}
}

func TestEOFAtExitPromptEndsTheRunInsteadOfLooping(t *testing.T) {
	// Re-prompting an exhausted stdin would spin forever: EOF at the exit
	// prompt stands in for the explicit /quit and ends the run cleanly.
	c := newConfig("yolo")
	l, h := pair(t, c, []any{submit("first\n")}, &env.Local{}, "\n", true)
	info, err := l.Run(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" || info["submission"] != "first\n" {
		t.Errorf("info = %#v", info)
	}
	if kinds(h) != "new-task,new-task" {
		t.Errorf("questions = %s, want the re-prompt that hit EOF", kinds(h))
	}
}

func TestConfirmExitFalseNeverPrompts(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []any{submit("done")}, &env.Local{}, "", true)
	info, _ := l.Run(context.Background(), "T")
	if info["exit_status"] != "Submitted" {
		t.Errorf("info = %#v", info)
	}
	if len(h.ui.kinds) != 0 {
		t.Errorf("unexpected prompts: %s", kinds(h))
	}
}

func TestInterruptIsRecordedWhenCancelled(t *testing.T) {
	c := newConfig("yolo")
	l, _ := pair(t, c, []any{"sleep 20"}, &env.Local{Timeout: 30 * time.Second}, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err := l.Run(ctx, "T"); err == nil {
		t.Fatal("expected the cancelled run to end with an error")
	}
	if !containsContent(l.Messages(), "Temporary interruption caught") {
		t.Errorf("interruption message missing:\n%s", contentsOf(l.Messages()))
	}
}

func TestMultilineCommentViaSlashM(t *testing.T) {
	c := newConfig("confirm")
	c.ConfirmExit = false
	l, _ := pair(t, c, []any{"echo hi", submit("done")}, &env.Local{}, "/m\nfirst line\nsecond line\n\n\n\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !containsContent(l.Messages(), "first line\nsecond line") {
		t.Errorf("multiline rejection text missing:\n%s", contentsOf(l.Messages()))
	}
}

func TestPrintHeadersMatchTheReferenceFormat(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []any{"echo hi", submit("done")}, &env.Local{}, "\n", true)
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
	l, h := pair(t, c, []any{"echo hi"}, &env.Local{}, "", true)
	h.ui.Print.Loop = l
	h.ui.Print.SetThinkingShown(true) // the streaming half of the /t preference
	// Simulate a streaming model: the UI sees chunks, then the finished message.
	l.Handler = &model.Handler{
		Content:   func(s string) { h.ui.Content(s) },
		Reasoning: func(s string) { h.ui.Reasoning(s) },
	}

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
	l, _ := pair(t, c, []any{"echo hi", submit("done")}, &env.Local{}, "", true)
	_, err := l.Run(context.Background(), "T")
	if err == nil || !strings.Contains(err.Error(), "no input available") {
		t.Fatalf("err = %v", err)
	}
	if l.NCalls > 1 {
		t.Errorf("n_calls = %d, want the run to stop at the first prompt", l.NCalls)
	}
}

// TestTranscriptShowsCommandDirAndExit is the human-readable transcript the
// JSON observation never gave: what ran, where, how it ended, what it printed.
func TestTranscriptShowsCommandDirAndExit(t *testing.T) {
	dir := t.TempDir()
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []any{"printf 'hi\\nthere\\n'; echo err >&2", submit("done")},
		&env.Local{CWD: dir, Timeout: 10 * time.Second}, "", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	out := h.output()
	for _, want := range []string{"$ printf 'hi\\nthere\\n'; echo err >&2", "  in " + dir, "exit 0", "hi\nthere", "err"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
	// and no JSON wire format leaking into the transcript
	for _, notWant := range []string{`"returncode"`, `"output"`, `"raw_output"`} {
		if strings.Contains(out, notWant) {
			t.Errorf("transcript shows the model's JSON view (%s):\n%s", notWant, out)
		}
	}
}

func TestTranscriptShowsFailingCommandAndCWD(t *testing.T) {
	dir := t.TempDir()
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []any{"cat definitely-missing", submit("done")},
		&env.Local{CWD: dir, Timeout: 10 * time.Second}, "", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	out := h.output()
	for _, want := range []string{"$ cat definitely-missing", "  in " + dir, "exit 1", "No such file or directory"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
}

// TestThinkingStateIsVisible: the status line always says whether thinking is
// on, and a turn with hidden thinking says how much is hidden.
func TestThinkingStateIsVisible(t *testing.T) {
	c := newConfig("yolo")
	c.ConfirmExit = false
	l, h := pair(t, c, []any{submit("done")}, &env.Local{Timeout: 10 * time.Second}, "", true)
	// Thinking is collapsed until the preference says otherwise (ui/prefs.go),
	// and this test is about the shown half of that switch.
	h.ui.Print.SetThinkingShown(true)
	l.Handler = &model.Handler{
		Reasoning: func(s string) { h.ui.Reasoning(s) },
		Content:   func(s string) { h.ui.Content(s) },
	}
	l.Handler.Reasoning("pondering the meaning of ls")
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.output(), "[thinking] pondering the meaning of ls") {
		t.Errorf("print mode never showed the thinking text:\n%s", h.output())
	}
}

// The gate is about starting work. Reading the registry and ending a job that
// already started must not cost a prompt - least of all job_kill, which is what
// you reach for when something is wrong and burning the GPU right now.
func TestJobToolsSkipTheConfirmationGate(t *testing.T) {
	// First prove the gate is armed in this configuration, so the negative
	// result below means something.
	l, h := pair(t, newConfig("confirm"), []any{"echo hi", submit("done")},
		&env.Local{Timeout: 5 * time.Second}, "\n/quit\n", true)
	if _, err := l.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kinds(h), "confirm") {
		t.Fatalf("bash did not prompt in confirm mode: kinds=%q", kinds(h))
	}

	kill := model.Message{
		"role": "assistant", "content": "stop it",
		"extra": map[string]any{"actions": []model.Action{
			{Tool: model.ToolJobs, ToolCallID: "c1"},
			{Tool: model.ToolJobKill, Command: "j1", ToolCallID: "c2"},
		}, "cost": 1.0},
	}
	l2, h2 := pair(t, newConfig("confirm"), []any{kill, submit("done")},
		&env.Local{Timeout: 5 * time.Second}, "/quit\n", true)
	if _, err := l2.Run(context.Background(), "T"); err != nil {
		t.Fatal(err)
	}
	if k := kinds(h2); strings.Contains(k, "confirm,") || strings.Contains(k, ",confirm") || k == "confirm" {
		t.Errorf("the job tools prompted: kinds=%q", k)
	}
	// And they still ran: the loop answered both calls.
	if got := len(observationsOf(l2)); got != 2 {
		t.Errorf("observations = %d, want one per job tool call", got)
	}
}

func observationsOf(l *agent.Loop) []model.Message {
	var out []model.Message
	for _, m := range l.Messages() {
		if model.IsToolResult(m) {
			out = append(out, m)
		}
	}
	return out
}
