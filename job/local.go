package job

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"uuid"

	"strings"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/session"
)

// Spec describes a run before it exists. Everything in it is kind-neutral
// except Command's meaning, which only the bash kind interprets as a shell
// script; a subagent kind would put its task description in the same field.
type Spec struct {
	// Command is the script to run. Required.
	Command string
	// Label is the model's short name for the run, and the thing a wake quotes
	// back. It is what turns "job j12 exited 1" into something actionable.
	Label       string
	Description string
	// Dir is the working directory; empty is the session's workdir.
	Dir string
	// Env is extra KEY=VALUE pairs for this run only.
	Env []string
	// Kind is session.KindBash (the default) or session.KindSubagent. It is
	// display metadata and the cap-exemption key, never a control path - the
	// control path is Run below.
	Kind string
	// Run is the work for a kind the interpreter cannot perform: an agent job
	// runs a session, not a shell. When it is set, Launch executes it on the
	// same context, the same output file and the same state machine as a bash
	// run, and nothing else in the package branches on the kind - which is what
	// the package doc means by "a job is a run and not a tool call", and why the
	// registry, the wake path and the wait/kill API never learned that a job has
	// a pid (job.go:5-11). The run owns its own ending: it returns when the work
	// is over, and the manager only records what it says.
	Run func(ctx context.Context, out io.Writer) Result
	// TCNO is the tool call that started the run, when there was one. It is a
	// display join, never a lookup path; the reason ids are derived from it is
	// that it is already unique per session.
	TCNO int
	// ParentID links this run to the job that spawned it, for trees and for
	// reaping a parent's children.
	ParentID string
	// File and Path redirect the run's output into a writer the caller owns -
	// a foreground call's result file, which a promoted run must keep writing
	// to after the tool call has been answered. The watcher closes it, so a
	// caller that hands one over must not close it itself.
	File *os.File
	Path string
}

// kindOrDefault is the spec's kind, or bash when unset - so every existing
// call site keeps producing the rows it always did.
func (s Spec) kindOrDefault() string {
	if s.Kind != "" {
		return s.Kind
	}
	return session.KindBash
}

// Result is what a custom Run reports when it ends. ExitCode is a pointer
// because "no code" and "code 0" are different statements: a run that ended
// because it was cancelled has no exit code, and claiming 0 would tell the
// parent that its own kill was a success.
type Result struct {
	ExitCode *int
	Reason   string
}

// outcomeFor turns a custom run's report into the internal shape the bash path
// produces, so recordEndLocked and the firing have one thing to consume.
func outcomeFor(res Result, err error) outcome {
	if err != nil {
		return outcome{state: session.JobKilled, reason: "killed on request: " + err.Error()}
	}
	if res.Reason != "" && res.ExitCode == nil {
		return outcome{state: session.JobFailed, reason: res.Reason}
	}
	if res.ExitCode != nil && *res.ExitCode != 0 {
		return outcome{state: session.JobFailed, code: res.ExitCode, reason: res.Reason}
	}
	return outcome{state: session.JobCompleted, code: res.ExitCode}
}

// ErrStillRunning is what a kill reports when the cancel landed but the run has
// not unwound yet. It is not a failure of the kill: the process was given
// SIGINT and then SIGKILL, and a job that ignores both is a story for the log,
// not a reason to tell the model its own command was refused.
var ErrStillRunning = errors.New("job: run has not exited after the kill")

// Start registers and launches one bash run.
//
// The script is interpreted by the same mvdan.cc/sh runner that answers every
// foreground `bash` call - env.Local, driven on a context this manager owns
// instead of the tool call's. That is the whole trick: the reason a background
// command must not be a different shell is that the model has no way to know it
// changed, and a `for` loop, a `[[ ]]`, or an unset variable behaving
// differently only in the background reads as the harness lying. Nor is it
// `cmd &` inside a foreground call, which cannot work: interp runs background
// statements in-process on the call's own context
// (interp/runner.go:307-328), so the call's deadline reaches them and the
// 30-second wall takes the job down with the shell.
//
// Lifetime therefore belongs to the manager, and the kill path is context
// cancellation, which env's process-group handler already turns into SIGINT →
// KillGrace → SIGKILL against the session the child was started in
// (env/exec_unix.go:46, :50-58). Every external command a job runs is already
// its own process-group leader, so cancelling reaches the whole subtree that
// actually holds the GPU without mini having to track a shell pid that owns it.
func (m *Manager) Start(s Spec) (*Run, error) {
	r, err := m.Launch(s)
	if err != nil {
		return nil, err
	}
	if _, err := m.Detach(r, "started in the background"); err != nil {
		// The run started seconds ago and has done no work yet, and a job with
		// no row cannot be listed, woken, or stopped by anyone - so this is the
		// one place where ending what we started is the honest answer. Contrast
		// Loop.handoff, which promotes a run that has already burned its whole
		// budget: there, a bookkeeping failure must never destroy the work.
		_ = r.Kill()
		return nil, err
	}
	return r, nil
}

