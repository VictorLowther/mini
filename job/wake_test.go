package job

import (
	"fmt"
	"testing"
	"time"

	"github.com/VictorLowther/mini/session"
)

// An idle queue is empty, not an error, and it stays that way until something
// actually ends. The step-boundary drain runs on every turn of every session,
// including the overwhelming majority that have never backgrounded anything, so
// the quiet path has to be the cheap one.
func TestClaimFiringsWhenNothingHappened(t *testing.T) {
	m, _ := newMgr(t)
	got, err := m.ClaimFirings(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("firings = %+v", got)
	}
	if n := m.OwedDeliveries(); n != 0 {
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
	m, _ := newMgr(t)
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
	if n := m.OwedDeliveries(); n != 0 {
		t.Errorf("owed after claim = %d", n)
	}
}

// The card asks whether a job has a result still sitting in the queue, which is
// how a human sees the difference between "finished" and "finished and nobody has
// been told yet". Any firing counts: when monitors exist, a match wake holds the
// card in the same state.
func TestHasUndeliveredFiringTracksTheQueue(t *testing.T) {
	m, _ := newMgr(t)
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

// The registry counts live jobs, and a run that finished inside its budget was
// never one - otherwise every `ls` in a long session would eat a slot. This is
// the lazy-registration invariant, which stands on its own now that there is no
// cap to demonstrate it with.
func TestRunningCountOnlyIncludesJobs(t *testing.T) {
	m, _ := newMgr(t)
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
	// No count bound: a second live job is simply admitted, and the count
	// follows. What still must not happen is a finished run leaving a slot
	// behind, which is the assertion above.
	if _, err := m.Start(Spec{Command: "sleep " + markerFor(t), TCNO: 3}); err != nil {
		t.Fatalf("a second live job was refused: %v", err)
	}
	if n := m.StillRunning(); n != 2 {
		t.Errorf("StillRunning = %d with two live jobs, want 2", n)
	}
	_ = slow
	time.Sleep(10 * time.Millisecond)
}

// A-10. The card's "finished, not yet reported" flag used to be decided by
// scanning a 32-row window of the queue for the job id, so once a session owed
// more than 32 results the ones at the back of the queue read as settled. With
// no cap on live jobs that backlog is now ordinary: a held session, or jobs
// ending faster than step boundaries arrive.
func TestUndeliveredFiringIsFoundPastTheProbeWindow(t *testing.T) {
	m, sess := newMgr(t)
	const total = 40

	// The window ordered by id, so the target must be the LAST event queued.
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("j%d", i)
		if _, err := sess.CreateJob(session.JobRow{
			ID: id, Label: id, Command: "soak", OutputPath: sess.JobPath(id),
			StartedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := sess.RecordJobEvent(session.JobEventRow{
			JobID: id, MonitorID: "exit", Reason: session.JobCompleted,
			FiredAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if n := m.OwedDeliveries(); n != total {
		t.Fatalf("owed = %d, want %d", n, total)
	}
	// Every one of them is owed, including the ones beyond the old window.
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("j%d", i)
		if !m.HasUndeliveredFiring(id) {
			t.Fatalf("job %s reported as settled while its result is still queued (position %d of %d)", id, i, total)
		}
	}
	// A job that never ended is not owed, and the answer is per-job rather
	// than "the queue is non-empty".
	if _, err := sess.CreateJob(session.JobRow{ID: "j900", Label: "running",
		Command: "sleep 30", OutputPath: sess.JobPath("j900"), StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if m.HasUndeliveredFiring("j900") {
		t.Error("a job with nothing queued for it reported as owed")
	}
	if m.HasUndeliveredFiring("j000-nonexistent") {
		t.Error("an unknown job reported as owed")
	}

	// Delivery retires the debt exactly.
	got, err := m.ClaimFirings(total)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("claimed %d, want %d", len(got), total)
	}
	if m.HasUndeliveredFiring("j40") {
		t.Error("still flagged as owed after being delivered")
	}
	if n := m.OwedDeliveries(); n != 0 {
		t.Errorf("owed = %d after the drain, want 0", n)
	}
}
