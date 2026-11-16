package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"uuid"
)

func mkJob(t *testing.T, s *Session, id string) *JobRow {
	t.Helper()
	j, err := s.CreateJob(JobRow{ID: id, Command: "sleep 100", Label: "soak"})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestJobRoundTripAndNulls(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)

	j := mkJob(t, s, "j1")
	if j.State != JobRunning || j.Kind != KindBash {
		t.Fatalf("fresh job = %+v", j)
	}
	// The nullable columns must read back as "unknown", never as zero: a
	// subagent's missing exit code and a successful exit 0 are different facts.
	if j.TCNO != nil || j.PID != nil || j.PGID != nil || j.ExitCode != nil || j.EndedAt != nil {
		t.Errorf("fresh job should carry no pid/tcno/exit/end: %+v", j)
	}
	if !strings.HasSuffix(j.OutputPath, "job-j1.log") {
		t.Errorf("OutputPath = %q", j.OutputPath)
	}

	got, err := s.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "soak" || got.Command != "sleep 100" || got.SessionID != s.ID.String() {
		t.Errorf("round trip = %+v", got)
	}

	if err := s.SetJobProcess("j1", 4242, 4242); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob("j1")
	if got.PID == nil || *got.PID != 4242 || got.PGID == nil {
		t.Errorf("pid not recorded: %+v", got)
	}
}

func TestJobIDPatternIsEnforced(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)
	// Ids become path components and the model will quote them back, so a
	// traversal attempt has to fail at the door rather than at the filesystem.
	for _, bad := range []string{"", "../evil", "j/../x", "has space", "_leading", strings.Repeat("j", 130)} {
		if _, err := s.CreateJob(JobRow{ID: bad}); err == nil {
			t.Errorf("id %q was accepted", bad)
		}
	}
	if _, err := s.CreateJob(JobRow{ID: "j9_ok-7"}); err != nil {
		t.Errorf("valid id rejected: %v", err)
	}
}

func TestFinishJobIsFirstWriterWins(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)
	mkJob(t, s, "j1")

	code := 0
	row, err := s.FinishJob("j1", JobCompleted, &code, "exited 0")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != JobCompleted || row.ExitCode == nil || *row.ExitCode != 0 || row.EndedAt == nil {
		t.Errorf("finished row = %+v", row)
	}

	// A kill that lands after the watcher already recorded the outcome must
	// report "unknown", not silently rewrite who ended the run.
	if _, err := s.FinishJob("j1", JobKilled, nil, "killed on request"); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("second finish = %v, want ErrUnknownJob", err)
	}
	again, _ := s.GetJob("j1")
	if again.State != JobCompleted || again.ExitReason != "exited 0" {
		t.Errorf("state was overwritten: %+v", again)
	}
	if _, err := s.RecordJobEvent(JobEventRow{JobID: "j1", Reason: "exit", ExitCode: &code, Context: "done"}); err != nil {
		t.Fatal(err)
	}
	// A kill of a job that never existed is unknown too.
	if _, err := s.FinishJob("j404", JobKilled, nil, ""); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("unknown id = %v", err)
	}
	// And "running" is not something FinishJob may write.
	if _, err := s.FinishJob("j1", JobRunning, nil, ""); err == nil {
		t.Error("FinishJob accepted a non-terminal state")
	}
}

func TestListAndCount(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)
	for _, id := range []string{"j1", "j2", "j3"} {
		mkJob(t, s, id)
	}
	if n, err := s.CountRunningJobs(); err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := s.FinishJob("j2", JobFailed, nil, "exited 1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %d rows", len(list))
	}
	// Newest first: j3 started last, so it leads. A parent/child tree is built
	// from these rows by following parent_id, so nothing may be filtered out
	// here.
	if list[0].ID != "j3" || list[2].ID != "j1" {
		t.Errorf("order = %s..%s", list[0].ID, list[2].ID)
	}
	for _, j := range list {
		if j.ID == "j2" && j.State != JobFailed {
			t.Errorf("j2 state = %s", j.State)
		}
	}
	if n, _ := s.CountRunningJobs(); n != 2 {
		t.Errorf("running count = %d", n)
	}
}