// Launch begins a run without registering it as a job. This is the foreground
// path: a command that finishes inside its budget was never a job, and the
// registry should not have to forget ten thousand rows a day that only ever
// said "ls". The run lives in the live set while it burns, so it can be waited
// on and cancelled, and Detach turns it into a job if it outlives the waiting.
//
// The reason a run must be launched this way rather than started by the caller
// and "promoted" afterwards is that the deadline is what kills it: once
// env.Local has signalled the process group there is nothing left to hand over.
// Promotion only works if the wait and the run are separate things from the
// start, which is also crush's shape (bash.go:386-451 starts under a detached
// context so it can survive being moved to background).
func (m *Manager) Launch(s Spec) (*Run, error) {
	if s.Command == "" {
		return nil, errors.New("job: a command is required")
	}
	id := JobID(s.TCNO)
	if !session.JobIDPattern.MatchString(id) {
		return nil, fmt.Errorf("job: id %q is not path-safe", id)
	}

	// Adopt the handle before anything is written: the duplicate check costs
	// nothing when it fires first, and a refusal must not leave a file or a row
	// describing a run that never existed.
	r := &Run{id: id, kind: s.kindOrDefault(), output: m.Session.JobPath(id),
		dir: m.dir(s.Dir), started: time.Now().UnixMilli(), mgr: m, done: make(chan struct{})}
	if s.Path != "" {
		r.output = s.Path
	}
	if err := m.register(r); err != nil {
		return nil, err
	}

	file := s.File
	if file == nil {
		// Append, never truncate: docs/jobs-research.md §4's one invariant is
		// that a file a transcript row points at is only ever written at EOF,
		// so every stored tcr_offset keeps describing the bytes it was computed
		// against.
		if err := os.MkdirAll(filepath.Dir(r.output), 0o700); err != nil {
			return nil, r.abandon(fmt.Errorf("job: create result directory: %w", err))
		}
		f, err := os.OpenFile(r.output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, r.abandon(fmt.Errorf("job: open %s: %w", r.output, err))
		}
		file = f
	}

	// Malformed pairs are dropped rather than guessed at: a job that starts with
	// nine of its ten variables is worth more than a job that refuses to start.
	r.file, r.spec = file, s
	envs := make(map[string]string, len(s.Env))
	for _, kv := range s.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envs[k] = v
		}
	}
	ctx, cancel := context.WithCancel(m.background())
	if s.Run != nil {
		// A run that is not a process. Same handle, same done channel, same
		// watcher contract - and the stopper is cancellation, with no
		// SIGINT-to-SIGKILL window to wait through, because there is nothing to
		// signal. That is the package doc's "a kind whose ending is not a process
		// exit simply has none", made real.
		agent := &funcRun{run: r, cancel: cancel, fn: s.Run}
		r.stop = agent
		go agent.watch(ctx, file, s)
		return r, nil
	}
	run := &interpRun{run: r, cancel: cancel}
	r.stop = run
	go run.watch(ctx, file, s, envs)
	return r, nil
}

// funcRun is a non-process kind's hold on a run: it ends by cancelling the
// context it was given, and trusts the work to notice.
type funcRun struct {
	run    *Run
	cancel context.CancelFunc
	fn     func(context.Context, io.Writer) Result
}

// hurry requests the end and returns at once, mirroring interpRun: a session
// with several live agent jobs is torn down in the time one takes, not per job
// (the reap is on every exit path of the app, §9.2's batched unwind).
func (f *funcRun) hurry() { f.cancel() }

func (f *funcRun) stop(grace time.Duration) error {
	select {
	case <-f.run.done:
		return nil
	default:
	}
	f.cancel()
	select {
	case <-f.run.done:
		return nil
	case <-time.After(grace):
		return ErrStillRunning
	}
}

