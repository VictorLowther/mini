package agent

import (
	"context"
	"testing"
	"time"

	"github.com/VictorLowther/mini/model"
)

// The signal is a hint and the store is the truth: a waiter must be able to
// ignore a notify with no debt behind it, and must see debt armed before anybody
// started waiting.
func TestWorkSignalArmsAndDrains(t *testing.T) {
	l := &Loop{}
	if l.PendingWork() {
		t.Fatal("a bare loop owes nothing")
	}
	// WorkSignal must always hand back a selectable channel: a nil one never
	// fires, and a prompt that silently cannot be woken is the bug this file
	// exists to close.
	select {
	case <-l.WorkSignal():
		t.Fatal("a fresh signal is empty")
	default:
	}

	l.NotifyWork()
	select {
	case <-l.WorkSignal():
	case <-time.After(time.Second):
		t.Fatal("NotifyWork did not reach a waiter")
	}
	if l.PendingWork() {
		t.Fatal("a bare notify invented work")
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 64; i++ {
			l.NotifyWork()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NotifyWork blocked the notifier")
	}
	l.ClearWorkSignal()
	l.ClearWorkSignal() // idempotent
}

func TestRequestCompactionArmsPendingWorkAndSignal(t *testing.T) {
	l := &Loop{}
	l.RequestCompaction()
	if !l.CompactionPending() || !l.PendingWork() {
		t.Fatal("RequestCompaction armed nothing")
	}
	select {
	case <-l.WorkSignal():
	case <-time.After(time.Second):
		t.Fatal("a compaction request must wake a parked prompt by itself")
	}
}

// A session parked at an idle prompt is already standing at a safe step
// boundary, so /compact has to mean now rather than sometime.
func TestCompactHereCompactsAtAnIdleBoundary(t *testing.T) {
	d := model.NewDeterministic("never queried")
	d.Summaries = []string{"THE SUMMARY"}
	l, _ := compactionLoop(t, d)
	l.messages = idleHistory(4)

	l.RequestCompaction()
	if !l.CompactHere(context.Background()) {
		t.Fatal("CompactHere did not run the compaction it was asked for")
	}
	if l.CompactionPending() {
		t.Fatal("the request survived a compaction that succeeded")
	}
	var anchored bool
	for _, m := range l.messages {
		if model.IsCompaction(m) {
			anchored = true
		}
	}
	if !anchored {
		t.Fatal("no compaction row was written at the idle boundary")
	}
	// The transcript is append-only: the anchor is appended, and nothing that
	// was already there is gone.
	if want := len(idleHistory(4)) + 1; len(l.messages) != want {
		t.Fatalf("history is %d rows, want %d (the seeded transcript plus the anchor)", len(l.messages), want)
	}
}

// A compaction that cannot run must still consume the request, or every later
// prompt wakes, fails, and wakes again forever.
func TestCompactHereConsumesTheRequestEvenOnFailure(t *testing.T) {
	d := model.NewDeterministic("never queried") // no canned summary: Summarize errors
	l, _ := compactionLoop(t, d)
	l.messages = idleHistory(4) // big enough that the cut is real and Summarize is reached
	l.RequestCompaction()
	if l.CompactHere(context.Background()) {
		t.Fatal("a compaction whose summary call failed reported success")
	}
	if l.CompactionPending() {
		t.Fatal("the request survived a failed compaction and will re-wake forever")
	}
}

// idleHistory is a settled transcript of n assistant turns plus their
// observations: the shape a session has when it is parked at a prompt, with a
// real tail budget worth cutting into and an assistant boundary to cut at.
func idleHistory(n int) []model.Message {
	big := ""
	for i := 0; i < 3000; i++ {
		big += "x"
	}
	msgs := []model.Message{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "instance"},
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			model.Message{"role": "assistant", "content": big, "extra": map[string]any{
				"actions": []model.Action{{Command: big, ToolCallID: "c1"}}, "cost": 1.0}},
			model.Message{"role": "user", "content": big, "extra": map[string]any{"turn": i + 2}},
		)
	}
	return msgs
}

