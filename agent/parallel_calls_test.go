package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// barrierEnv is a fake interpreter that will not let a command finish until the
// whole batch has arrived. It exists because the honest way to prove N commands
// run at once is to require it: a serial loop reaches the first Execute and
// waits there forever, so the test fails on its deadline instead of on a wall
// clock a loaded CI box can ruin. The real commands are run by the embedded
// env.Local, so output files and return codes still behave normally.
type barrierEnv struct {
	inner *env.Local

	mu      sync.Mutex
	want    int
	arrived int
	release chan struct{}
	closed  bool
	// running/peak are the concurrency the batch actually reached.
	running int
	peak    int
	// started records the order the commands began in, which under a concurrent
	// loop is deliberately NOT the issue order.
	started []string
}

func newBarrierEnv(t *testing.T, want int) *barrierEnv {
	t.Helper()
	return &barrierEnv{inner: &env.Local{Timeout: 30 * time.Second}, want: want,
		release: make(chan struct{})}
}

func (b *barrierEnv) Execute(ctx context.Context, command, cwd string, out io.Writer) (env.Result, error) {
	b.mu.Lock()
	b.arrived++
	b.running++
	if b.running > b.peak {
		b.peak = b.running
	}
	b.started = append(b.started, command)
	if b.arrived >= b.want && !b.closed {
		b.closed = true
		close(b.release)
	}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.running--
		b.mu.Unlock()
	}()

	select {
	case <-b.release:
	case <-ctx.Done():
		return env.Result{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return env.Result{ReturnCode: 1,
			ExceptionInfo: fmt.Sprintf("barrier: only %d of %d commands ever started", b.snapshot(), b.want)}, nil
	}
	return b.inner.Execute(ctx, command, cwd, out)
}

func (b *barrierEnv) snapshot() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.arrived
}

func (b *barrierEnv) stats() (peak int, started []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak, append([]string(nil), b.started...)
}

func (b *barrierEnv) TemplateVars() map[string]any { return b.inner.TemplateVars() }
func (b *barrierEnv) Serialize() map[string]any    { return b.inner.Serialize() }

func batch(content string, commands ...string) model.Message {
	acts := make([]model.Action, 0, len(commands))
	for _, c := range commands {
		acts = append(acts, model.Action{Command: c})
	}
	return turnActions(content, acts...)
}

// TestBatchOfCallsRunsTogether is the headline claim: a turn that issues N
// commands does not wait for them one at a time.
func TestBatchOfCallsRunsTogether(t *testing.T) {
	const n = 4
	e := newBarrierEnv(t, n)
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		batch("four at once", "echo a", "echo b", "echo c", "echo d"),
		done("all in"),
	}}
	l := newLoop(t, m, e)
	if _, err := run(t, l, "prove concurrency"); err != nil {
		t.Fatal(err)
	}

	peak, started := e.stats()
	if peak < n {
		t.Errorf("peak concurrency = %d of %d; the batch did not run together (started %v)", peak, n, started)
	}
	if got := e.snapshot(); got != n {
		t.Errorf("%d commands started, want %d", got, n)
	}

	// And the work is intact: every command produced its own observation, in the
	// order it was issued, whatever order it finished in.
	obs := resultMessages(l)
	if len(obs) != n {
		t.Fatalf("%d observations, want %d", len(obs), n)
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if got := contentOf(obs[i]); !strings.Contains(got, want) {
			t.Errorf("observation %d = %q, want the output of command %d (%q)", i, got, i, want)
		}
	}
}

// TestResultsAreReportedInIssueOrder is the property the transcript depends on:
// execution order is the machine's business, but results[i] must belong to
// actions[i]. The commands are timed to finish in exactly the reverse order, so
// an implementation that appended as they landed would be caught.
func TestResultsAreReportedInIssueOrder(t *testing.T) {
	specs := []struct{ sleep, mark string }{
		{"0.9", "first"},
		{"0.6", "second"},
		{"0.3", "third"},
	}
	cmds := make([]string, 0, len(specs))
	for _, s := range specs {
		cmds = append(cmds, fmt.Sprintf("sleep %s; echo %s", s.sleep, s.mark))
	}
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		batch("reversed finish", cmds...),
		done("ordered"),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 30 * time.Second})
	if _, err := run(t, l, "order"); err != nil {
		t.Fatal(err)
	}

	obs := resultMessages(l)
	if len(obs) != len(specs) {
		t.Fatalf("%d observations, want %d", len(obs), len(specs))
	}
	for i, s := range specs {
		got := contentOf(obs[i])
		if !strings.Contains(got, s.mark) {
			t.Errorf("observation %d = %q, want %q: results were reported in completion order", i, got, s.mark)
		}
		// Each observation names its own command, so a swapped pairing is
		// visible twice over - in the text and in the metadata.
		if cmd, _ := obs[i]["extra"].(map[string]any)["command"].(string); cmd != cmds[i] {
			t.Errorf("observation %d claims command %q, want %q", i, cmd, cmds[i])
		}
	}
}

