// Package job runs commands that outlive the tool call that started them.
//
// The design is docs/jobs-research.md §4. Two ideas hold the whole package
// together. First, a job is a run and not a tool call: on day one the only kind
// of run is a bash command that outlived its foreground budget, but a subagent
// will want the same row, the same output file, the same completion event, and
// the same wake path, so nothing in the registry, the state machine, or the
// wait/kill API may know that a job has a pid. Process particulars live behind
// the kind-specific stopper interface, and a kind whose ending is not a process
// exit simply has none. Second, the database is the source of truth for what is
// live, never the transcript: compaction keeps recency and summarizes state
// away (docs/compaction-research.md §4), so a model that forgot it had a job
// must be able to ask and get the truth.
//
// Ownership rule, load-bearing: mini only ever signals a process it started and
// still holds the handle for. A job found running at open is swept to orphaned
// and never signalled, because after a restart a pid proves nothing about who
// owns it - the reuse window zero defends against with isRunningPID
// (manager.go:325) is closed here by refusing to touch what we did not spawn.
// The cost of that choice is honest: a run away from mini's own process
// lifetime can survive the sweep, and it is the human who sees it, via the
// warning the sweep returns and the footer count.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/session"
)

// Defaults. Both are quotable by the prompts when the tool descriptions land,
// and per docs/jobs-research.md §4 ("Numbers the prompts quote") they belong in
// agent.TemplateVars the day a prompt states them, not retyped there.
const (
	// DefaultMaxJobs is the per-session cap on live jobs. little-coder uses 8;
	// on a single-GPU box two training runs is already one too many, so the
	// number is a guard against a model multiplying its own mistakes, and the
	// error that carries it has to name the remedy.
	DefaultMaxJobs = 8
	// DefaultKillGrace is env.KillGrace: the same SIGINT-to-SIGKILL window a
	// foreground command gets, so "the same chance to clean up" is one number in
	// one place rather than two that have to agree.
	DefaultKillGrace = env.KillGrace
)

// ErrCap is what Start returns at the job cap. It is a value and not a wrapped
// message because the tool layer turns it into the model-facing refusal.
var ErrCap = errors.New("job: session job cap reached")

// unwindAllowance is how long a caller may wait for a cancelled run to return.
// The cancel starts a chain that outlives it: env's handler waits KillGrace for
// SIGINT to land, then SIGKILLs, then exec waits WaitDelay (also KillGrace) for
// the pipes to drain. Two grace windows plus slack, so a kill that is working is
// never reported as one that failed.
const unwindAllowance = 2*env.KillGrace + 2*time.Second

// UnknownError is what Get answers for an id that is not in this session.
type UnknownError struct{ ID string }

func (e *UnknownError) Error() string { return "job: unknown job " + e.ID }

// Manager owns the live jobs of one session and the rows that describe them.
//
// A Manager is safe for concurrent use. Live handles are in memory; every
// answer about state is read from the store, so a handle is only ever the
// mechanism for signalling and waiting, never the truth about whether something
// finished.
type Manager struct {
	// Session is the transcript this manager's jobs belong to.
	Session *session.Session
	// Logger receives the lifecycle lines; nil is slog.Default().
	Logger *slog.Logger
	// MaxJobs bounds live jobs per session. Zero means DefaultMaxJobs; a
	// negative value means no cap, for tests and for a human who knows better.
	MaxJobs int
	// KillGrace is how long a kill waits for the run to unwind. Zero means
	// DefaultKillGrace.
	KillGrace time.Duration
	// Local configures the interpreter a bash job runs in. nil is a fresh
	// env.Local. The deadline is always replaced with "none" - see local().
	Local *env.Local
	// Env is the environment override for every job; Spec.Env wins over it.
	Env map[string]string

	mu      sync.Mutex
	handles map[string]*Run
	// ctx is the parent of every job, so no turn, tool call, or model deadline
	// can end a run. nil means context.Background().
	ctx context.Context
}

// WithContext ties every job this manager starts to ctx, which is how an
// application lifetime - one GUI session, one --print run - becomes the outer
// bound of a job without any turn being able to reach it. Set it before the
// first Start.
func (m *Manager) WithContext(ctx context.Context) *Manager {
	m.ctx = ctx
	return m
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) maxJobs() int {
	if m.MaxJobs == 0 {
		return DefaultMaxJobs
	}
	return m.MaxJobs
}

