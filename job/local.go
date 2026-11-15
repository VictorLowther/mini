package job

import (
	"context"
	"errors"
	"fmt"
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
	// TCNO is the tool call that started the run, when there was one. It is a
	// display join, never a lookup path; the reason ids are derived from it is
	// that it is already unique per session.
	TCNO int
	// ParentID links this run to the job that spawned it, for trees and for
	// reaping a parent's children.
	ParentID string
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
	if s.Command == "" {
		return nil, errors.New("job: a command is required")
	}
	id := JobID(s.TCNO)
	if !session.JobIDPattern.MatchString(id) {
		return nil, fmt.Errorf("job: id %q is not path-safe", id)
	}

	// Adopt the handle before anything is written: the cap and the duplicate
	// check are the two refusals that cost nothing when they fire first, and a
	// refusal must not leave a row describing a run that never existed.
	r := &Run{id: id, kind: session.KindBash, output: m.Session.JobPath(id),
		dir: m.dir(s.Dir), started: time.Now().UnixMilli(), mgr: m, done: make(chan struct{})}
	if err := m.register(r); err != nil {
		return nil, err
	}

	// Append, never truncate: docs/jobs-research.md §4's one invariant is that a
	// file a transcript row points at is only ever written at EOF, so every
	// stored tcr_offset keeps describing the bytes it was computed against.
	if err := os.MkdirAll(filepath.Dir(r.output), 0o700); err != nil {
		return nil, r.abandon(fmt.Errorf("job: create result directory: %w", err))
	}
	file, err := os.OpenFile(r.output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, r.abandon(fmt.Errorf("job: open %s: %w", r.output, err))
	}

	row, err := m.Session.CreateJob(session.JobRow{
		ID: id, Kind: session.KindBash, ParentID: s.ParentID, TCNO: intPtr(s.TCNO),
		Label: s.Label, Command: s.Command, Description: s.Description,
		OutputPath: r.output, StartedAt: r.started,
	})
	if err != nil {
		file.Close()
		return nil, r.abandon(err)
	}
	// The row is authoritative from here: its timestamps are what the listing
	// and the elapsed arithmetic report, not the ones guessed above.
	r.output, r.started = row.OutputPath, row.StartedAt

	ctx, cancel := context.WithCancel(m.background())
	run := &interpRun{run: r, cancel: cancel}
	r.stop = run

	// Malformed pairs are dropped rather than guessed at: a job that starts with
	// nine of its ten variables is worth more than a job that refuses to start.
	envs := make(map[string]string, len(s.Env))
	for _, kv := range s.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envs[k] = v
		}
	}
	go run.watch(ctx, file, s, envs)
	return r, nil
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

	var code *int
	state, reason := session.JobCompleted, ""
	switch {
	case err != nil:
		// Execute returns a Go error for exactly one thing: the caller's
		// context ended. That is our cancel, so this is a kill, and the partial
		// output is already in the file. No exit code is claimed, because
		// reporting 0 for a run we interrupted would say it succeeded.
		state, reason = session.JobKilled, "killed on request: "+err.Error()
	case exceptionType(res) == "TimeoutExpired":
		// Unreachable for a job of its own, which has no deadline: it means a
		// promotion handed a foreground timer to a run that outlived it.
		state, reason = session.JobFailed, "timed out after the deadline it was given"
	case exceptionType(res) != "":
		// A parse error, a panic in the runner, or an exec failure: the command
		// never ran to completion, and ExceptionInfo says why in prose the model
		// can act on.
		state, reason = session.JobFailed, res.ExceptionInfo
	default:
		c := res.ReturnCode
		code = &c
		if c != 0 {
			state, reason = session.JobFailed, fmt.Sprintf("exited %d", c)
		}
	}

	row, ferr := m.Session.FinishJob(r.id, state, code, reason)
	if ferr != nil && !errors.Is(ferr, session.ErrUnknownJob) {
		m.logger().Error("job: could not record outcome", "id", r.id, "err", ferr)
		return
	}
	if row != nil {
		m.logger().Info("job finished", "id", row.ID, "state", row.State,
			"elapsed_ms", elapsedSince(row).Milliseconds())
	}
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
