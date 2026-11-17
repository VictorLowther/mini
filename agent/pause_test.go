package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// Pause holds a session at its next step boundary and resume carries on from
// there. §5.7 accepted starvation of background sessions only because pause and
// kill were the remedies; this is the pause half, and the point of it is that
// nothing is lost - the transcript, the jobs and the place in the conversation
// all stay exactly where they were.

type runResult struct {
	info map[string]any
	err  error
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestPauseHoldsBeforeTheNextStep is the hold and its release, parked at the very
// first boundary so the test cannot race the loop: pause before the run starts, and
// no model call may happen until the session is released.
func TestPauseHoldsBeforeTheNextStep(t *testing.T) {
	l := newLoop(t, &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("looking", "echo one"),
		done("the line is there"),
	}}, &env.Local{Timeout: 10 * time.Second})

	l.Pause()
	finished := make(chan runResult, 1)
	go func() {
		info, err := l.Run(context.Background(), "count the lines")
		finished <- runResult{info, err}
	}()

	time.Sleep(300 * time.Millisecond)
	if got := l.Snapshot().Calls; got != 0 {
		t.Errorf("%d model calls while held, want none", got)
	}
	if !l.Running() {
		t.Error("the loop ended rather than waiting at the boundary")
	}
	if !l.Paused() {
		t.Error("Paused() reports false while the session is held")
	}
	select {
	case r := <-finished:
		t.Fatalf("the run returned while paused: %+v %v", r.info, r.err)
	default:
	}

	l.Unpause()
	if l.Paused() {
		t.Error("still paused after Unpause")
	}
	select {
	case r := <-finished:
		if r.err != nil {
			t.Fatalf("resumed run failed: %v", r.err)
		}
		if got, _ := r.info["exit_status"].(string); got != "Submitted" {
			t.Errorf("exit_status = %q, want the submission the second turn carries", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed run never finished")
	}
	// Carrying on means the whole conversation, not a fresh start: both turns ran.
	if got := l.Snapshot().Calls; got != 2 {
		t.Errorf("%d model calls after resuming, want both turns", got)
	}
}

// TestPauseLeavesTheTurnInFlightToFinish is the difference between pause and esc.
// The hold is asked for while a command is running, and that command's result must
// still land in the history - abandoning a write half-done is what CancelTurn is
// for, and pause promises the opposite.
func TestPauseLeavesTheTurnInFlightToFinish(t *testing.T) {
	l := newLoop(t, &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("looking", "sleep 1 && echo SLEPTOK"),
		done("the line is there"),
	}}, &env.Local{Timeout: 10 * time.Second})

	finished := make(chan runResult, 1)
	go func() {
		info, err := l.Run(context.Background(), "count the lines")
		finished <- runResult{info, err}
	}()

	eventually(t, "the model call that issues the command", func() bool { return l.Snapshot().Calls >= 1 })
	time.Sleep(200 * time.Millisecond) // mid-command, for certain
	l.Pause()

	// The step completes: the command's result reaches the history (observations
	// land as user messages, as everywhere in mini's prompt contract), and the run
	// is still going because the boundary holds rather than because work remains.
	eventually(t, "the command's result to land while held", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, m := range l.Messages {
			if strings.Contains(fmt.Sprint(m["content"]), "SLEPTOK") {
				return true
			}
		}
		return false
	})
	calls := l.Snapshot().Calls
	time.Sleep(300 * time.Millisecond)
	if got := l.Snapshot().Calls; got != calls {
		t.Errorf("%d model calls while held, want the boundary to be the last one", got)
	}

	l.Unpause()
	select {
	case r := <-finished:
		if r.err != nil {
			t.Fatalf("resumed run failed: %v", r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed run never finished")
	}
}

// TestHeldSessionStillEndsOnCancel is why the wait is a select and not a plain
// channel receive: esc and quitting have to work on a session that is parked, or
// pause would be a way to build a run nobody can stop.
func TestHeldSessionStillEndsOnCancel(t *testing.T) {
	l := newLoop(t, &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		done("never reached"),
	}}, &env.Local{Timeout: 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	l.Pause() // before the run starts, so it parks at the very first boundary
	finished := make(chan runResult, 1)
	go func() {
		info, err := l.Run(ctx, "task")
		finished <- runResult{info, err}
	}()

	time.Sleep(200 * time.Millisecond)
	if got := l.Snapshot().Calls; got != 0 {
		t.Errorf("%d model calls before release, want none", got)
	}

	cancel()
	select {
	case r := <-finished:
		if r.err == nil {
			t.Fatal("the held run returned without an error after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled session stayed parked - pause made the run unkillable")
	}
	// The hold survives the run; the next run starts released only when the human
	// says so, which is what makes the ⏸ on the tab a true statement.
	if !l.Paused() {
		t.Error("cancelling the run quietly cleared the pause")
	}
}