// watch mirrors interpRun.watch, including the deferral order: close(done)
// happens last, so a caller woken by it is entitled to a recorded row and a
// closed file.
func (f *funcRun) watch(ctx context.Context, file *os.File, s Spec) {
	r, m := f.run, f.run.mgr
	defer close(r.done)
	defer file.Close()

	res := f.fn(ctx, file)
	oc := outcomeFor(res, ctx.Err())

	r.mu.Lock()
	r.end = &oc
	detached := r.detached
	var row *session.JobRow
	if detached {
		row = m.recordEndLocked(r, oc)
	}
	r.mu.Unlock()
	if detached {
		logEnd(m, row, r.id, oc)
		m.noteQuiescent()
	}
}

// Detach registers a running launch as a job: from here it has a row, survives
// the turn, and can be listed and killed by id. Doing this lazily is what keeps
// the registry about jobs - a command that finished in 40ms was never one - and
// it is why a promoted run's row stores the path it has been writing to all
// along instead of inventing a second file the model would have to be told about.
//
// There is no cap here and there is deliberately no knob. mini puts no limit on
// how many tool calls may be in flight or in total: a backgrounded job is a
// shell, so it is a process *tree*, and counting trees bounds nothing that
// matters - `make -j64` and a fork loop are each exactly one job. Concurrency
// belongs to the operating system (ulimit, cgroups), where the process tree is
// actually visible, and the one real shared resource in this architecture is the
// model server, which is already bounded where it is contended - see
// model.Gate. What remains is refusal only where a job cannot be tracked at all.
func (m *Manager) Detach(r *Run, reason string) (*session.JobRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.detached {
		// Already a job: a second handoff must not invent a second row, and the
		// caller's answer is the row that exists.
		return m.Session.GetJob(r.id)
	}
	row, err := m.Session.CreateJob(session.JobRow{
		ID: r.id, Kind: r.kind, ParentID: r.spec.ParentID, TCNO: intPtr(r.spec.TCNO),
		Label: r.spec.Label, Command: r.spec.Command, Description: r.spec.Description,
		OutputPath: r.output, StartedAt: r.started,
	})
	if err != nil {
		return nil, err
	}
	r.detached, r.reason = true, reason
	// The run may already have ended while this call was in flight, which is
	// the race r.mu exists to close: settle the row in the same critical
	// section that marks it a job.
	if r.end != nil {
		m.recordEndLocked(r, *r.end)
	}
	return row, nil
}

// interpRun is the bash kind's hold on a run. It is the only thing in the
// package that knows what a run is made of, and it does not know the pid:
// ending a job is cancelling its context, and env's exec handler is what
// signals the processes that were started under it.
type interpRun struct {
	run    *Run
	cancel context.CancelFunc
}

func (i *interpRun) stop(grace time.Duration) error {
	select {
	case <-i.run.done:
		// Already finished: the kill is a no-op, not a failure. The model may
		// have pressed it twice, and a job that is already gone must not look
		// like an error.
		return nil
	default:
	}
	i.cancel()
	// The cancel is the request; env's handler needs its own SIGINT-to-SIGKILL
	// window before the in-flight command dies, so bound the wait past that
	// rather than pretending the signal is instant.
	select {
	case <-i.run.done:
		return nil
	case <-time.After(grace + unwindAllowance):
		return fmt.Errorf("%w: %s (state %s)", ErrStillRunning, i.run.id, i.run.State())
	}
}

// hurry is the reap's non-blocking ask: same cancel, no wait for the unwind.
func (i *interpRun) hurry() { i.cancel() }

// watch runs the command to its end and turns that ending into a state. It is
// the only writer of terminal states for a live run, which is what lets
// FinishJob refuse to overwrite one that has already been decided.
func (i *interpRun) watch(ctx context.Context, file *os.File, s Spec, envs map[string]string) {
	r, m := i.run, i.run.mgr
	// Deferred in this order so close(done) runs LAST: a caller woken by it is
	// entitled to read a recorded row and a closed file, and the state must not
	// be claimable while bytes may still arrive. Dropping the live handle is
	// register's goroutine's job now, not this one's.
	defer close(r.done)
	defer file.Close()

	local := m.local()
	local.CWD = r.dir
	if len(envs) > 0 {
		local.Env = m.mergeEnv(envs)
	}
	// local is a copy, so the shared foreground environment is never touched:
	// a job that overrides PATH must not change what the next foreground call
	// sees.

	res, err := local.Execute(ctx, s.Command, r.dir, file)
	oc := classify(res, err)

	// The row and the outcome have to agree, and either can arrive first: the
	// watcher only writes when the run is detached, and Detach writes when the
	// run has already ended. mu covers both, so a command that finishes in the
	// same instant its budget expires cannot leave a row claiming it is running.
	r.mu.Lock()
	r.result, r.end = res, &oc
	detached := r.detached
	var row *session.JobRow
	if detached {
		row = m.recordEndLocked(r, oc)
	}
	r.mu.Unlock()
	if detached {
		logEnd(m, row, r.id, oc)
		m.noteQuiescent()
	}
}

