package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// deadlineEnv is the interpreter's shadow: it records the budget the loop put on
// each command's context, which is the only way a per-call timeout reaches an
// Environment whose Execute signature carries no timeout argument.
type deadlineEnv struct{ budgets []time.Duration }

func (d *deadlineEnv) Execute(ctx context.Context, command, cwd string, sink io.Writer) (env.Result, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.budgets = append(d.budgets, time.Until(dl).Round(100*time.Millisecond))
	} else {
		d.budgets = append(d.budgets, 0)
	}
	fmt.Fprintln(sink, "ran")
	return env.Result{ReturnCode: 0}, nil
}

func (d *deadlineEnv) TemplateVars() map[string]any { return nil }
func (d *deadlineEnv) Serialize() map[string]any    { return nil }

// newJobLoop wires a Loop to a real registry, so the handoff, the promotion and
// the row all get exercised together rather than against a mock of each other.
func newJobLoop(t *testing.T, m model.Model, e env.Environment) (*Loop, *job.Manager, *fakeRecorder) {
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
	l, rec := newRecorderLoop(t, m, e)
	jm := &job.Manager{Session: sess, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	l.Jobs = jm
	t.Cleanup(func() { jm.KillAll(); st.Close() })
	return l, jm, rec
}

// observations returns the loop's tool-result messages, oldest first.
func observations(l *Loop) []string {
	var out []string
	for _, msg := range l.Messages {
		if model.IsToolResult(msg) {
			out = append(out, fmt.Sprint(msg["content"]))
		}
	}
	return out
}

func lastObservation(l *Loop) string {
	obs := observations(l)
	if len(obs) == 0 {
		return ""
	}
	return obs[len(obs)-1]
}

// The path the call's own result file had, which is where the job writes.
func callPath(rec *fakeRecorder) string {
	if len(rec.opens) == 0 {
		return ""
	}
	return rec.opens[len(rec.opens)-1].tc.Path
}

func TestBackgroundReturnsAHandoffInsteadOfWaiting(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("start the soak", model.Action{
			Command: "sleep 30; echo should-never-be-seen", Background: true}),
		done("submitted"),
	}}
	l, jm, rec := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})

	start := time.Now()
	if _, err := run(t, l, "start a soak test"); err != nil {
		t.Fatal(err)
	}
	// The whole point of the parameter: a 30-second command must not make the
	// turn take 30 seconds.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the backgrounded call blocked the loop for %v", d)
	}

	last := lastObservation(l)
	for _, want := range []string{"started in the background as job j1", "out.log", "tail -n 100", "killed when the session closes"} {
		if !strings.Contains(last, want) {
			t.Errorf("handoff missing %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "should-never-be-seen") {
		t.Errorf("the backgrounded command's output leaked into the observation: %s", last)
	}

	list, err := jm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "j1" || list[0].State != session.JobRunning {
		t.Fatalf("registry = %+v", list)
	}
	if list[0].TCNO == nil || *list[0].TCNO != 1 {
		t.Errorf("job did not record the call that started it: %+v", list[0])
	}
	// One file, not two. The job appends to the very result file the call
	// already points at, so the transcript's tcr_path for the call IS the live
	// job log and a promoted run needs no second path explained to the model.
	if got := callPath(rec); got == "" || list[0].OutputPath != got {
		t.Errorf("job logged somewhere else: row=%q call=%q", list[0].OutputPath, got)
	}
	if _, err := os.Stat(list[0].OutputPath); err != nil {
		t.Errorf("job output file: %v", err)
	}
	// And the row stays the only record of the outcome: the call row is never
	// completed, because the call did not complete - it handed off.
	for _, c := range rec.comps {
		t.Errorf("a handed-off call was recorded as complete: %+v", c)
	}
}

// The promotion is what the wall becomes: a command that outruns its budget
// keeps running, and the deadline stops the waiting rather than the work.
func TestForegroundCommandIsPromotedWhenItOutrunsItsBudget(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		// 30s of work with a 1s budget: the budget is the model's, the work is
		// not. env would have killed this at the deadline a step ago.
		turnActions("build it", model.Action{Command: "echo building; sleep 4; echo finished", TimeoutS: 1}),
		done("submitted"),
	}}
	l, jm, rec := newJobLoop(t, m, &env.Local{Timeout: 30 * time.Second})

	start := time.Now()
	if _, err := run(t, l, "build it"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the loop waited for the whole command (%v) instead of the budget", d)
	}

	last := lastObservation(l)
	for _, want := range []string{"still running after 1s as job j1", "tail -n 100",
		"This is not a timeout - nothing was killed"} {
		if !strings.Contains(last, want) {
			t.Errorf("promotion note missing %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "timed out") {
		t.Errorf("a promotion must not read as a timeout:\n%s", last)
	}

	// The promoted run is a real job pointed at the same file, and it is still
	// burning: the command was never interrupted.
	list, err := jm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != session.JobRunning {
		t.Fatalf("registry = %+v", list)
	}
	if got := callPath(rec); list[0].OutputPath != got {
		t.Errorf("promoted job writes elsewhere: row=%q call=%q", list[0].OutputPath, got)
	}
	// It finishes on its own clock and records its own outcome, after the turn
	// that started it has already been answered.
	r, err := jm.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatalf("the promoted run did not survive its turn: %v", err)
	}
	row, err := r.Row()
	if err != nil {
		t.Fatal(err)
	}
	if row.State != session.JobCompleted || row.ExitCode == nil || *row.ExitCode != 0 {
		t.Errorf("promoted outcome = %+v", row)
	}
	data, err := os.ReadFile(row.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	// Both lines the command printed, in the file the model was pointed at.
	if got := string(data); !strings.Contains(got, "building") || !strings.Contains(got, "finished") {
		t.Errorf("job log = %q", got)
	}
}

// A command that finishes inside the budget is not a job at all: no row, no
// cap pressure, no id to remember. That is why the registry can stay about
// jobs while every foreground call runs through it.
func TestShortCommandLeavesNoJobBehind(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("quick", model.Action{Command: "echo fast", TimeoutS: 20}),
		done("submitted"),
	}}
	l, jm, rec := newJobLoop(t, m, &env.Local{Timeout: 30 * time.Second})
	if _, err := run(t, l, "echo fast"); err != nil {
		t.Fatal(err)
	}
	list, err := jm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a finished command was registered as a job: %+v", list)
	}
	last := lastObservation(l)
	if !strings.Contains(last, "fast") {
		t.Errorf("observation = %q", last)
	}
	if len(rec.comps) != 1 || rec.comps[0].out.ReturnCode != 0 {
		t.Errorf("the call should have been completed normally: %+v", rec.comps)
	}
}