func TestSweepOrphanedMarksAndWarns(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)
	mkJob(t, s, "j1")
	mkJob(t, s, "j2")
	if _, err := s.FinishJob("j2", JobCompleted, nil, "exited 0"); err != nil {
		t.Fatal(err)
	}

	warnings, err := s.SweepOrphanedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want only the still-running row", warnings)
	}
	if !strings.Contains(warnings[0], "ownership was lost") || !strings.Contains(warnings[0], "j1") {
		t.Errorf("warning does not explain itself: %q", warnings[0])
	}
	j1, _ := s.GetJob("j1")
	if j1.State != JobOrphaned || j1.EndedAt == nil {
		t.Errorf("swept row = %+v", j1)
	}
	// A sweep is not a time machine: the completed row keeps its outcome.
	j2, _ := s.GetJob("j2")
	if j2.State != JobCompleted {
		t.Errorf("completed row swept to %s", j2.State)
	}
	// And the second sweep has nothing to say.
	if again, err := s.SweepOrphanedJobs(); err != nil || len(again) != 0 {
		t.Errorf("second sweep = %v, %v", again, err)
	}
}

func TestJobEventsPendingAndDelivered(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)
	mkJob(t, s, "j1")

	line := 4318
	off := int64(90210)
	for i := 0; i < 3; i++ {
		id, err := s.RecordJobEvent(JobEventRow{
			JobID: "j1", MonitorID: "exit", Reason: "match", LineNo: &line,
			ByteOffset: &off, Matched: "val_loss=nan", Context: "epoch 3", Elapsed: 1234,
		})
		if err != nil {
			t.Fatal(err)
		}
		if id <= 0 {
			t.Errorf("event id = %d", id)
		}
	}
	pending, err := s.PendingJobEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d", len(pending))
	}
	if pending[0].Matched != "val_loss=nan" || pending[0].LineNo == nil || *pending[0].LineNo != 4318 {
		t.Errorf("event = %+v", pending[0])
	}
	if pending[0].DeliveredAt != nil {
		t.Error("a fresh firing reads as delivered")
	}
	// Coalescing and job_wait both need the "only what is owed" property.
	if err := s.MarkJobEventsDelivered([]int64{pending[0].ID, pending[1].ID}); err != nil {
		t.Fatal(err)
	}
	left, _ := s.PendingJobEvents(0)
	if len(left) != 1 || left[0].ID != pending[2].ID {
		t.Errorf("after delivery = %+v", left)
	}
	if err := s.MarkJobEventsDelivered(nil); err != nil {
		t.Errorf("empty delivery batch: %v", err)
	}
	// The limit is what keeps a batch drain bounded, so it has to bite.
	lim, err := s.PendingJobEvents(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lim) != 1 || lim[0].ID != pending[2].ID {
		t.Errorf("bounded read = %+v", lim)
	}
}

func TestDeletingSessionTakesItsJobsAndEvents(t *testing.T) {
	st, dir := openStore(t)
	s := newSession(t, st)
	mkJob(t, s, "j1")
	if _, err := s.RecordJobEvent(JobEventRow{JobID: "j1", Reason: "exit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishJob("j1", JobCompleted, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM jobs WHERE id = 'j1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d job rows survived the session", n)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM job_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d firings survived the session", n)
	}
	// The job file lives in the directory the session's cleanup removes, which
	// is why jobs share <tcrBase>/<uuid>/ instead of a tree of their own.
	if entries, err := readDir(filepath.Join(dir, "tcr", s.ID.String())); err == nil && entries != 0 {
		t.Errorf("result directory still holds %d files", entries)
	}
}

func TestJobsAreScopedToTheirSession(t *testing.T) {
	st, _ := openStore(t)
	a := newSession(t, st)
	b := newSession(t, st)
	mkJob(t, a, "j1")
	// The same id in another session is a different job, and one session must
	// never be able to read, finish, or sweep another's rows.
	if _, err := b.GetJob("j1"); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("cross-session read: %v", err)
	}
	if _, err := b.FinishJob("j1", JobKilled, nil, ""); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("cross-session kill: %v", err)
	}
	if w, _ := b.SweepOrphanedJobs(); len(w) != 0 {
		t.Errorf("cross-session sweep: %v", w)
	}
	if n, _ := a.CountRunningJobs(); n != 1 {
		t.Errorf("sweeping b changed a's count: %d", n)
	}
}