// classify turns what the interpreter reported into a state, and is the only
// place that decision lives. It takes the error separately because
// env.Local.Execute reserves the Go error for one thing - the caller's context
// ending - which is precisely the shape of a kill.
func classify(res env.Result, err error) outcome {
	if err != nil {
		// No exit code is claimed: reporting 0 for a run we interrupted would
		// say it succeeded, and -1 is already the timeout shape.
		return outcome{state: session.JobKilled, reason: "killed on request: " + err.Error()}
	}
	switch exceptionType(res) {
	case "TimeoutExpired":
		// Unreachable for a run of its own, which has no deadline: it means a
		// promotion handed a foreground timer to something that outlived it.
		return outcome{state: session.JobFailed, reason: "timed out after the deadline it was given"}
	case "ParserError", "Panic":
		// A parse error or a panic in the runner: the command never ran to
		// completion, and ExceptionInfo says why in prose the model can act on.
		return outcome{state: session.JobFailed, reason: res.ExceptionInfo}
	}
	code := res.ReturnCode
	if code != 0 {
		return outcome{state: session.JobFailed, code: &code,
			reason: fmt.Sprintf("exited %d", code)}
	}
	return outcome{state: session.JobCompleted, code: &code}
}

// recordEndLocked writes the terminal state. The caller holds r.mu; the store
// serialises its own writes, so holding a mutex across one costs nothing and
// keeps the two racing writers ordered.
func (m *Manager) recordEndLocked(r *Run, oc outcome) *session.JobRow {
	row, err := m.Session.FinishJob(r.id, oc.state, oc.code, oc.reason)
	if err != nil {
		if !errors.Is(err, session.ErrUnknownJob) {
			m.logger().Error("job: could not record outcome", "id", r.id, "err", err)
			return nil
		}
		// A-6's second half. The row stopped being 'running' while this process
		// still held the handle - a second opener swept it - and the old code
		// swallowed exactly that case, so the real exit code and its firing
		// vanished and the registry kept a "process ownership was lost" story
		// about a command that had in fact finished normally. We started the
		// process, so we know how it ended: correct the row and deliver the
		// firing. If the row is not ours to correct, say so loudly instead of
		// quietly - "my row was taken over" is precisely the fact a log line is
		// for.
		reclaimed, ok, rerr := m.Session.ReclaimJob(r.id, oc.state, oc.code, oc.reason)
		if rerr != nil {
			m.logger().Error("job: could not reclaim a swept row", "id", r.id, "err", rerr)
			return nil
		}
		if !ok {
			m.logger().Warn("job: outcome not recorded, the row is no longer ours",
				"id", r.id, "state", oc.state, "err", err)
			return nil
		}
		m.logger().Warn("job: swept row reclaimed; another process opened this session while the job ran",
			"id", reclaimed.ID, "state", reclaimed.State)
		return reclaimed
	}
	m.recordExit(row)
	return row
}

