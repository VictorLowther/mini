package ui

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// parkedUI is a front end that never answers: it blocks in Ask the way the
// window blocks at "Agent is ready", and honours cancellation the way the window
// does. Nothing can be typed into it, which is the point - a keystroke must not
// be what delivers a job's result.
type parkedUI struct {
	mu       sync.Mutex
	asked    []string
	out      io.Writer
	terminal bool
}

func (u *parkedUI) Reasoning(string)      {}
func (u *parkedUI) Content(string)        {}
func (u *parkedUI) Message(model.Message) {}
func (u *parkedUI) Interactive() bool     { return u.terminal }

func (u *parkedUI) Ask(ctx context.Context, q Question) (Answer, error) {
	u.mu.Lock()
	u.asked = append(u.asked, q.Kind)
	u.mu.Unlock()
	<-ctx.Done()
	return Answer{}, ctx.Err()
}

func (u *parkedUI) askedCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.asked)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// wakeLoop wires an Interactive front end to a real registry over a real store.
func wakeLoop(t *testing.T, c *agent.Config, turns []any) (*agent.Loop, *parkedUI, *job.Manager, *session.Session, func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Create(session.Row{ID: uuid.NewV7(), Workdir: dir, Project: dir, ModelName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	u := &parkedUI{out: io.Discard, terminal: true}
	l := &agent.Loop{Cfg: c, Model: outputs(t, turns...), Env: &env.Local{Timeout: 10 * time.Second}, Recorder: sess}
	jm := &job.Manager{
		Session: sess, Local: &env.Local{Timeout: 10 * time.Second},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	l.Jobs = jm
	// What host.Open now wires on every session.
	jm.OnFiring = func() { l.NotifyWork() }
	i, err := New(u, c, l)
	if err != nil {
		t.Fatal(err)
	}
	l.Hooks = i
	return l, u, jm, sess, func() { jm.KillAll(); st.Close() }
}

// transcriptOf reads the history back through the store. Loop.Messages must not
// be touched from a test goroutine while the agent goroutine owns it - reading
// it directly is the race agent/agent.go:842 documents - and the store is where
// the transcript actually lives.
func transcriptOf(t *testing.T, sess *session.Session) []model.Message {
	t.Helper()
	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func transcriptHas(sess *session.Session, want func(model.Message) bool) bool {
	msgs, err := sess.Transcript()
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if want(m) {
			return true
		}
	}
	return false
}

// A subagent (or any job) that finishes while the session sits at the ready
// prompt must start the next turn itself. Before this, the firing sat in the
// durable queue and the report only reached the model when a human typed -
// a ninety-second subagent reporting twenty minutes later, or never.
func TestIdleReadyPromptWakesForAJobFiring(t *testing.T) {
	c := newConfig("confirm")
	l, u, jm, sess, closer := wakeLoop(t, c, []any{submit("first"), submit("second")})
	defer closer()

	// A job whose ending the test controls, so the firing provably lands while
	// the prompt is parked rather than racing it.
	release := make(chan struct{})
	if _, err := jm.Start(job.Spec{
		Command: "held open by the test",
		Label:   "waker",
		Kind:    session.KindSubagent,
		Run: func(ctx context.Context, out io.Writer) job.Result {
			<-release // the test closes this, once, while the prompt is parked
			code := 0
			return job.Result{ExitCode: &code, Reason: "reported"}
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := l.Run(ctx, "task"); done <- err }()

	waitUntil(t, "the ready prompt to be asked", func() bool { return u.askedCount() >= 1 })
	callsBefore := l.Snapshot().Calls

	close(release) // the job ends; only its firing can move this session now

	waitUntil(t, "the parked prompt to wake and take a turn on its own",
		func() bool { return l.Snapshot().Calls > callsBefore })

	woke := transcriptHas(sess, func(m model.Message) bool {
		return model.Extra(m)["interrupt_type"] == "JobUpdate"
	})
	if !woke {
		t.Fatal("the run advanced without the firing ever reaching the history")
	}
	if u.askedCount() < 1 {
		t.Fatal("the ready prompt was never shown")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end on cancel")
	}
}

// The same session, parked, asked for a compaction. /compact says "before the
// model's next turn" - and an idle session has no next turn coming, so the
// request used to sit armed indefinitely. Here is must run at the boundary the
// prompt is already standing in, and must not spend a model call to do it.
func TestIdleReadyPromptRunsARequestedCompactionWithoutATurn(t *testing.T) {
	c := newConfig("confirm")
	c.ContextWindow = 128000 // far above the seeded history: only /compact compacts
	c.CompactEnabled = true
	c.CompactKeepTokens = 60
	c.CompactSummaryMaxTokens = 100

	dir := t.TempDir()
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, err := st.Create(session.Row{ID: uuid.NewV7(), Workdir: dir, Project: dir, ModelName: "test"})
	if err != nil {
		t.Fatal(err)
	}

	d := model.NewDeterministic()
	d.Summaries = []string{"CHECKPOINTED"}
	u := &parkedUI{out: io.Discard, terminal: true}
	l := &agent.Loop{Cfg: c, Model: d, Env: &env.Local{Timeout: 10 * time.Second}, Recorder: sess}
	i, err := New(u, c, l)
	if err != nil {
		t.Fatal(err)
	}
	l.Hooks = i

	// A long settled transcript, resumed so Run does not wipe it.
	big := ""
	for k := 0; k < 4000; k++ {
		big += "x"
	}
	msgs := []model.Message{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "instance"},
	}
	for k := 0; k < 4; k++ {
		msgs = append(msgs,
			model.Message{"role": "assistant", "content": big, "extra": map[string]any{
				"actions": []model.Action{{Command: big, ToolCallID: "c"}}, "cost": 1.0}},
			model.Message{"role": "user", "content": big, "extra": map[string]any{"turn": k}},
		)
	}
	l.Resume(msgs)
	// The scripted model ends the run at the first turn, so we reach the prompt.
	d.Outputs = append(d.Outputs, submit("done"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { defer close(runDone); _, _ = l.Run(ctx, "") }()
	// The run must be joined before the test returns. A leaked Loop goroutine
	// outlives the test, keeps driving a shared Fyne test app, and turns the
	// next GUI test in the package into a deadlock that names neither culprit.
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("the run did not finish on cancel")
		}
	}()

	waitUntil(t, "the ready prompt to be asked", func() bool { return u.askedCount() >= 1 })
	callsBefore := l.Snapshot().Calls

	l.RequestCompaction()

	waitUntil(t, "the parked session to compact without waiting for a turn",
		func() bool { return transcriptHas(sess, model.IsCompaction) })
	if l.CompactionPending() {
		t.Fatal("the request survived the compaction")
	}
	if got := l.Snapshot().Calls; got != callsBefore {
		t.Fatalf("the compaction spent a model turn to run: %d calls, was %d", got, callsBefore)
	}
}

// A print-mode prompt blocked on stdin must still be abandonable, or waking it
// strands the ask queue behind a read nobody is waiting for.
func TestPrintAskHonoursCancellationWhileReadingStdin(t *testing.T) {
	c := newConfig("confirm")
	pr, _ := io.Pipe() // never written: no keystroke ever arrives
	p := NewPrint(io.Discard, pr, c, nil)
	p.Terminal = true

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.Ask(ctx, Question{Kind: AskNewTask, Title: "parked"})
		errc <- err
	}()

	time.Sleep(150 * time.Millisecond) // long enough that the read is certainly blocked
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Ask returned no error after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a print prompt blocked on stdin ignored its cancellation")
	}
}