// TestResultFilesStayPairedWithTheirCall is the same ordering claim one level
// down: the call row completed with a byte count must be the row whose file the
// command wrote, not a sibling's.
func TestResultFilesStayPairedWithTheirCall(t *testing.T) {
	e := &env.Local{Timeout: 30 * time.Second}
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		batch("paired", "printf 'AAAAAAAAAA'; exit 3", "printf 'BB'; exit 7"),
		done("ok"),
	}}
	l, rec := newRecorderLoop(t, m, e)
	if _, err := run(t, l, "pairing"); err != nil {
		t.Fatal(err)
	}

	if len(rec.opens) != 2 || len(rec.comps) != 2 {
		t.Fatalf("%d opens / %d completions, want 2 and 2", len(rec.opens), len(rec.comps))
	}
	byNumber := map[int]model.ToolOutcome{}
	for _, c := range rec.comps {
		byNumber[c.number] = c.out
	}
	for i, want := range []struct {
		bytes int64
		code  int
	}{{10, 3}, {2, 7}} {
		var number int
		for _, o := range rec.opens {
			if o.idx == i {
				number = o.tc.Number
			}
		}
		got, ok := byNumber[number]
		if !ok {
			t.Fatalf("call %d (action %d) was never completed", number, i)
		}
		if got.Bytes != want.bytes || got.ReturnCode != want.code {
			t.Errorf("action %d -> call %d recorded %d bytes / exit %d, want %d / %d",
				i, number, got.Bytes, got.ReturnCode, want.bytes, want.code)
		}
	}
}

// TestOnCallOpenedIsSerialAndInIssueOrder guards the half of the loop that was
// deliberately NOT parallelised. The window builds one card per call before any
// command starts and then marks each running, and a CallStarted that arrives out
// of order - or two at once - is a card that silently never shows its output.
func TestOnCallOpenedIsSerialAndInIssueOrder(t *testing.T) {
	const n = 4
	e := newBarrierEnv(t, n)
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		batch("announced", "echo 1", "echo 2", "echo 3", "echo 4"),
		done("ok"),
	}}
	l := newLoop(t, m, e)
	stub := &openerHooks{}
	l.Hooks = stub
	if _, err := run(t, l, "announce"); err != nil {
		t.Fatal(err)
	}

	opened, paths, exists := stub.seen()
	if len(opened) != n {
		t.Fatalf("%d announcements, want %d", len(opened), n)
	}
	for i := range opened {
		if want := fmt.Sprintf("echo %d", i+1); opened[i].Command != want {
			t.Errorf("announcement %d was for %q, want %q", i, opened[i].Command, want)
		}
		// Every announcement names a file that exists at that instant, which is
		// the whole promise CallOpener makes.
		if !exists[i] {
			t.Errorf("announcement %d named %q, which did not exist", i, paths[i])
		}
	}
	// Announcing all of them up front is what lets the batch start together:
	// the last card must be open before the last command is waited on.
	if peak, _ := e.stats(); peak < n {
		t.Errorf("peak concurrency = %d of %d: announcements serialised the batch", peak, n)
	}
}

// TestCancelledBatchStillReportsWhatFinished is the behaviour change parallelism
// forces. Serially, an interruption ended the batch and the commands after it
// simply never ran. Concurrently they are already in flight, so the loop settles
// everything that produced a result and interrupts afterwards rather than
// discarding work that exists.
func TestCancelledBatchStillReportsWhatFinished(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		batch("cancel me", "echo quick", "sleep 20", "sleep 20"),
		done("never reached"),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 30 * time.Second})
	stub := &interruptRecorder{}
	l.Hooks = stub

	finished := make(chan struct{})
	go func() { defer close(finished); _, _ = l.Run(context.Background(), "cancel") }()

	for !l.Running() {
		time.Sleep(5 * time.Millisecond)
	}
	// Let the fast command land before pulling the plug, so the batch is
	// genuinely mixed: one finished, the rest killed on the way.
	time.Sleep(700 * time.Millisecond)
	l.CancelTurn()

	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("the cancelled batch never unwound")
	}

	if n := stub.count(); n == 0 {
		t.Error("the interruption was never reported to the hooks")
	}
	obs := resultMessages(l)
	if len(obs) != 3 {
		t.Fatalf("%d observations, want one per issued call", len(obs))
	}
	if got := contentOf(obs[0]); !strings.Contains(got, "quick") {
		t.Errorf("the finished command's result was discarded: %q", got)
	}
	for i := 1; i < 3; i++ {
		if got := contentOf(obs[i]); !strings.Contains(got, "not executed") {
			t.Errorf("observation %d = %q, want the not-executed padding", i, got)
		}
	}
}