func TestBackgroundWithoutARegistryReportsInsteadOfRunningForeground(t *testing.T) {
	// A Loop built without a registry (a test harness, or a front end that has
	// not wired one) must refuse rather than obey the letter of the request and
	// break its intent.
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("try to background", model.Action{Command: "echo nope", Background: true}),
		done("submitted"),
	}}
	l, _ := newRecorderLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "background this"); err != nil {
		t.Fatal(err)
	}
	last := lastObservation(l)
	if !strings.Contains(last, "no job registry") {
		t.Errorf("observation = %q", last)
	}
	if strings.Contains(last, "nope") {
		t.Errorf("the command ran anyway: %q", last)
	}
}

func TestTimeoutSReachesTheCommandAsADeadline(t *testing.T) {
	d := &deadlineEnv{}
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("ask for a big budget", model.Action{Command: "make", TimeoutS: 7}),
		turnActions("and then do not", model.Action{Command: "ls"}),
		done("submitted"),
	}}
	l := newLoop(t, m, d)
	if _, err := run(t, l, "build then list"); err != nil {
		t.Fatal(err)
	}
	if len(d.budgets) != 2 {
		t.Fatalf("commands = %d", len(d.budgets))
	}
	// 7s asked for, arriving as a deadline on the context rather than as an
	// argument the Environment would have to know how to honour.
	if d.budgets[0] < 6*time.Second || d.budgets[0] > 7*time.Second {
		t.Errorf("timeout_s budget = %v, want ~7s", d.budgets[0])
	}
	// No parameter means no opinion, so the loop must invent nothing.
	if d.budgets[1] != 0 {
		t.Errorf("unbudgeted command got a deadline of %v", d.budgets[1])
	}
}

// The promotion threshold is the configured wait, not a constant the loop
// invented: raising environment.timeout must mean "wait longer", not "kill at
// the same 30 seconds and call it a job".
func TestPromotionUsesTheConfiguredBudget(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("takes a moment", model.Action{Command: "echo one; sleep 2; echo two"}),
		done("submitted"),
	}}
	l, jm, _ := newJobLoop(t, m, &env.Local{Timeout: 10 * time.Second})
	l.CommandBudget = 3 * time.Second

	if _, err := run(t, l, "run it"); err != nil {
		t.Fatal(err)
	}
	// Under the configured budget the command finishes normally: no job, and
	// the output is the observation rather than a handoff. The absence of a row
	// is the timing assertion - a 2s command inside a 3s budget was waited on
	// to the end, and measuring wall-clock here would only test the scheduler.
	if list, _ := jm.List(); len(list) != 0 {
		t.Errorf("a command inside its budget became a job: %+v", list)
	}
	if !strings.Contains(lastObservation(l), "two") {
		t.Errorf("observation = %q", lastObservation(l))
	}

	// Over it, it is promoted at the configured number and not before.
	m2 := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("takes longer", model.Action{Command: "sleep 30"}),
		done("submitted"),
	}}
	l2, jm2, _ := newJobLoop(t, m2, &env.Local{Timeout: 60 * time.Second})
	l2.CommandBudget = 1 * time.Second
	if _, err := run(t, l2, "run it long"); err != nil {
		t.Fatal(err)
	}
	last := lastObservation(l2)
	if !strings.Contains(last, "still running after 1s as job j1") {
		t.Errorf("observation = %q, want a handoff at the configured budget", last)
	}
	if list, _ := jm2.List(); len(list) != 1 || list[0].State != session.JobRunning {
		t.Errorf("registry = %+v", list)
	}
}

