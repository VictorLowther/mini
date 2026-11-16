package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/session"
)

func TestWaitAnyWithNothingRunningIsAnAnswerNotATimeout(t *testing.T) {
	m, _ := newMgr(t, 0)
	row, ok, err := m.WaitAny(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok || row != nil {
		t.Errorf("ok=%v row=%v, want nothing to wait for reported immediately", ok, row)
	}
	r, err := m.Start(Spec{Command: "echo quick", TCNO: 1})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, r)

	// An ending nobody has been told about IS something to wait for. Reporting
	// "nothing to wait here" instead would make a job that finished early
	// invisible, and the model would either re-run it or forget it - so the wait
	// reads the firing queue, not the running set.
	woken, ok, err := m.WaitAny(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || woken == nil || woken.ID != "j1" {
		t.Fatalf("ok=%v row=%v, want the job that ended unreported", ok, woken)
	}
	// And exactly once: the queue is claimed on delivery, so a second wait has
	// genuinely nothing left to report rather than resurfacing stale news.
	if _, ok, err := m.WaitAny(context.Background()); ok || err != nil {
		t.Errorf("second wait: ok=%v err=%v, want the firing consumed", ok, err)
	}
	if n := m.PendingFirings(); n != 0 {
		t.Errorf("undelivered firings after delivery = %d", n)
	}
}

func TestWaitAnyWakesOnTheFirstJobToEnd(t *testing.T) {
	m, _ := newMgr(t, 0)
	slow := markerFor(t)
	fast := "echo first"
	rs, err := m.Start(Spec{Command: "sleep 30 & sleep " + slow, TCNO: 11})
	if err != nil {
		t.Fatal(err)
	}
	rf, err := m.Start(Spec{Command: fast, TCNO: 12})
	if err != nil {
		t.Fatal(err)
	}
	// One child carries the marker; the other is a plain sleep 30. What matters
	// is that the slow run really is live before the wait is asked to notice the
	// fast one.
	markerPIDs(t, "sleep "+slow, 1)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	row, ok, err := m.WaitAny(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row == nil {
		t.Fatal("the wait reported nothing")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("waited %v for a job that finished immediately", d)
	}
	if row.ID != rf.ID() || row.ID != "j12" || row.State != session.JobCompleted {
		t.Errorf("woke on %s (%s), want the finished job %s", row.ID, row.State, rf.ID())
	}
	if row.ExitCode == nil || *row.ExitCode != 0 {
		t.Errorf("exit code = %v", row.ExitCode)
	}
	// The other run is untouched by someone else's wait ending.
	if !rs.Alive() {
		t.Error("the slow job was ended by the wait for the fast one")
	}
	// And the set is fixed at entry: a job that starts after the wait began is
	// not this wait's business.
	ids, err := m.Session.RunningJobIDs()
	if err != nil || len(ids) != 1 {
		t.Errorf("running ids = %v, %v", ids, err)
	}
}

// The guarantee that makes job_wait safe: giving up on a wait must never be
// indistinguishable from killing the job it was watching.
func TestInterruptedWaitLeavesEveryJobAlone(t *testing.T) {
	m, _ := newMgr(t, 0)
	marker := markerFor(t)
	r, err := m.Start(Spec{Command: "sleep " + marker, TCNO: 21})
	if err != nil {
		t.Fatal(err)
	}
	markerPIDs(t, "sleep "+marker, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	row, ok, err := m.WaitAny(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if row != nil || !ok {
		t.Errorf("row=%v ok=%v, want no result but a real wait", row, ok)
	}
	if !r.Alive() {
		end, _ := r.Row()
		t.Fatalf("the cancelled wait ended the job: %+v", end)
	}
	if strings.Contains(r.State(), "kill") {
		t.Errorf("the job was marked killed: %s", r.State())
	}
	// It is still killable afterwards, and still listed.
	if n := m.StillRunning(); n != 1 {
		t.Errorf("StillRunning = %d", n)
	}
}