// interruptRecorder is a stubHooks that can answer "was this turn interrupted"
// afterwards. The shared stubHooks counts BeforeExecute and OnSubmit but not
// OnInterrupt, and a cancelled batch is exactly the case worth telling apart.
type interruptRecorder struct {
	stubHooks
	mu  sync.Mutex
	ros int
}

func (h *interruptRecorder) OnInterrupt(ctx context.Context) error {
	h.mu.Lock()
	h.ros++
	h.mu.Unlock()
	return h.stubHooks.OnInterrupt(ctx)
}

func (h *interruptRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ros
}

// TestParallelBoundSizesTheFanOut covers the one number that limits the batch,
// including the two settings that mean "no limit" and "one at a time".
func TestParallelBoundSizesTheFanOut(t *testing.T) {
	l := &Loop{}
	cases := []struct {
		name  string
		set   int
		batch int
		want  int
	}{
		{"unset uses the default", 0, 32, DefaultMaxParallelCalls},
		{"never more workers than work", 0, 2, 2},
		{"configured below the batch", 2, 8, 2},
		{"configured above the batch", 16, 5, 5},
		{"negative is unbounded", -1, 40, 40},
	}
	for _, c := range cases {
		l.Cfg = &Config{MaxParallelCalls: c.set}
		if got := l.parallelBound(c.batch); got != c.want {
			t.Errorf("%s: parallelBound(%d) = %d, want %d", c.name, c.batch, got, c.want)
		}
	}
}

// TestSerialBoundStillProducesTheSameTranscript proves the bound is a
// concurrency setting and not a semantic one: with one worker the batch behaves
// exactly as the old loop did.
func TestSerialBoundStillProducesTheSameTranscript(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		batch("one at a time", "echo x", "echo y"),
		done("ok"),
	}}
	l := newLoop(t, m, &env.Local{Timeout: 30 * time.Second}, func(c *Config) {
		c.MaxParallelCalls = 1
	})
	if _, err := run(t, l, "serial"); err != nil {
		t.Fatal(err)
	}
	obs := resultMessages(l)
	if len(obs) != 2 {
		t.Fatalf("%d observations, want 2", len(obs))
	}
	if !strings.Contains(contentOf(obs[0]), "x") || !strings.Contains(contentOf(obs[1]), "y") {
		t.Errorf("observations out of order: %q / %q", contentOf(obs[0]), contentOf(obs[1]))
	}
}

// TestEmptyBatchIsUnchanged keeps the no-actions branch, which is the model
// putting its tools down and is not a batch at all.
func TestSubmitStillSkipsTheWholeMachinery(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{done("submitted")}}
	l := newLoop(t, m, newBarrierEnv(t, 1))
	info, err := run(t, l, "submit only")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != model.KindSubmitted {
		t.Errorf("exit_status = %v", info["exit_status"])
	}
	if got := l.messages[len(l.messages)-1]["role"]; got != "exit" {
		t.Errorf("last role = %v, want exit", got)
	}
}