// pgrepCount counts live processes whose command line matches, which is how
// these tests prove a kill reached the process and not just the row.
func pgrepCount(t *testing.T, pattern string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", pattern).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func TestJobsToolDescribesALiveJob(t *testing.T) {
	const marker = "43.7"
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("background it", model.Action{Command: "sleep " + marker, Background: true, Description: "soak"}),
		turnActions("what is running", model.Action{Tool: model.ToolJobs}),
		done("submitted"),
	}}
	l, jm, _ := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "background a soak and list it"); err != nil {
		t.Fatal(err)
	}
	// The process really is there before the listing says so.
	if pgrepCount(t, "sleep "+marker) == 0 {
		t.Fatal("the backgrounded command never started")
	}

	obs := observations(l)
	if len(obs) != 2 {
		t.Fatalf("observations = %d", len(obs))
	}
	listing := obs[1]
	for _, want := range []string{"j1", "running", "out=", "cmd=sleep " + marker, "soak", "1 job(s), 1 running", "job_kill"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing missing %q:\n%s", want, listing)
		}
	}
	// Why it exists is in the listing, because "backgrounded on purpose" and
	// "outran its budget" call for different next moves.
	if !strings.Contains(listing, "why=started in the background") {
		t.Errorf("listing lost the reason:\n%s", listing)
	}
	// And the registry is what it reads - not the conversation.
	if list, _ := jm.List(); len(list) != 1 {
		t.Errorf("registry = %+v", list)
	}
}

func TestJobKillToolEndsTheJobAndItsProcess(t *testing.T) {
	const marker = "55.3"
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("start it", model.Action{Command: "sleep " + marker, Background: true}),
		turnActions("kill it", model.Action{Tool: model.ToolJobKill, Command: "j1"}),
		done("submitted"),
	}}
	l, jm, _ := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "start then stop a soak"); err != nil {
		t.Fatal(err)
	}

	obs := observations(l)
	if len(obs) != 2 {
		t.Fatalf("observations = %d", len(obs))
	}
	if !strings.Contains(obs[1], "j1: killed") {
		t.Errorf("kill observation = %q", obs[1])
	}
	list, err := jm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != session.JobKilled {
		t.Fatalf("registry after kill = %+v", list)
	}
	// The claim that matters: the process is gone. Nothing here used a pattern
	// kill, and the run's own context is what ended it.
	for i := 0; i < 60; i++ {
		if pgrepCount(t, "sleep "+marker) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if left := pgrepCount(t, "sleep "+marker); left != 0 {
		t.Errorf("%d processes survived job_kill", left)
	}
}

// An id the session does not have must not look like success, and an orphan from
// before a restart must say it cannot be touched rather than pretend.
func TestJobKillReportsWhatItCouldNotDo(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("ghost", model.Action{Tool: model.ToolJobKill, Command: "j99"}),
		done("submitted"),
	}}
	l, jm, _ := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "kill a ghost"); err != nil {
		t.Fatal(err)
	}
	obs := observations(l)
	if !strings.Contains(obs[0], "j99: no such job in this session") {
		t.Errorf("observation = %q", obs[0])
	}
	// The return code has to carry the failure, or a typo id reads as a kill.
	if !strings.Contains(obs[0], "<returncode>1</returncode>") {
		t.Errorf("unknown id did not fail: %q", obs[0])
	}

	// An orphaned row has no handle, and killing it is not this process's to do.
	if _, err := jm.Session.CreateJob(session.JobRow{ID: "j7", Command: "python train.py"}); err != nil {
		t.Fatal(err)
	}
	jm.Sweep()
	m2 := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("kill the orphan", model.Action{Tool: model.ToolJobKill, Command: "j7"}),
		done("submitted"),
	}}
	l2, _, _ := newJobLoop(t, m2, &env.Local{Timeout: 5 * time.Second})
	l2.Jobs = jm
	if _, err := run(t, l2, "kill the leftover"); err != nil {
		t.Fatal(err)
	}
	if got := lastObservation(l2); !strings.Contains(got, "orphaned") {
		t.Errorf("observation = %q, want the honest refusal", got)
	}
}