func (m *Manager) grace() time.Duration {
	if m.KillGrace <= 0 {
		return DefaultKillGrace
	}
	return m.KillGrace
}

// Run is one job the manager has either started or knows about.
type Run struct {
	id      string
	kind    string
	output  string
	dir     string
	started int64
	mgr     *Manager

	// file is where the run's merged output goes, and the watcher closes it
	// when the command ends - including a file a foreground call handed over,
	// which is why the loop must not close one that got promoted.
	file *os.File
	// spec is kept so Detach can fill the row honestly, long after the caller
	// that launched the run has thrown its copy away.
	spec Spec

	// mu guards the three fields below, all of which change while a waiter is
	// looking: the run detaches when the foreground budget expires, ends when
	// the watcher returns, and records its Result in between.
	mu       sync.Mutex
	detached bool
	reason   string
	result   env.Result
	// end is the outcome once the watcher has classified one, and is nil while
	// the run is burning. It exists so Detach and the watcher cannot lose a
	// race: a command finishing at the instant its budget expires is exactly the
	// moment a row could be created already-finished and then never updated,
	// leaving a job listed as running forever. Both paths settle it under mu.
	end *outcome

	// done closes when the run reaches a terminal state. It is closed exactly
	// once, by the watcher that finished the row, so Wait and any future
	// monitor engine can both hang off it without racing for the state.
	done chan struct{}

	// stop is the kind-specific ability to end this run, and nil for a kind
	// that has no process to signal. Nothing outside a kind's own code reads
	// through it.
	stop stopper
}

// stopper ends a run. Implementing it is what makes a kind a runnable kind; the
// generic path calls it and never learns how it works.
type stopper interface {
	// stop requests the end and waits, bounded, for the run to unwind.
	stop(grace time.Duration) error
	// hurry requests the end and returns immediately. The reap uses it so a
	// session with six live jobs tears them down in the time one takes rather
	// than six times over: every exit path of the app is waiting on it.
	hurry()
}

func (r *Run) ID() string            { return r.id }
func (r *Run) Kind() string          { return r.kind }
func (r *Run) OutputPath() string    { return r.output }
func (r *Run) StartedAt() int64      { return r.started }
func (r *Run) Done() <-chan struct{} { return r.done }

// outcome is what a run became, decided by the kind that ran it and written to
// the registry only if the run turned out to be a job.
type outcome struct {
	state  string
	code   *int
	reason string
}

// Detached reports whether this run is a job: registered in the registry,
// counted against the cap, and expected to outlive the turn that started it.
func (r *Run) Detached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.detached
}

// Reason is why the run became a job - "started in the background" or the
// auto-background handoff - which is the difference between a model choosing
// this and a command outrunning its budget. Worth keeping because a job the
// model cannot explain to itself is a job it will kill by accident.
func (r *Run) Reason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reason
}

// Result is the env.Result of a finished run, valid once Done has closed. A
// run still burning returns the zero Result, which is honest: there is no
// return code for a command that has not exited, and inventing one is how a
// training run gets reported as succeeding.
func (r *Run) Result() env.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}

// Row reads the authoritative record.
func (r *Run) Row() (*session.JobRow, error) { return r.mgr.Session.GetJob(r.id) }

// State reads the authoritative state. An unreadable row reports running:
// "we could not tell" must not look like "it ended", and a job that looks
// finished when it holds the GPU is the failure this design cannot have.
func (r *Run) State() string {
	row, err := r.Row()
	if err != nil {
		return session.JobRunning
	}
	return row.State
}

// Alive reports whether this run can still change state.
func (r *Run) Alive() bool { return r.State() == session.JobRunning }

// Elapsed is the run's age, ending at its end time once it has one.
func (r *Run) Elapsed() time.Duration {
	end := time.Now().UnixMilli()
	if row, err := r.Row(); err == nil && row.EndedAt != nil {
		end = *row.EndedAt
	}
	d := time.Duration(end-r.started) * time.Millisecond
	if d < 0 {
		return 0
	}
	return d
}

// Wait blocks until the run is finished or ctx is done. A cancelled wait is not
// an error in the job: the run keeps going, which is the whole point, so the
// caller gets ctx.Err() and is expected to answer with current state rather
// than with a failure (the crush/zero rule that a blocked read never loses the
// run).
func (r *Run) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Hurry requests the end without waiting for the unwind. The state stays
// 'running' until the watcher records it, which is correct: nothing has been
// decided yet, only asked for.
func (r *Run) Hurry() {
	if r.stop != nil {
		r.stop.hurry()
	}
}

