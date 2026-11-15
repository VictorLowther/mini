package job

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/session"
)

func newMgr(t *testing.T, max int) (*Manager, *session.Session) {
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
	m := &Manager{
		Session:   sess,
		MaxJobs:   max,
		KillGrace: 500 * time.Millisecond,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Nothing here may outlive the test: a leaked sleeper holding a process
	// group is exactly the failure the reap exists to prevent.
	t.Cleanup(func() { m.KillAll(); st.Close() })
	return m, sess
}

// finish waits for a run to leave running, and fails rather than sleeping a
// fixed amount: every assertion below is about what the run became.
func finish(t *testing.T, r *Run) *session.JobRow {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(20 * time.Second):
		t.Fatalf("job %s never finished (state %s)", r.ID(), r.State())
	}
	row, err := r.Row()
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// markerFor hands a test a sleep length no other run produces. Asserting on a
// fixed "sleep 381" makes a process leaked by an earlier test binary - or by a
// different implementation during a rewrite - indistinguishable from a
// survivor of this one, which is a false failure that costs real debugging
// time. The cleanup is best-effort hygiene, not part of the assertion.
var markerSeq int64

func markerFor(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&markerSeq, 1)
	m := strconv.Itoa(9000 + (os.Getpid()%900)*10 + int(n%10))
	t.Cleanup(func() { exec.Command("pkill", "-f", "sleep "+m).Run() })
	return m
}