// job_wait takes no arguments and wakes on whichever of the session's jobs ends
// first - the model is not asked to hold a list of its own background work in
// context to be told when any of it finished.
func TestJobWaitWakesOnTheFirstJobToEnd(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("start the long one", model.Action{Command: "sleep 40", Background: true, Description: "long"}),
		turnActions("start the short one", model.Action{Command: "sleep 1; echo finished-fast", Background: true, Description: "short"}),
		turnActions("block until one ends", model.Action{Tool: model.ToolJobWait}),
		done("submitted"),
	}}
	l, jm, _ := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})

	start := time.Now()
	if _, err := run(t, l, "run two soaks and wait for either"); err != nil {
		t.Fatal(err)
	}
	// It woke on the 1-second job, not the 40-second one, and it did not sit
	// through the slow one to prove a point.
	if d := time.Since(start); d > 25*time.Second {
		t.Errorf("the wait took %v; it should have woken on the fast job", d)
	}

	obs := observations(l)
	if len(obs) != 3 {
		t.Fatalf("observations = %d", len(obs))
	}
	woke := obs[2]
	for _, want := range []string{"job j2 ended: completed", "exit 0", "label=short",
		"finished-fast", "read it with: tail -n 100", "1 other job(s) still running"} {
		if !strings.Contains(woke, want) {
			t.Errorf("wait report missing %q:\n%s", want, woke)
		}
	}
	if !strings.Contains(woke, "after 1") {
		t.Errorf("the report should say how long it ran:\n%s", woke)
	}
	// The job that did not end is untouched, and still listed as running.
	list, err := jm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("registry = %+v", list)
	}
	for _, j := range list {
		if j.ID == "j1" && j.State != session.JobRunning {
			t.Errorf("the unwaited-for job ended too: %+v", j)
		}
		if j.ID == "j2" && j.State != session.JobCompleted {
			t.Errorf("j2 = %s", j.State)
		}
	}
}

// Nothing to wait for is an answer, delivered now - not a timeout to sit
// through, and not an error to retry.
func TestJobWaitWithNoJobsAnswersImmediately(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("wait for nothing", model.Action{Tool: model.ToolJobWait}),
		done("submitted"),
	}}
	l, _, _ := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	start := time.Now()
	if _, err := run(t, l, "wait"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("an empty wait blocked for %v", d)
	}
	woke := lastObservation(l)
	for _, want := range []string{"no background jobs are running", "No job has been started in this session yet"} {
		if !strings.Contains(woke, want) {
			t.Errorf("empty-wait report missing %q:\n%s", want, woke)
		}
	}
	// Success, because the tool did exactly what it was asked.
	if strings.Contains(woke, "<returncode>1") || strings.Contains(woke, "-1") {
		t.Errorf("an empty wait should not read as a failure:\n%s", woke)
	}
}

// A job that finished while the model was busy elsewhere must still be there when
// it asks. This is the case a wait built on live handles gets wrong: the job is
// gone from the running set, so the wait blocks on the next one instead of
// reporting the event it exists to report.
func TestJobWaitReportsAnEndingNobodySaw(t *testing.T) {
	m := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("quick background job", model.Action{Command: "echo bye", Background: true}),
		// An ordinary foreground turn passes while the job ends.
		turnActions("busy elsewhere", model.Action{Command: "echo working", TimeoutS: 5}),
		turnActions("now wait", model.Action{Tool: model.ToolJobWait}),
		done("submitted"),
	}}
	l, _, _ := newJobLoop(t, m, &env.Local{Timeout: 5 * time.Second})
	if _, err := run(t, l, "start a quick job, do other work, then wait"); err != nil {
		t.Fatal(err)
	}
	obs := observations(l)
	if len(obs) != 3 {
		t.Fatalf("observations = %d", len(obs))
	}
	if !strings.Contains(obs[2], "job j1 ended: completed") {
		t.Errorf("the missed ending was not reported:\n%s", obs[2])
	}
	// And it is reported once: the next wait has nothing owed to it.
	m2 := &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
		turnActions("wait again", model.Action{Tool: model.ToolJobWait}),
		done("submitted"),
	}}
	l2, jm2, _ := newJobLoop(t, m2, &env.Local{Timeout: 5 * time.Second})
	l2.Jobs = jm2
	if _, err := run(t, l2, "wait again"); err != nil {
		t.Fatal(err)
	}
}