// TestNoExecutorIsNeverStartedAfterCancel drives the guard that stops a worker
// launching a command into a turn nobody is waiting for any more.
// TestCancelledBeforeTheWorkersStartedNamesNoNewCommands drives the guard that
// keeps a worker from launching a command into a turn nobody is waiting for. The
// batch is handed an already-cancelled context, so every worker must decline; the
// files phase 1 opened still have to be closed, and every call still gets an
// observation.
func TestCancelledBeforeTheWorkersStartedNamesNoNewCommands(t *testing.T) {
	e := newBarrierEnv(t, 3)
	l, rec := newRecorderLoop(t, &model.Deterministic{}, e)

	// The issuing turn is recorded before its calls are opened, which is what
	// allocates each call's transcript slot; ExecuteActions assumes it happened.
	msg := batch("dead turn", "echo a", "echo b", "echo c")
	if _, err := rec.RecordMessage(msg); err != nil {
		t.Fatal(err)
	}
	err := l.ExecuteActions(cancelledCtx(), msg)
	if err == nil {
		t.Fatal("a cancelled batch must interrupt, not report success")
	}
	if got := e.snapshot(); got != 0 {
		t.Errorf("%d commands started against a cancelled context, want 0", got)
	}
	// Every call was still opened and still closed: no descriptor is left behind,
	// and none was completed with a result nobody measured.
	if len(rec.opens) != 3 {
		t.Errorf("%d calls opened, want 3", len(rec.opens))
	}
	if len(rec.comps) != 0 {
		t.Errorf("%d calls completed, want 0 - nothing ran", len(rec.comps))
	}
	obs := resultMessages(l)
	if len(obs) != 3 {
		t.Fatalf("%d observations, want one padded slot per issued call", len(obs))
	}
	for i := range obs {
		if !strings.Contains(contentOf(obs[i]), "not executed") {
			t.Errorf("observation %d = %q, want the not-executed padding", i, contentOf(obs[i]))
		}
	}
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func resultMessages(l *Loop) []model.Message {
	msgs := l.messages
	out := []model.Message{}
	for _, m := range msgs {
		if model.IsToolResult(m) {
			out = append(out, m)
		}
	}
	return out
}

func contentOf(m model.Message) string {
	s, _ := m["content"].(string)
	return s
}

// TestMixedBatchCoversEveryToolKind runs a batch whose three arms go through
// different code: a bash command that outlives its budget and hands off to the
// registry, one that finishes normally, and the jobs tool answering from the
// registry while the first is still burning. Each arm has its own result, its own
// file, and its own slot in the transcript, and only the middle one may be
// completed as a call - a handed-off run keeps its row 'started' because there is
// no exit code to record yet.
func TestMixedBatchCoversEveryToolKind(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("a soak, a tick and a listing",
			model.Action{Command: "sleep 5; echo later"},
			model.Action{Command: "echo now"},
			model.Action{Tool: model.ToolJobs}),
		done("cleanup"),
		done("cleanup after the live job warning"),
	}}
	l, jm, _ := newJobLoop(t, m, &env.Local{Timeout: 30 * time.Second})
	l.CommandBudget = 300 * time.Millisecond

	if _, err := run(t, l, "mixed"); err != nil {
		t.Fatal(err)
	}
	_ = jm

	obs := resultMessages(l)
	if len(obs) != 3 {
		t.Fatalf("%d observations, want 3", len(obs))
	}
	// Slot 0 handed off: pending, with a job id and a path, and no exit status.
	first := obs[0]
	extra, _ := first["extra"].(map[string]any)
	if pending, _ := extra["pending"].(bool); !pending {
		t.Errorf("the long command was not handed off: %q", contentOf(first))
	}
	if jobID, _ := extra["job_id"].(string); jobID == "" {
		t.Errorf("the handoff named no job: %q", contentOf(first))
	}
	// Slot 1 is the short command's own output, not the long one's.
	if got := contentOf(obs[1]); !strings.Contains(got, "now") || strings.Contains(got, "later") {
		t.Errorf("observation 1 = %q, want the fast command's output alone", got)
	}
	if cmd, _ := obs[1]["extra"].(map[string]any)["command"].(string); cmd != "echo now" {
		t.Errorf("observation 1 claims command %q", cmd)
	}
	// Slot 2 belongs to the jobs tool, and asserts something narrower than "it
	// listed the job slot 0 started": the two ran at the same time, so the
	// listing may legitimately predate the handoff. That is the batch contract
	// in action - a command and an introspection of that command cannot be
	// ordered by putting them in one response - and what must survive is that the
	// tool answered for itself, in its own slot, with its own exit status and
	// without inheriting either sibling's result.
	third := obs[2]["extra"].(map[string]any)
	if code, _ := third["returncode"].(int); code != 0 {
		t.Errorf("the jobs tool reported returncode %v", third["returncode"])
	}
	if exc, _ := third["exception_info"].(string); exc != "" {
		t.Errorf("the jobs tool failed: %s", exc)
	}
	if cmd, _ := third["command"].(string); cmd == "echo now" || cmd == "sleep 5; echo later" {
		t.Errorf("observation 2 carries a sibling's command: %q", cmd)
	}
}