func pgrep(t *testing.T, pattern string) []string {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", pattern).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// markerPIDs waits for n processes matching pattern, so a test that is about to
// assert they die first proves they were alive.
func markerPIDs(t *testing.T, pattern string, n int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if len(pgrep(t, pattern)) >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never saw %d processes matching %q (saw %d)", n, pattern, len(pgrep(t, pattern)))
}

func TestCompletesAndMergesOutput(t *testing.T) {
	m, sess := newMgr(t, 0)
	r, err := m.Start(Spec{Command: `echo to-stdout; echo to-stderr >&2; echo "line three"`, TCNO: 7, Label: "greeting"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID() != "j7" {
		t.Errorf("id = %q, want the promoted call's tcno", r.ID())
	}
	row := finish(t, r)

	if row.State != session.JobCompleted || row.ExitCode == nil || *row.ExitCode != 0 {
		t.Fatalf("row = %+v", row)
	}
	// stdout and stderr share one file description, so both streams are in the
	// one log the model will read, in the order the kernel saw them.
	data, err := os.ReadFile(row.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"to-stdout", "to-stderr", "line three"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
	if !strings.Contains(got, "to-stdout\nto-stderr\nline three") {
		t.Errorf("merged order wrong: %q", got)
	}
	// A run through the interpreter has no single pid to record: the shell is
	// mini itself, and the processes that matter are the external commands the
	// exec handler started, each in its own session. Recording mini's pid would
	// invite a `kill` that takes the agent down, so nil is the true answer.
	if row.PID != nil || row.PGID != nil {
		t.Errorf("an in-process run claimed a process: pid=%v pgid=%v", row.PID, row.PGID)
	}
	if row.StartedAt == 0 || row.EndedAt == nil || *row.EndedAt < row.StartedAt {
		t.Errorf("timestamps = %+v", row)
	}
	// The store, not the returned handle, is the record of what is live.
	list, err := sess.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "j7" || list[0].Label != "greeting" {
		t.Errorf("listing = %+v", list)
	}
	// A finished run is not addressable for signalling, and its handle is gone.
	if _, err := m.Get("j7"); err == nil {
		t.Error("a finished run stayed in the live set")
	}
}

func TestJobRunsInTheSameInterpreterAsForeground(t *testing.T) {
	// A backgrounded command must not be a different shell. The macOS
	// /bin/bash is 3.2 and has no ${v,,} downcase expansion, so a script that
	// uses it proves the mvdan.cc/sh interpreter is answering - the one that
	// serves every foreground call (env/env.go:1-6) - rather than a forked
	// system shell whose version and builtins differ per machine.
	m, _ := newMgr(t, 0)
	r, err := m.Start(Spec{Command: `v=ABC; echo "case=${v,,}"; arr=(x y z); echo "array=${arr[1]}"; [[ 2 -lt 3 ]] && echo "test=ok"`, TCNO: 91})
	if err != nil {
		t.Fatal(err)
	}
	row := finish(t, r)
	if row.State != session.JobCompleted {
		t.Fatalf("state = %s, reason = %q", row.State, row.ExitReason)
	}
	data, err := os.ReadFile(row.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"case=abc", "array=y", "test=ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q - the job is not running in mini's interpreter", got, want)
		}
	}
}

func TestJobIsNotGivenATimeout(t *testing.T) {
	// Decision 2: a job's length is nobody's business but the model's. A run
	// that outlasts the foreground DefaultTimeout must still be alive at twice
	// it, because the 30-second wall belongs to a tool call, not to a job.
	m, _ := newMgr(t, 0)
	r, err := m.Start(Spec{Command: "sleep 3; echo done"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Kill()
	time.Sleep(1200 * time.Millisecond)
	if !r.Alive() {
		row, _ := r.Row()
		t.Fatalf("run ended early, env.KillGrace-independent deadline fired? %+v", row)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	row := finish(t, r)
	if row.State != session.JobCompleted || strings.Contains(row.OutputPath, "") == false {
		t.Fatalf("row = %+v", row)
	}
	if data, _ := os.ReadFile(row.OutputPath); !strings.Contains(string(data), "done") {
		t.Errorf("output = %q", data)
	}
}

func TestFailureRecordsExitCode(t *testing.T) {
	m, _ := newMgr(t, 0)
	r, err := m.Start(Spec{Command: "echo starting; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	row := finish(t, r)
	if row.State != session.JobFailed {
		t.Fatalf("state = %s", row.State)
	}
	if row.ExitCode == nil || *row.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", row.ExitCode)
	}
	if !strings.Contains(row.ExitReason, "3") {
		t.Errorf("exit reason = %q", row.ExitReason)
	}
}

func TestKillTakesTheWholeGroup(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	m, _ := newMgr(t, 0)
	marker := markerFor(t)
	// One backgrounded grandchild and one foreground child, both in the run's
	// process group. Signalling only the shell would orphan the pair of them,
	// which is the specific failure docs/jobs-research.md §2.1 is about: the
	// thing holding the GPU is never the process the tool call started.
	r, err := m.Start(Spec{Command: "sleep " + marker + " & sleep " + marker, Label: "soak"})
	if err != nil {
		t.Fatal(err)
	}
	markerPIDs(t, "sleep "+marker, 2)

	if !r.Alive() {
		t.Fatal("run should be alive before the kill")
	}
	if err := r.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	row := finish(t, r)
	if row.State != session.JobKilled {
		t.Errorf("state = %s, want killed", row.State)
	}
	// A killed run reports no exit code. Inventing 0 would tell the model the
	// command succeeded, and inventing -1 would be indistinguishable from the
	// timeout shape the foreground path uses for a hung command.
	if row.ExitCode != nil {
		t.Errorf("killed run claimed exit code %v", *row.ExitCode)
	}
	if !strings.Contains(row.ExitReason, "killed") {
		t.Errorf("exit reason = %q", row.ExitReason)
	}
	// The point of the group: nothing matching the marker survives, so no
	// orphan is left holding memory. A zero exit code is fine here - what a
	// SIGINT-killed shell reports is the shell's business, and the state says
	// who ended the run.
	for i := 0; i < 60; i++ {
		if left := pgrep(t, "sleep "+marker); len(left) == 0 {
			break
		}
		if i == 59 {
			t.Fatalf("survivors after kill: %v", pgrep(t, "sleep "+marker))
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Killing again is a no-op, not an error: the model may have pressed it
	// twice, and a job that is already gone must not look like a failure.
	if err := r.Kill(); err != nil {
		t.Errorf("second kill = %v", err)
	}
}

func TestCapRefusesAndNamesTheRemedy(t *testing.T) {
	m, _ := newMgr(t, 1)
	marker := markerFor(t)
	if _, err := m.Start(Spec{Command: "sleep " + marker + " & sleep " + marker}); err != nil {
		t.Fatal(err)
	}
	markerPIDs(t, "sleep "+marker, 2)

	_, err := m.Start(Spec{Command: "echo should not start"})
	if !errors.Is(err, ErrCap) {
		t.Fatalf("second start = %v, want ErrCap", err)
	}
	if !strings.Contains(err.Error(), "job_kill") {
		t.Errorf("refusal must name the remedy: %v", err)
	}
	// The refusal costs nothing: no row, no file, no process.
	if n, _ := m.Session.CountRunningJobs(); n != 1 {
		t.Errorf("running rows after refusal = %d", n)
	}
	if rows, _ := m.Session.ListJobs(); len(rows) != 1 {
		t.Errorf("refusal left %d rows behind", len(rows))
	}
	// And the cap releases when the job ends, which is the only way a long
	// autonomous run can make room for the next one.
	m.KillAll()
	if _, err := m.Start(Spec{Command: "echo room now"}); err != nil {
		t.Errorf("cap did not release after the reap: %v", err)
	}
}

func TestKillAllReapsEveryLiveJob(t *testing.T) {
	m, sess := newMgr(t, 0)
	marker := markerFor(t)
	for _, tc := range []int{11, 12} {
		if _, err := m.Start(Spec{Command: "sleep " + marker + " & sleep " + marker, TCNO: tc}); err != nil {
			t.Fatal(err)
		}
	}
	markerPIDs(t, "sleep "+marker, 4)

	if n := m.KillAll(); n != 2 {
		t.Errorf("KillAll = %d, want 2", n)
	}
	list, err := sess.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range list {
		if !session.Terminal(j.State) {
			t.Errorf("job %s still %s after the reap", j.ID, j.State)
		}
	}
	if n, _ := sess.CountRunningJobs(); n != 0 {
		t.Errorf("%d jobs still running after the reap", n)
	}
	// A second reap has nothing to do and must not report work.
	if n := m.KillAll(); n != 0 {
		t.Errorf("second KillAll = %d", n)
	}
	for i := 0; i < 60; i++ {
		if len(pgrep(t, "sleep "+marker)) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if left := pgrep(t, "sleep "+marker); len(left) != 0 {
		t.Errorf("the reap left processes behind: %v", left)
	}
}

func TestSweepEndsRowsThatSurvivedARestart(t *testing.T) {
	m, sess := newMgr(t, 0)
	// A row left 'running' by a mini that died: nothing in this process can
	// prove anything about that pid, so the sweep must assert only that
	// ownership was lost.
	if _, err := sess.CreateJob(session.JobRow{ID: "j1", Command: "python train.py", Label: "finetune"}); err != nil {
		t.Fatal(err)
	}
	warnings := m.Sweep()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "finetune") {
		t.Fatalf("warnings = %v", warnings)
	}
	row, err := sess.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != session.JobOrphaned || row.EndedAt == nil {
		t.Errorf("row = %+v", row)
	}
	// An orphan has no stopper: it is not this process's to signal, and
	// pretending otherwise is how you kill somebody else's recycled pid.
	if _, err := m.Get("j1"); err == nil {
		t.Error("an orphan must not be addressable for signalling")
	}
	// Reopening the store and sweeping again is quiet: the row already told the
	// truth, and a warning repeated every start is a warning nobody reads.
	if again := m.Sweep(); len(again) != 0 {
		t.Errorf("second sweep = %v", again)
	}
}

func TestWaitDeadlineNeverLosesTheRun(t *testing.T) {
	m, _ := newMgr(t, 0)
	r, err := m.Start(Spec{Command: "sleep " + markerFor(t) + " & sleep " + markerFor(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// This is crush's and zero's rule: a blocked read that runs out of patience
	// answers with state, and the run is still there to be looked at.
	if err := r.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %v", err)
	}
	if !r.Alive() {
		t.Error("an expired wait must not end the job")
	}
	if r.State() != session.JobRunning {
		t.Errorf("state = %s", r.State())
	}
	// And a wait that is not racing a deadline sees the end.
	if err := r.Kill(); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	if err := r.Wait(ctx2); err != nil {
		t.Errorf("wait after kill = %v", err)
	}
}

func TestJobIDShape(t *testing.T) {
	if got := JobID(42); got != "j42" {
		t.Errorf("JobID(42) = %q", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := JobID(0)
		if !session.JobIDPattern.MatchString(id) {
			t.Fatalf("id %q is not path-safe", id)
		}
		seen[id] = true
	}
	if len(seen) < 400 {
		t.Errorf("only %d distinct ids from 500 calls; a collision here would refuse a job", len(seen))
	}
}

func TestStartRequiresACommand(t *testing.T) {
	m, _ := newMgr(t, 0)
	if _, err := m.Start(Spec{}); err == nil {
		t.Error("an empty command was accepted")
	}
	if _, err := m.Get("nope"); !errors.As(err, new(*UnknownError)) {
		t.Errorf("unknown id = %v", err)
	}
}

func TestJobEnvLayersOverTheConfiguredEnvironment(t *testing.T) {
	// A job that sets one variable must not drop what the config's env block
	// established: two `python` invocations resolving differently only because
	// one was backgrounded is the kind of bug that reads as the harness lying.
	dir := t.TempDir()
	st, err := session.Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, err := st.Create(session.Row{ID: uuid.NewV7(), Workdir: dir, Project: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		Session: sess,
		Local:   &env.Local{Env: map[string]string{"FROM_CONFIG": "config", "OVERRIDDEN": "config"}},
		Env:     map[string]string{"FROM_MANAGER": "manager", "OVERRIDDEN": "manager"},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	t.Cleanup(func() { m.KillAll() })

	r, err := m.Start(Spec{Command: `echo "$FROM_CONFIG|$FROM_MANAGER|$OVERRIDDEN|$FROM_JOB"`, Env: []string{"FROM_JOB=job", "OVERRIDDEN=job"}})
	if err != nil {
		t.Fatal(err)
	}
	row := finish(t, r)
	data, err := os.ReadFile(row.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "config|manager|job|job" {
		t.Errorf("env layering = %q, want config|manager|job|job", got)
	}
	// And the shared foreground Local still holds exactly what it started with:
	// the job layered on a copy, so the next foreground call sees mini's config
	// and not this run's overrides.
	if m.Local.Env["OVERRIDDEN"] != "config" || m.Local.Env["FROM_CONFIG"] != "config" || len(m.Local.Env) != 2 {
		t.Errorf("the foreground Local was mutated: %v", m.Local.Env)
	}
}

// stubKind is a second implementation of stopper, which is the point: a run
// that is not a process must be registrable, killable, and reapable by the same
// code, or decision 1 in docs/jobs-research.md §4 is aspirational.
type stubKind struct {
	run     *Run
	unwind  time.Duration
	hurried int32
}

func (s *stubKind) hurry() {
	if atomic.AddInt32(&s.hurried, 1) == 1 {
		// The end is requested, not delivered: the run unwinds on its own
		// clock, which is exactly what makes a serial reap slow.
		time.AfterFunc(s.unwind, func() { close(s.run.done) })
	}
}

func (s *stubKind) stop(grace time.Duration) error {
	s.hurry()
	select {
	case <-s.run.done:
		return nil
	case <-time.After(grace + unwindAllowance):
		return ErrStillRunning
	}
}

func TestReapTearsEveryJobDownTogether(t *testing.T) {
	m, sess := newMgr(t, 0)
	const jobs = 4
	const unwind = 400 * time.Millisecond
	for i := 1; i <= jobs; i++ {
		id := "s" + strconv.Itoa(i)
		if _, err := sess.CreateJob(session.JobRow{ID: id, Kind: session.KindSubagent, Command: "stub"}); err != nil {
			t.Fatal(err)
		}
		r := &Run{id: id, kind: session.KindSubagent, output: sess.JobPath(id),
			started: time.Now().UnixMilli(), mgr: m, done: make(chan struct{})}
		if err := m.register(r); err != nil {
			t.Fatal(err)
		}
		r.stop = &stubKind{run: r, unwind: unwind}
	}

	start := time.Now()
	if n := m.KillAll(); n != jobs {
		t.Fatalf("KillAll = %d, want %d", n, jobs)
	}
	d := time.Since(start)
	// Asking all four at once costs one unwind; ending them one at a time costs
	// four. The exit path of the app is waiting on this, and so is a human who
	// pressed q while a soak test ran.
	if d > 2*unwind {
		t.Errorf("reap took %v for %d jobs, each unwinding in %v: it is serial", d, jobs, unwind)
	}
	// Every handle is gone from the live set once its run ends, so the cap
	// releases. Polling rather than asserting inline: dropping the handle is a
	// watcher of done, and that goroutine is allowed to lose the race here.
	for i := 1; i <= jobs; i++ {
		id := "s" + strconv.Itoa(i)
		for attempt := 0; attempt < 100; attempt++ {
			if _, err := m.Get(id); err != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
			if attempt == 99 {
				t.Errorf("stub %d still in the live set after its run ended", i)
			}
		}
	}
	// A stub has no process to signal, and Hurry must not invent one: a kind
	// that cannot be killed reports that instead of pretending it was.
	orphan := &Run{id: "orphan", kind: session.KindSubagent, mgr: m, done: make(chan struct{})}
	if err := orphan.Kill(); err == nil {
		t.Error("a run with no stopper reported success")
	}
	orphan.Hurry() // must not panic on the nil stopper
}

func TestRefusedStartLeaksNoGoroutine(t *testing.T) {
	// register's watcher waits on done for every adopted handle, so a refusal
	// before the spawn has to close it too. This counts the goroutines, because
	// the leak is invisible otherwise and grows with every cap refusal.
	m, _ := newMgr(t, 1)
	r, err := m.Start(Spec{Command: "sleep " + markerFor(t), TCNO: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Kill()

	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		// The DB cap has not been reached (one running job, MaxJobs 1 does
		// reach it), so every one of these refusals comes from the live set.
		if _, err := m.Start(Spec{Command: "echo nope", TCNO: 1}); err == nil {
			t.Fatal("duplicate tcno was accepted while live")
		}
	}
	runtime.Gosched()
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Errorf("goroutines went %d -> %d across 50 refused starts", before, after)
	}
}

// A single bash command can hold several external processes at the same time -
// pipelines, `&`, make -j, a loop that forks. They are not one process group
// either: the interpreter gives each external command its own session, because
// that is what lets a foreground call's timeout take down the tree it started.
// So there is nothing a pattern-based kill could aim at that names "the job",
// and this is why ending a run has to be the harness cancelling the context
// every one of those commands was started under.
func TestKillEndsEveryConcurrentExternalOfARun(t *testing.T) {
	m, _ := newMgr(t, 0)
	marker := markerFor(t)
	const leaves = 4
	script := ""
	for i := 0; i < leaves; i++ {
		script += "sleep " + marker
		if i < leaves-1 {
			script += " & "
		}
	}
	r, err := m.Start(Spec{Command: script, Label: "many children"})
	if err != nil {
		t.Fatal(err)
	}
	markerPIDs(t, "sleep "+marker, leaves)
	if got := len(pgrep(t, "sleep "+marker)); got < leaves {
		t.Fatalf("expected %d concurrent externals, saw %d", leaves, got)
	}

	if err := r.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// Every leaf dies, including the backgrounded ones - which is the whole
	// claim. A kill that reached only the command still attached to the
	// interpreter would leak the GPU memory the design exists to protect.
	for i := 0; i < 100; i++ {
		left := pgrep(t, "sleep "+marker)
		if len(left) == 0 {
			break
		}
		if i == 99 {
			t.Fatalf("%d of %d children survived the kill: %v", len(left), leaves, left)
		}
		time.Sleep(50 * time.Millisecond)
	}
	row := finish(t, r)
	if row.State != session.JobKilled {
		t.Errorf("state = %s, want killed", row.State)
	}
}