func readDir(p string) (int, error) {
	ents, err := os.ReadDir(p)
	return len(ents), err
}

// Job ids are per-session handles, and the live run proved the key has to agree
// with that: with an id-only primary key, the second session in the database
// that backgrounded anything failed with "UNIQUE constraint failed: jobs.id".
// The unit tests missed it because every test used its own database.
func TestSameJobIDInTwoSessions(t *testing.T) {
	st, _ := openStore(t)
	a, b := newSession(t, st), newSession(t, st)

	if _, err := a.CreateJob(JobRow{ID: "j1", Command: "make"}); err != nil {
		t.Fatalf("first session: %v", err)
	}
	if _, err := b.CreateJob(JobRow{ID: "j1", Command: "pytest"}); err != nil {
		t.Fatalf("second session refused the same id: %v", err)
	}
	// Each session sees its own job and only its own job.
	ja, err := a.GetJob("j1")
	if err != nil || ja.Command != "make" {
		t.Errorf("session a = %+v, %v", ja, err)
	}
	jb, err := b.GetJob("j1")
	if err != nil || jb.Command != "pytest" {
		t.Errorf("session b = %+v, %v", jb, err)
	}
	// Ending one does not end the other, which is what the composite key buys
	// for the update path too.
	code := 0
	if _, err := a.FinishJob("j1", JobCompleted, &code, "exited 0"); err != nil {
		t.Fatal(err)
	}
	if row, _ := b.GetJob("j1"); row.State != JobRunning {
		t.Errorf("finishing a's job touched b's: %+v", row)
	}
	// Firings are scoped the same way, and the composite FK still cascades.
	if _, err := b.RecordJobEvent(JobEventRow{JobID: "j1", Reason: "exit"}); err != nil {
		t.Fatalf("event on the second session's job: %v", err)
	}
	if err := st.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetJob("j1"); err != nil {
		t.Errorf("deleting session b removed a's job: %v", err)
	}
}

// A database created by the build with the id-only key has to be repaired on
// open, not fail on the next backgrounded command.
func TestOpenRebuildsTheOldJobShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	st, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Create(Row{ID: uuid.NewV7(), Workdir: "/w", Project: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO jobs (id, kind, session_id, command, state, output_path, started_at, updated_at)
VALUES ('j1','bash',?,'make','completed',?,1,1)`, sess.ID.String(), sess.JobPath("j1")); err != nil {
		t.Fatal(err)
	}
	// Hand the file back exactly as the first build would have left it.
	// Deliberately the *shipped* DDL, column alignment and all: the stored text
	// is verbatim, and a detector that only matches a tidier spelling of it is
	// the bug this test exists to keep dead.
	if _, err := st.db.Exec(`DROP TABLE IF EXISTS job_events; DROP TABLE IF EXISTS jobs;
CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT    NOT NULL PRIMARY KEY,
	kind         TEXT    NOT NULL DEFAULT 'bash',
	session_id   TEXT    NOT NULL,
	tcno         INTEGER,
	label        TEXT    NOT NULL DEFAULT '',
	command      TEXT    NOT NULL DEFAULT '',
	description  TEXT    NOT NULL DEFAULT '',
	state        TEXT    NOT NULL DEFAULT 'running'
		CHECK (state IN ('running','completed','failed','killed','orphaned')),
	pid          INTEGER,
	pgid         INTEGER,
	output_path  TEXT NOT NULL,
	exit_code    INTEGER,
	exit_reason  TEXT NOT NULL DEFAULT '',
	started_at   INTEGER NOT NULL,
	ended_at     INTEGER,
	updated_at   INTEGER NOT NULL,
	FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	reopened, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatalf("reopen after the old-shape downgrade: %v", err)
	}
	defer reopened.Close()
	var ddl string
	if err := reopened.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='jobs'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "PRIMARY KEY (session_id, id)") {
		t.Errorf("open did not rebuild the table:\n%s", ddl)
	}
	var n int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Errorf("rebuilt table is unreadable: %v", err)
	}
	if n != 0 {
		t.Errorf("the old rows survived a key change that could not hold them: %d", n)
	}
	st2, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if err := st2.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='jobs'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "PRIMARY KEY (session_id, id)") {
		t.Error("a second open changed the shape again: the rebuild is not idempotent")
	}
}

