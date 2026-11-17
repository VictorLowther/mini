package host

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// Session is one conversation: the six objects a mini run is made of, bound to
// one durable transcript row and one job registry, and addressable while it is
// running. It is the unit both features are built on - feature 2 opens several
// of them, feature 1 opens one from inside another - and the type is deliberately
// ignorant of what is displaying it.
//
// The zero value is not usable; get one from Process.Open.
type Session struct {
	ID  uuid.UUID
	Cfg *agent.Config
	// Model is the chat client. Typed as the interface rather than *model.Sushi
	// so Spec.Model can inject a scripted one, which is how these sessions are
	// tested without a server.
	Model   model.Model
	Env     *env.Local
	Loop    *agent.Loop
	Sess    *session.Session
	Jobs    *job.Manager
	Configs map[string]any // the merged config this session was assembled from

	Kind      string
	Name      string
	Workdir   string
	Project   string
	Parent    uuid.UUID
	SweepWarn []string

	// ResumeMsgs and ResumeNote carry a continued transcript to the front end,
	// which is what lets a replayed session render its history without lying
	// about having just generated it.
	ResumeMsgs []model.Message
	ResumeNote string

	proc *Process
	log  *slog.Logger

	// wake carries "something happened to this session's child work" to the
	// wrapper holding its agent row: a buffered one, so a full channel already
	// means "an unread event exists" and the sender never blocks on a goroutine
	// that may be about to be cancelled.
	wake chan struct{}

	// jobCancel is the session's own bound on its jobs' lifetime. Cancelling it
	// is how a session ends work that outlived its turn without ending other
	// sessions' - "a job outlives a turn, never a session" with the referent made
	// explicit (§5.11).
	jobCancel context.CancelFunc

	mu           sync.Mutex
	running      bool
	finished     chan struct{}
	finishedOnce sync.Once
	done         sync.Once
	info         map[string]any
	runErr       error
	closed       bool
	born         time.Time

	// parked says a wrapper for this session is waiting in waitQuiet, so a send
	// can tell "somebody will re-drive this" from "nobody is left to". Cleared
	// before the wrapper acts on a wake, so a send that arrives during a live
	// turn takes the running path instead of starting a second run beside it.
	parked bool
}

func (s *Session) Logger() *slog.Logger { return s.log }

// ParentID is the session that opened this one, or "" for a top-level one.
func (s *Session) ParentID() string {
	if s.Parent == uuid.Nil() {
		return ""
	}
	return s.Parent.String()
}

// Run drives the loop to completion on the calling goroutine. It is synchronous
// because that is what both existing front ends need: they stream into the loop
// through agent.Hooks while it runs.
//
// A panic is not caught here. See the package note - it is a ruling, and adding
// the recovery that the survey's Go harness uses at this exact seam
// (swarm/member.go:70-78) would be the single most tempting way to make mini
// worse at telling the truth about itself.
func (s *Session) Run(ctx context.Context, task string) (map[string]any, error) {
	if !s.claim() {
		// Loop.Run has its own single-flight guard; this one exists because Go
		// and Run share a `finished` channel and a second caller would close it
		// out from under the first, and because Wait's contract is "the run that
		// happened", which needs exactly one of them to have happened.
		return nil, agent.ErrRunInProgress
	}
	info, err := s.Loop.Run(ctx, task)
	s.finish(info, err)
	return info, err
}

// Go starts a run in its own goroutine and returns as soon as it is going. This
// is the shape a subagent needs - a job, not a call the spawner waits on - and
// the answer to the question "how does a model pay for a long command": it does
// not, the result arrives at a step boundary later.
func (s *Session) Go(task string) {
	go func() {
		// The run's context is the process's, not the caller's: a handed-off
		// conversation must not die because the tool call that started it was
		// answered. Quit and Process.Close are the ways to end it, and both
		// cancel the job context this inherits from.
		ctx, cancel := context.WithCancel(s.proc.processCtx())
		defer cancel()
		info, err := s.Run(ctx, task)
		_ = info
		_ = err
	}()
}