// recordExit turns a terminal state into a firing, and is the one place a job's
// last word is written.
//
// The invariant monitors must honour here: **finishing cancels the job's unfired
// monitors**, before this firing is queued. A job cannot produce another result,
// so a rule still armed against its log is a promise that can never be kept - and
// if it fired afterwards the run would take a step to report news about a job that
// had already said its last thing. Cancelling first also keeps the debt finite and
// decidable: at most one firing per monitor plus the exit firing per job, and
// UndeliveredFirings is then exactly "results owed", never "results that might
// still arrive". This is what makes a wait
// correct rather than merely fast: a job that ended while the model was doing
// something else has to still be waiting there when the model asks, because the
// alternative is a hold that ignores the one event it was asked to watch and
// blocks on a run that may never finish. That bug was found by a test, not by
// reading.
//
// The row is also the queue the batched wake will drain at the tool-loop
// boundary, so nothing here needs a callback registry, and a delivery is a
// timestamp rather than the disappearance of an event.
func (m *Manager) recordExit(row *session.JobRow) {
	elapsed := row.StartedAt
	fired := time.Now().UnixMilli()
	if row.EndedAt != nil {
		elapsed = *row.EndedAt - row.StartedAt
		fired = *row.EndedAt
	}
	// `fired` no longer dereferences row.EndedAt unguarded: the guard above has
	// always existed for `elapsed`, and reading the one and assuming the other
	// is a nil panic in the watcher goroutine. It is unreachable from today's
	// callers, which all pass a FinishJob RETURNING row whose ended_at cannot be
	// NULL - but a monitor's firing will come from a row read back rather than
	// written, and this function is documented as the one place a job's last
	// word is written, so it will be reused before it is audited again.
	if _, err := m.Session.RecordJobEvent(session.JobEventRow{
		JobID: row.ID, MonitorID: "exit", Reason: row.State, Elapsed: elapsed,
		ExitCode: row.ExitCode, FiredAt: fired,
	}); err != nil {
		m.logger().Error("job: could not record the exit firing", "id", row.ID, "err", err)
		return
	}
	// The result is in the queue, so the debt now exists. Say so after the write
	// and never before: a waiter woken here re-reads the store rather than
	// trusting the hint, and a hint that arrives before the row it describes
	// would be a wake that finds nothing and goes back to sleep.
	if m.OnFiring != nil {
		m.OnFiring()
	}
}

func logEnd(m *Manager, row *session.JobRow, id string, oc outcome) {
	if row != nil {
		m.logger().Info("job finished", "id", row.ID, "state", row.State,
			"elapsed_ms", elapsedSince(row).Milliseconds())
		return
	}
	m.logger().Info("job finished", "id", id, "state", oc.state)
}

// exceptionType reads the mirror of Python's subprocess exception that
// env.Local reports in Extra. It is a string lookup rather than a typed field
// because that is the shape the observation template already renders, and a job
// must classify the same results the model sees.
func exceptionType(res env.Result) string {
	if res.Extra == nil {
		return ""
	}
	s, _ := res.Extra["exception_type"].(string)
	return s
}

// JobID is a run's handle: short, path-safe, and stable for the tool call that
// produced it, because a promoted call already has a per-session unique number.
// tcno 0 means a run no tool call issued, which gets a random token rather than
// a colliding "j0".
func JobID(tcno int) string {
	if tcno > 0 {
		return "j" + strconv.Itoa(tcno)
	}
	// The trailing group, not the leading one: NewV7 is time-ordered, so the
	// first 12 hex digits are the millisecond and two jobs started in the same
	// tick would collide - which is not a cosmetic bug, it is Start refusing a
	// job because the registry thinks it is already live.
	id := uuid.NewV7().String()
	return "j" + id[len(id)-8:]
}

func (m *Manager) dir(d string) string {
	if d != "" {
		return d
	}
	return m.Session.Workdir
}

// local is the environment a bash job runs in: the foreground interpreter with
// its own deadline removed, because decision 2 says a job's length is the
// model's business and nothing else's.
func (m *Manager) local() *env.Local {
	if m.Local != nil {
		l := *m.Local
		l.Timeout = -1
		return &l
	}
	return &env.Local{Timeout: -1}
}

// background is the parent context of every job: not the agent's, not the tool
// call's, so a turn ending cannot end a run.
func (m *Manager) background() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// mergeEnv layers the overrides in the order that decides a winner: the
// environment the foreground calls already run under, then the manager's, then
// this job's. Skipping the first layer is the trap - a job that set one
// variable would otherwise silently drop everything the config's env block
// established, so a backgrounded `python` could resolve to a different
// interpreter than a foreground one.
func (m *Manager) mergeEnv(extra map[string]string) map[string]string {
	out := map[string]string{}
	for _, base := range []map[string]string{m.baseEnv(), m.Env, extra} {
		for k, v := range base {
			out[k] = v
		}
	}
	return out
}

func (m *Manager) baseEnv() map[string]string {
	if m.Local != nil {
		return m.Local.Env
	}
	return nil
}

func intPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// elapsedSince is a row's age at now, or at the moment it ended. It lives here
// rather than as a session method so the store stays a description of rows and
// this package keeps the clock.
func elapsedSince(j *session.JobRow) time.Duration {
	end := time.Now().UnixMilli()
	if j.EndedAt != nil {
		end = *j.EndedAt
	}
	return time.Duration(end-j.StartedAt) * time.Millisecond
}
