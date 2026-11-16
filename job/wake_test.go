package job

import (
	"testing"
	"time"

	"github.com/VictorLowther/mini/session"
)

// An idle queue is empty, not an error, and it stays that way until something
// actually ends. The step-boundary drain runs on every turn of every session,
// including the overwhelming majority that have never backgrounded anything, so
// the quiet path has to be the cheap one.
func TestClaimFiringsWhenNothingHappened(t *testing.T) {
	m, _ := newMgr(t, 0)
	got, err := m.ClaimFirings(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("firings = %+v", got)
	}
	if n := m.PendingFirings(); n != 0 {
		t.Errorf("pending = %d", n)
	}
	if n := m.StillRunning(); n != 0 {
		t.Errorf("running = %d", n)
	}
	// A run that finished inside its budget never queued anything either: it was
	// never a job.
	r, err := m.Start(Spec{Command: "echo hi", TCNO: 1})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, r)
	if _, err := m.ClaimFirings(8); err != nil {
		t.Fatal(err)
	}
}

// Every terminal state queues exactly one wake, whatever ended it - exit,
// failure, kill - and claiming is what removes it. Delivery and observation are
// separate on purpose: an event that is read but not handed to the model must
// still be there for the boundary that will.
func TestClaimFiringsReportsEachEndingOnce(t *testing.T) {
	m, _ := newMgr(t, 0)
	r1, err := m.Start(Spec{Command: "echo done", TCNO: 1})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, r1)
	code := 7
	r2, err := m.Start(Spec{Command: "exit 7", TCNO: 2, Label: "will-fail"})
	if err != nil {
		t.Fatal(err)
	}
	_ = code
	finish(t, r2)
	r3, err := m.Start(Spec{Command: "sleep " + markerFor(t), TCNO: 3})
	if err != nil {
		t.Fatal(err)
	}
	markerPIDs(t, "sleep ", 1)
	if err := r3.Kill(); err != nil {
		t.Fatal(err)
	}
	finish(t, r3)

	got, err := m.ClaimFirings(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("firings = %d, want one per ended job: %+v", len(got), got)
	}
	byID := map[string]session.JobRow{}
	for _, f := range got {
		byID[f.Job.ID] = f.Job
	}
	for id, want := range map[string]string{"j1": session.JobCompleted, "j2": session.JobFailed, "j3": session.JobKilled} {
		row, ok := byID[id]
		if !ok {
			t.Errorf("no firing for %s", id)
			continue
		}
		if row.State != want {
			t.Errorf("%s state = %s, want %s", id, row.State, want)
		}
		if f := byID[id]; f.EndedAt == nil {
			t.Errorf("%s has no end time", id)
		}
	}
	if j2 := byID["j2"]; j2.ExitCode == nil || *j2.ExitCode != 7 {
		t.Errorf("failed job lost its exit code: %+v", j2)
	}
	// The event carries the same facts, so the drain does not have to re-read the
	// world to describe what happened.
	if got[0].Event.Reason == "" || got[0].Event.FiredAt == 0 {
		t.Errorf("event = %+v", got[0].Event)
	}
	// Claimed, once.
	if left, _ := m.ClaimFirings(8); len(left) != 0 {
		t.Errorf("claimed firings came back: %+v", left)
	}
	if n := m.PendingFirings(); n != 0 {
		t.Errorf("pending after claim = %d", n)
	}
}

// The card asks whether a job has a result still sitting in the queue, which is
// how a human sees the difference between "finished" and "finished and nobody has
// been told yet". Any firing counts: when monitors exist, a match wake holds the
// card in the same state.
func TestHasUndeliveredFiringTracksTheQueue(t *testing.T) {
	m, _ := newMgr(t, 0)
	r, err := m.Start(Spec{Command: "echo bye", TCNO: 1})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, r)
	if !m.HasUndeliveredFiring("j1") {
		t.Fatal("an unreported ending is not flagged")
	}
	if m.HasUndeliveredFiring("j99") {
		t.Error("an unknown job is flagged")
	}
	if _, err := m.ClaimFirings(4); err != nil {
		t.Fatal(err)
	}
	if m.HasUndeliveredFiring("j1") {
		t.Error("the ending is still flagged after being delivered")
	}
}

// The cap counts live jobs, and a run that finished inside its budget was never
// one - otherwise every `ls` in a long session would eat a slot.
func TestCapAndRunningCountOnlyIncludeJobs(t *testing.T) {
	m, _ := newMgr(t, 1)
	r, err := m.Start(Spec{Command: "echo quick", TCNO: 1})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, r)
	if n := m.StillRunning(); n != 0 {
		t.Fatalf("StillRunning = %d after the job ended", n)
	}
	slow, err := m.Start(Spec{Command: "sleep " + markerFor(t), TCNO: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n := m.StillRunning(); n != 1 {
		t.Errorf("StillRunning = %d, want 1", n)
	}
	// Cap already reached by the live job: a second *job* is refused.
	if _, err := m.Start(Spec{Command: "echo nope", TCNO: 3}); err == nil {
		t.Error("the cap did not bite")
	}
	_ = slow
	time.Sleep(10 * time.Millisecond)
}