// Wait blocks for the run that Go or Run started. It returns the same value Run
// would have: the loop's last message extra (exit_status, submission), or the
// error that ended the run.
func (s *Session) Wait(ctx context.Context) (map[string]any, error) {
	select {
	case <-s.finished:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.info, s.runErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Running reports whether a run is in progress. Advisory, like Loop.Running -
// the guarantee lives in the single-flight guard, not here.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// CancelTurn interrupts the current command without ending the run, which is
// Ctrl-C's meaning in every front end.
func (s *Session) CancelTurn() { s.Loop.CancelTurn() }

// Quit ends the run and stops this session's jobs, without touching any other
// session's. It is the per-session lever the starvation ruling relies on
// (§5.7): the bound on concurrent requests is accepted as a fact of local-model
// life because the human can end the session that is hoarding them.
func (s *Session) Quit() error {
	if s.jobCancel != nil {
		s.jobCancel()
	}
	s.Loop.CancelTurn()
	return s.Close()
}

// Close reaps this session's live jobs and deregisters it. Idempotent, and it
// does not close the store.
//
// The reap happens before the process drops the session because the registry is
// the source of truth for what is live: a row left 'running' after the handle is
// gone is precisely the stale claim the open-time sweep exists to repair, and
// repairing it at the next launch is a worse answer than not creating it.
func (s *Session) Close() error {
	var err error
	s.done.Do(func() {
		// Children first, in both directions of the rule (§9.2): a parent's reap
		// reaches into its children, and the one state that must never be
		// reachable is a running process whose session is already closed.
		if s.proc != nil {
			for _, kid := range s.proc.Children(s.ID) {
				_ = kid.Close()
			}
		}
		if n := s.Jobs.KillAll(); n > 0 {
			s.log.Info("reaped live jobs on session close", "count", n)
		}
		if s.jobCancel != nil {
			s.jobCancel()
		}
		// A session that never ran still has a `finished` channel with nobody
		// waiting on it; closing it makes Wait return rather than block forever
		// on a conversation that ended before it started.
		s.finish(nil, sessionClosed)
		s.proc.forget(s.ID)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	})
	return err
}

// Closed reports whether Close has run.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// LiveJobs counts the rows this session's registry still believes are running,
// from the store rather than from memory - which is the rule the whole jobs
// design rests on: a model that forgot it had a job must be able to ask and get
// the truth (docs/compaction-research.md §4). Feature 1's agent rows test this
// to decide whether a child has actually finished (§5.10, §9.10).
func (s *Session) LiveJobs() int { return s.Jobs.StillRunning() }

// QueueMessage speaks to a session that is already running, which is the only
// supported way to reach a live loop; a second Run is refused. `job_send` (step
// 6) delivers through here for a running child.
func (s *Session) QueueMessage(m model.Message) { s.Loop.QueueMessage(m) }

func (s *Session) setParked(v bool) {
	s.mu.Lock()
	s.parked = v
	s.mu.Unlock()
}

func (s *Session) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.closed {
		return false
	}
	s.running = true
	return true
}

func (s *Session) finish(info map[string]any, err error) {
	s.mu.Lock()
	s.running = false
	if s.info == nil {
		s.info = info
	}
	if err != nil && !errors.Is(err, sessionClosed) {
		s.runErr = err
	}
	s.mu.Unlock()
	s.markFinished()
}

// markFinished closes the wait channel exactly once, whether the run ended,
// the session was closed, or neither ever happened.
func (s *Session) markFinished() {
	s.finishedOnce.Do(func() { close(s.finished) })
}

// Elapsed is how long the session has been open, for a roster row.
func (s *Session) Elapsed() time.Duration { return time.Since(s.born) }

var sessionClosed = errors.New("host: session closed")