// The fourth state of the machine is not in the jobs table, and this is the
// query that expresses it: what does the conversation still owe? A finished job
// with no undelivered firing is the only thing that can make a run continue after
// its model has stopped talking, and a *running* job never appears here - which is
// why ending a run beside a server costs nothing.
func TestUndeliveredFiringsIsTheDebtQuestion(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)

	if n, err := s.UndeliveredFirings(); err != nil || n != 0 {
		t.Fatalf("fresh session: %d, %v", n, err)
	}
	mkJob(t, s, "j1")
	// Running, and nothing said: not a debt yet.
	if n, _ := s.UndeliveredFirings(); n != 0 {
		t.Errorf("a running job owes %d deliveries", n)
	}

	code := 0
	if _, err := s.FinishJob("j1", JobCompleted, &code, "exited 0"); err != nil {
		t.Fatal(err)
	}
	// Finished, unreported: the debt exists only once a firing is written, which
	// is the manager's job - the store cannot know a result is owed until
	// something recorded it.
	if n, _ := s.UndeliveredFirings(); n != 0 {
		t.Errorf("finished job with no firing recorded %d owed", n)
	}
	id, err := s.RecordJobEvent(JobEventRow{JobID: "j1", Reason: JobCompleted, ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UndeliveredFirings(); n != 1 {
		t.Fatalf("after the firing: %d owed, want 1", n)
	}
	if err := s.MarkJobEventsDelivered([]int64{id}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UndeliveredFirings(); n != 0 {
		t.Errorf("after delivery: %d owed, want 0", n)
	}

	// Scoped to the session, like every other question here.
	b := newSession(t, st)
	if _, err := b.CreateJob(JobRow{ID: "j1", Command: "make"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := b.UndeliveredFirings(); n != 0 {
		t.Errorf("the other session owes %d", n)
	}
	if _, err := b.RecordJobEvent(JobEventRow{JobID: "j1", Reason: JobFailed}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UndeliveredFirings(); n != 0 {
		t.Errorf("session b's firing showed up in a's debt: %d", n)
	}
}

// Orphaning is a finish, so it queues a firing like any other ending. Without
// it, a soak killed by the previous process's death leaves the resumed model
// with no reason to expect its work is gone - the log simply stopped growing and
// nobody said so.
func TestSweepOrphanedQueuesTheEnding(t *testing.T) {
	st, _ := openStore(t)
	s := newSession(t, st)
	mkJob(t, s, "j1")
	if _, err := s.CreateJob(JobRow{ID: "j2", Command: "python train.py", Label: "finetune"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SweepOrphanedJobs(); err != nil {
		t.Fatal(err)
	}
	owed, err := s.UndeliveredFirings()
	if err != nil {
		t.Fatal(err)
	}
	if owed != 2 {
		t.Fatalf("owed after sweeping two running jobs = %d, want 2", owed)
	}
	events, err := s.PendingJobEvents(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("pending = %d", len(events))
	}
	for _, e := range events {
		if e.Reason != JobOrphaned {
			t.Errorf("firing reason = %s, want %s", e.Reason, JobOrphaned)
		}
		// An orphan has no exit code and cannot invent one; the useful fact is
		// which run it was, carried in context.
		if e.ExitCode != nil {
			t.Errorf("orphan firing claimed exit code %v", *e.ExitCode)
		}
	}
	// It is debt exactly once: the resumed conversation pays it at a boundary and
	// is not asked to pay it again.
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	if err := s.MarkJobEventsDelivered(ids); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UndeliveredFirings(); n != 0 {
		t.Errorf("owed after delivery = %d", n)
	}
	// And a second sweep, with nothing running, queues nothing new.
	w, err := s.SweepOrphanedJobs()
	if err != nil || len(w) != 0 {
		t.Errorf("second sweep: %v, %v", w, err)
	}
	if n, _ := s.UndeliveredFirings(); n != 0 {
		t.Errorf("second sweep created %d new owed deliveries", n)
	}
}
