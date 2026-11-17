package agent

import (
	"context"
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

func TestPauseHoldsAtTheStepBoundary(t *testing.T) {
	l := newLoop(t, &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turn("looking", "echo one"),
		done("the line is there"),
	}}, &env.Local{Timeout: 10 * time.Second})

	finished := make(chan runResult, 1)
	go func() {
		info, err := l.Run(context.Background(), "count the lines")
		finished <- runResult{info, err}
	}()

	// Let one whole step land - the model call and its command - then hold.
	eventually(t, "the first step to land", func() bool { return l.Snapshot().Calls >= 1 })
	l.Pause()
	if !l.Paused() {
		t.Fatal("Paused() reports false immediately after Pause()")
	}

	// Held means: still running, and no further model call. A session that
	// "paused" by ending its run would be indistinguishable from one that died.
	time.Sleep(200 * time.Millisecond)
	if got := l.Snapshot().Calls; got != 1 {
		t.Errorf("%d model calls while held, want the boundary to be the last one", got)
	}
	if !l.Running() {
		t.Error("the loop ended rather than waiting at the boundary")
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
	case <-time.After(10 * time.Second):
		t.Fatal("the resumed run never finished")
	}
	if got := l.Snapshot().Calls; got != 2 {
		t.Errorf("%d model calls after resuming, want both turns", got)
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