// Kill ends the run through its kind's stopper. A run with no stopper - one
// that was never ours to signal - reports that instead of pretending.
func (r *Run) Kill() error {
	if r.stop == nil {
		return fmt.Errorf("job: %s has no process mini can signal (state %s)", r.id, r.State())
	}
	return r.stop.stop(r.mgr.grace())
}

// abandon ends a run that never got as far as a process. Closing done is what
// releases register's watcher and drops the live handle: dropping the handle
// alone would strand the goroutine that waits on it, leaking one per refused
// start, and a refused start is exactly the path a model can hit repeatedly
// against the cap.
func (r *Run) abandon(err error) error {
	close(r.done)
	return err
}

// forget drops the live handle. The row stays: a terminal job is still a job
// the model can list, read, and quote.
func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.handles, id)
	m.mu.Unlock()
}

// Get returns the live handle for id, or UnknownError. Only runs this manager
// started are addressable for signalling; historical rows are still readable
// through Session.GetJob, which is the same split the kill path depends on.
func (m *Manager) Get(id string) (*Run, error) {
	m.mu.Lock()
	r, ok := m.handles[id]
	m.mu.Unlock()
	if !ok {
		return nil, &UnknownError{ID: id}
	}
	return r, nil
}

// List returns every row in the session, newest first, so a caller can render
// the tree from parent_id without a second query.
func (m *Manager) List() ([]session.JobRow, error) { return m.Session.ListJobs() }

// register adopts a row and its handle into the live set, enforcing the cap.
func (m *Manager) register(r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The map is created on first use so a zero-value Manager works: every
	// other type in mini is built as a struct literal with the fields it needs
	// (env.Local, agent.Loop), and a Manager wired up that way must not have to
	// remember a constructor. Reads and ranges below tolerate nil; only
	// assignment cannot.
	if m.handles == nil {
		m.handles = make(map[string]*Run)
	}
	if max := m.maxJobs(); max > 0 && len(m.handles) >= max {
		return fmt.Errorf("%w: %d live, stop one with job_kill first", ErrCap, len(m.handles))
	}
	if _, dup := m.handles[r.id]; dup {
		return fmt.Errorf("job: %s is already live", r.id)
	}
	m.handles[r.id] = r
	// Dropping the handle is the registry's job, not the kind's: it hangs off
	// the end of the run itself, so a kind that forgets, or panics before, its
	// own bookkeeping cannot leak a slot in the live set - which would
	// otherwise count against the cap forever and quietly refuse the model's
	// next job.
	go func() {
		<-r.done
		m.forget(r.id)
	}()
	return nil
}

// Sweep ends every row the store still claims is running, and logs what it
// found. Call it once per session at open, before anything can start a job:
// a restart cannot resurrect a process, and the model must never be told a job
// is running because a stale column says so.
func (m *Manager) Sweep() []string {
	warnings, err := m.Session.SweepOrphanedJobs()
	if err != nil {
		m.logger().Error("job sweep failed", "err", err)
		return []string{err.Error()}
	}
	for _, w := range warnings {
		m.logger().Warn("job", "warning", w)
	}
	return warnings
}

// KillAll ends every live job of the session and reports how many it touched.
// It is the reap-on-quit half of "a job outlives a turn, never a session", so
// it runs on GUI quit and at --print end. Failures are logged and counted
// anyway: a kill that could not land must not leave the caller believing
// nothing was running.
func (m *Manager) KillAll() int {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.handles))
	for _, r := range m.handles {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	// Ask for every end first: an unwind costs the SIGINT-to-SIGKILL window
	// plus the bounded pipe drain, and doing that one job at a time would make
	// quitting cost seconds per live job.
	var live []*Run
	for _, r := range runs {
		if r.Alive() {
			live = append(live, r)
			r.Hurry()
		}
	}
	// One shared deadline, so a job that refuses to die delays only its own
	// bookkeeping and never the whole exit path.
	bound := time.After(m.grace() + unwindAllowance)
	for _, r := range live {
		select {
		case <-r.done:
		case <-bound:
			m.logger().Warn("reap: job still unwinding at exit", "id", r.id, "state", r.State())
		}
	}
	if len(live) > 0 {
		m.logger().Info("reap: ended live jobs", "count", len(live))
	}
	return len(live)
}