// humanOnlyHistory is the transcript shape that crashed: prompt plumbing plus
// user rows, and not one assistant turn. Human mode produces exactly this - it
// answers with a user row and its observation and returns before the model is
// queried - so a session can sit on it indefinitely until something asks for a
// compaction.
func humanOnlyHistory() []model.Message {
	big := ""
	for i := 0; i < 2000; i++ {
		big += "y"
	}
	return []model.Message{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "instance"},
		{"role": "user", "content": big, "extra": map[string]any{"actions": []model.Action{{Command: big}}}},
		{"role": "user", "content": big, "extra": map[string]any{"turn": 2}},
	}
}

// A-3. compact() indexed proj[cut] with cut == len(proj), which is a panic and,
// with no recover on the run seam, the end of the process and every session in
// it - live jobs included. Compaction has to decline instead, and leave the
// history untouched.
func TestCompactHereDeclinesWithoutAnAssistantBoundary(t *testing.T) {
	d := model.NewDeterministic("never queried")
	d.Summaries = []string{"S"}
	l, _ := compactionLoop(t, d)
	l.messages = humanOnlyHistory()
	before := len(l.messages)

	l.RequestCompaction()
	if l.CompactHere(context.Background()) {
		t.Fatal("a context with no assistant boundary must not compact")
	}
	if len(l.messages) != before {
		t.Fatalf("a declined compaction changed the history: %d rows, was %d", len(l.messages), before)
	}
	if l.CompactionPending() {
		t.Fatal("the declined request stayed armed and will wake the prompt again")
	}
}

// The same crash through the door it actually came in: Query projects, the
// estimate crosses the trigger, and prepareContext compacts. A server that
// reports a small context window (host.go feeds DiscoverContextWindow straight
// into the config) makes this the first query of the run, not an edge case
// reached after a long human-mode session.
func TestQueryCompactsAnAssistantlessContextWithoutPanicking(t *testing.T) {
	d := model.NewDeterministic("a")
	d.Summaries = []string{"S"}
	l, _ := compactionLoop(t, d, func(c *Config) {
		c.ContextWindow = 1000 // trigger 900; the seeded rows clear it
		c.CompactKeepTokens = 60
	})
	l.messages = humanOnlyHistory()

	if _, err := l.Query(context.Background()); err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, m := range l.messages {
		if model.IsCompaction(m) {
			t.Fatal("no compaction was possible, so none should have been recorded")
		}
	}
}

// A-4's guarantee, as an access pattern rather than a comment. A second reader
// walking the history while the agent goroutine appends to it used to be a race
// that `go test -race ./agent/` reported; the exported field is gone, so the
// pattern is now merely legal. Run under -race to mean anything.
func TestMessagesCanBeReadWhileTheRunAppends(t *testing.T) {
	d := det(4, "checkpoint")
	l, _ := compactionLoop(t, d)
	l.Hooks = nil

	done := make(chan struct{})
	go func() { defer close(done); _, _ = run(t, l, "task") }()

	stop := make(chan struct{})
	reads := make(chan int, 1)
	go func() {
		total := 0
		for {
			select {
			case <-stop:
				reads <- total
				return
			default:
			}
			m := l.Messages() // a copy; the run goroutine keeps appending
			total += len(m)
			for _, m := range m {
				_ = m["role"]
			}
		}
	}()

	<-done
	close(stop)
	if n := <-reads; n == 0 {
		t.Fatal("the concurrent reader never observed a row")
	}
	// The copy must not alias the live array: mutating what we were handed
	// cannot rewrite the conversation.
	got := l.Messages()
	if len(got) == 0 {
		t.Fatal("empty history after a run")
	}
	got[0] = model.Message{"role": "tampered"}
	if l.Messages()[0]["role"] == "tampered" {
		t.Fatal("Messages() aliased the transcript")
	}
}
