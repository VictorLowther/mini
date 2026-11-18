package session

import (
	"testing"
	"time"

	"uuid"
)

// A-6, the half that loses information. A competing opener sweeps a running row
// to 'orphaned'; the process that actually started the job then finishes it, and
// FinishJob's `AND state = 'running'` guard - correct for a live row - means the
// real exit code and its firing were dropped without a word. ReclaimJob is how
// the holder of the live handle puts the truth back.
func TestReclaimJobRecoversASweptOutcome(t *testing.T) {
	st, dir := openStore(t)
	sess, err := st.Create(Row{ID: uuid.NewV7(), Workdir: dir, Project: dir, ModelName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CreateJob(JobRow{
		ID: "j1", Label: "soak", Command: "soak", OutputPath: dir + "/j1.log",
		StartedAt: time.Now().Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	// A second driver opens the session and sweeps.
	warn, err := sess.SweepOrphanedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 1 {
		t.Fatalf("sweep reported %v, want the one job", warn)
	}
	row, err := sess.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != JobOrphaned {
		t.Fatalf("after sweep state = %s, want orphaned", row.State)
	}

	// The real process now ends. FinishJob must still refuse (the row is not
	// ours as a running row) and ReclaimJob must restore the truth.
	code := 0
	if _, err := sess.FinishJob("j1", JobCompleted, &code, "exited 0"); err == nil {
		t.Fatal("FinishJob must not move a row it no longer owns as running")
	}
	got, ok, err := sess.ReclaimJob("j1", JobCompleted, &code, "exited 0")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a swept row must be reclaimable by the process holding the handle")
	}
	if got.State != JobCompleted || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("reclaimed row = %s/%v, want completed/0", got.State, got.ExitCode)
	}

	// Once reclaimed it is no longer swept, so a second reclaim declines rather
	// than rewriting a finished outcome.
	if _, ok, _ := sess.ReclaimJob("j1", JobFailed, &code, "later"); ok {
		t.Fatal("a settled row must not be reclaimable")
	}
	// A running row is not ours to reclaim either: that is FinishJob's job.
	if _, err := sess.CreateJob(JobRow{ID: "j2", OutputPath: dir + "/j2.log", StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := sess.ReclaimJob("j2", JobCompleted, &code, "x"); err != nil || ok {
		t.Fatalf("reclaiming a running row = %v,%v, want false,nil", ok, err)
	}
}
