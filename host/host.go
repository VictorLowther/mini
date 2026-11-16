// Package host assembles and owns the sessions of one mini process.
//
// It exists because `mini` used to have exactly one of everything per process -
// one session, one interpreter, one job registry, one loop - and that was never
// a rule anywhere. It was a property of buildRun in cmd/mini, which assembled
// all six objects in one function and returned one value, and of run, which
// drove exactly one front end from it. The design and the survey behind it are
// docs/parallel-sessions.md; the short version is that the loop, the
// interpreter, the registry and the store were already safe to use concurrently,
// so this package is not a thread-safety layer. It is the removal of six
// one-per-process assumptions, plus the place where a session can be built
// without flags, a terminal, or a human.
//
// Two things deliberately are NOT here. It does not parse flags or resolve
// config specs: `Spec.Config` is a merged map, because a subagent has no argv
// and it does need "the parent's config except the model name and the step
// limit" - that is zero's discipline of resolving before the seam
// (swarm/member.go:23), and it is what keeps this package from importing
// cmd/mini, which would be the dependency pointing the wrong way. And it does
// not own a front end: `Session.Run` is synchronous because both existing front
// ends call `Loop.Run` inline and stream into it through `agent.Hooks`, and
// `Go`/`Wait` exist for callers that need a job-shaped lifecycle instead.
//
// Panics are not caught anywhere in this package. That is a ruling, not an
// omission (docs/parallel-sessions.md §9.9): while mini is in private active
// development a panic is a bug caught red-handed, and a recover that turns one
// into a tidy `failed` row would make the registry - which is this design's
// source of truth - tell the parent "the subagent did not work out" when the
// truth is that mini broke.
package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"uuid"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// Process owns the genuinely process-wide half of a mini run: the shared store,
// the live sessions, the context that bounds every job in the process, and the
// global environment - which is exactly the kind of thing that looks
// per-session-shaped right up until a second session exists to disagree with it.
//
// A Process is safe for concurrent use. Open may be called from any goroutine,
// any number of times.
type Process struct {
	// Store is the transcript source of truth shared by every session in this
	// process. The Process does not own it: the caller opened it, and the caller
	// closes it, after Close has reaped the sessions - the order matters because
	// a reap records terminal states that have to land in the database.
	Store *session.Store

	// ChildModel, when set, supplies the chat client for a spawned session
	// instead of the one its config describes. Tests use it to run a real
	// parent/child pair with no server; an embedding host could use it to give
	// subagents a cheaper model than the conversation that spawned them, which
	// is the legitimate production case and the reason it is a field on the
	// process rather than an argument to Spawn.
	ChildModel func(kind string) model.Model

	// Logger receives the process-level records. Each session gets a copy with
	// its own id attached, so a crash or a stalled run names the session it
	// belongs to rather than joining an undifferentiated stream
	// (docs/parallel-sessions.md §7 step 2).
	Logger *slog.Logger

	// ProcessCtx is the ancestor of every job every session starts. nil means
	// context.Background(). Cancelling it is how the whole process winds down;
	// per-session cancellation is Session.Quit.
	ProcessCtx context.Context

	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
	order    []uuid.UUID
	closed   bool

	envOnce sync.Once

	// agentJobs maps "parent session id / job id" to the child it runs. The
	// store cannot answer it - a job row knows its own session, and the child's
	// rows live under the CHILD's session id - so the edge is held in memory,
	// which is also why it cannot survive a restart, and why the sweep orphans
	// rather than signals (§5.11).
	agentJobs map[string]*Session
}

// New returns a Process over an already-opened store, and collects what no
// session can reach any more.
//
// The sweeps run here rather than being left to the caller because the failure
// they repair is silent and cumulative: a child session whose parent was deleted
// by an older build, or a result directory whose row was deleted a moment before
// its files, is invisible until the disk is full. Making it a method a caller can
// forget is how that becomes a support question, and it is the same reasoning
// that put the request gate in the model client rather than in each session
// (§5.7) - the unskippable place is the one that works.
//
// A failed sweep is logged and does not stop the process: refusing to run because
// garbage could not be collected would trade a leak for an outage.
func New(store *session.Store, ctx context.Context) *Process {
	if ctx == nil {
		ctx = context.Background()
	}
	p := &Process{
		Store: store, ProcessCtx: ctx,
		sessions:  map[uuid.UUID]*Session{},
		agentJobs: map[string]*Session{},
	}
	for _, w := range warnIf(store.SweepLineage()) {
		p.logger().Warn("session sweep", "warning", w)
	}
	if n, err := store.SweepStrayResults(); err != nil {
		p.logger().Warn("result-directory sweep failed", "err", err)
	} else if n > 0 {
		p.logger().Info("removed result directories no session names", "count", n)
	}
	return p
}

// warnIf folds a (warnings, error) sweep into the one shape both callers want.
func warnIf(warnings []string, err error) []string {
	if err != nil {
		return append(warnings, err.Error())
	}
	return warnings
}

func (p *Process) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// LoadGlobalEnv applies the global .env file to the process environment, without
// overriding variables that are already set - dotenv's default, and the behavior
// mini has always had.
//
// It is guarded by a sync.Once for one reason. It used to be called from
// buildRun, which runs once per process, so mutating os.Environ() there was
// harmless; with assembly callable N times it becomes a race between one
// session's startup and another session's shells snapshotting the environment
// with os.Environ() (env/local.go:148), which is precisely the cross-session
// interference the rest of this package is organized to prevent. Process-wide
// state is set up once, before sessions exist, or not at all.
func (p *Process) LoadGlobalEnv() {
	p.envOnce.Do(func() {
		values, err := config.LoadEnv(config.GlobalConfigFile())
		if err != nil {
			p.logger().Warn("could not read the global config", "path", config.GlobalConfigFile(), "err", err)
			return
		}
		for k, v := range values {
			if _, ok := os.LookupEnv(k); !ok {
				_ = os.Setenv(k, v)
			}
		}
	})
}

// Spec is one session's fully-resolved instructions. Like zero's MemberSpec,
// everything in it is decided before it crosses the seam, so no part of the
// assembly can guess differently about what an absent value meant.
type Spec struct {
	// Config is the merged configuration - the same map a front end resolved
	// from files, embedded names and key=value overrides. Required. Its `agent`,
	// `model` and `environment` sections are read from here.
	Config map[string]any

	// Workdir is the session's working directory. Empty means whatever the
	// `environment.cwd` config says, and then the process's own directory.
	Workdir string
	// Project is the identity `mini sessions` and `mini -r` group by. It is
	// computed by the caller (cmd/mini resolves the git toplevel) rather than
	// here, because it is a product rule about checkouts and symlinks, not a
	// property of running a conversation. A subagent inherits its parent's.
	Project string

	// Kind is session.KindTop (the default) or session.KindSubagent.
	Kind string
	// ParentID is the session that opened this one. Required for a subagent:
	// session.Create refuses a child whose parent is unknown, and refuses a
	// grandchild outright, which is the store's own backstop for the two-level
	// tree (docs/parallel-sessions.md §5.6).
	ParentID string
	// Name is the short human-facing label a parent gives a subagent. It lands
	// in the session's title, which is already written first-write-wins, so the
	// backfill from the task text cannot overwrite a name that was chosen on
	// purpose (§5.5).
	Name string

	// Resume continues an existing session instead of creating one. The caller
	// resolved it - including the rule that a human may only resume a top-level
	// session, which store.Find enforces (§5.4).
	Resume *session.Session

	// Hooks is the user-in-the-loop seam. nil means no prompts, which is what a
	// batch run wants and what a subagent gets until its confirmations are
	// bubbled to its parent's front end (§5.5).
	Hooks agent.Hooks

	// OutputPath is the trajectory file. Empty means a per-session path under
	// the state directory, which is the default for exactly one reason: one
	// fixed name for the whole process lets a second session overwrite the
	// first one's record and then the filename misdescribes what it holds.
	// A child that wants the parent's path has to ask for it deliberately.
	OutputPath string

	// Logger overrides the session's logger wholesale. Normally unset; the
	// process logger plus a session= attribute is the right answer.
	Logger *slog.Logger

	// Model supplies the chat client, bypassing the `model:` section and the
	// startup window lookup entirely. Tests use it to drive a real session
	// against a scripted model; a future non-HTTP backend would too. Leaving it
	// nil is the normal case.
	Model model.Model
}

// Open assembles one session: config, model client, interpreter, durable
// transcript row, job registry, and the loop that ties them together. It starts
// nothing - call Run or Go for that - and it does not touch process-global state
// beyond the store it was handed.
func (p *Process) Open(spec Spec) (*Session, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("host: process is closed")
	}
	p.mu.Unlock()

	if spec.Config == nil {
		return nil, errors.New("host: Spec.Config is required")
	}
	agentCfg := agentConfigOf(spec.Config)
	modelCfg := config.Sub(spec.Config, "model")
	envCfg := config.Sub(spec.Config, "environment")

	if agentCfg.SystemTemplate == "" || agentCfg.InstanceTemplate == "" {
		return nil, errors.New("the agent config needs system_template and instance_template")
	}

	sess, err := p.row(spec, modelCfg)
	if err != nil {
		return nil, err
	}
	log := spec.Logger
	if log == nil {
		log = p.logger().With("session", sess.ID.String())
	}

	e := NewEnvironment(envCfg)
	if spec.Workdir != "" {
		e.CWD = spec.Workdir
	}
	if e.CWD == "" {
		e.CWD = sess.Workdir
	}
	e.Logger = log

	// One tool set per session kind, and it reaches all three renderings from
	// here: the request array (client.Tools), the parser's acceptance (same
	// client), and the prompt listing (loop.Tools). Setting one without the other
	// is the drift §5.6 exists to prevent, so both are assigned from one value.
	tools := model.TopLevelTools()
	if spec.Kind == session.KindSubagent {
		tools = model.SubagentTools()
	}

	m := spec.Model
	if m == nil && spec.Kind == session.KindSubagent && p.ChildModel != nil {
		m = p.ChildModel(spec.Kind)
	}
	if m == nil {
		sushi, err := modelClientOf(modelCfg)
		if err != nil {
			return nil, err
		}
		sushi.Tools = tools
		m = sushi
		// The window is the server's to know, and the lookup is cached per
		// (base URL, model) - so a subagent costs no extra request and cannot get
		// a different answer from the session that made it. A caller that brought
		// its own client has no model list to read, so the configured window
		// stands.
		agentCfg.ContextWindow, _ = sushi.DiscoverContextWindow(p.processCtx(), agentCfg.ContextWindow)
	}
	agentCfg.OutputPath = p.trajectoryPath(spec, sess, agentCfg.OutputPath)

	l := &agent.Loop{
		Cfg: agentCfg, Model: m, Env: e, Logger: log, Recorder: sess, Hooks: spec.Hooks,
		Tools: tools,
		// One number for the foreground wait, from the same setting that governs
		// the environment's own deadline.
		CommandBudget: e.Timeout,
	}

	// Job registry, one per session. It interprets backgrounded commands through
	// the same interpreter that answers the foreground bash tool, so a job is the
	// same shell with a longer leash rather than a second dialect the model
	// cannot see coming. Its parent context is the session's, not any turn's -
	// which is what makes "a job outlives a turn, never a session" a statement
	// with a referent (§5.11, §9.2).
	jctx, jcancel := context.WithCancel(p.processCtx())
	jobs := (&job.Manager{Session: sess, Local: e, Logger: log}).WithContext(jctx)
	l.Jobs = jobs

	// Every loop gets the seam; only a top-level session can use it, and Spawn
	// says so. Setting it unconditionally is what makes "the tool is advertised
	// but the call cannot land" impossible - the array, the instructions and the
	// capability are decided from the same kind (§5.6).
	s := &Session{
		born: time.Now(),
		ID:   sess.ID, Cfg: agentCfg, Model: m, Env: e, Loop: l, Sess: sess, Jobs: jobs,
		Configs: spec.Config,
		Kind:    sess.Kind, Name: sess.Title, Workdir: e.CWD, Project: sess.Project,
		proc: p, log: log, jobCancel: jcancel,
		finished: make(chan struct{}), wake: make(chan struct{}, 1),
	}
	if sess.ParentID != "" {
		if pid, err := uuid.Parse(sess.ParentID); err == nil {
			s.Parent = pid
		}
	}

	// A resume loads the stored transcript and continues it; the stored system
	// prompt wins even if the config changed underneath it, because the database
	// is the source of truth for a resumed run. A fresh session gets nothing
	// pre-loaded and Run renders the templates as always.
	if spec.Resume != nil {
		msgs, err := sess.Transcript()
		if err != nil {
			return nil, fmt.Errorf("host: resume transcript: %w", err)
		}
		l.Cost, l.NCalls = sess.Cost, sess.NCalls
		l.Resume(msgs)
		s.ResumeMsgs, s.ResumeNote = msgs, fmt.Sprintf("resumed session %s (%d turns)", sess.ID, len(msgs))
	}

	// The sweep runs before anything can start: a row still marked 'running' is a
	// claim about a process this mini does not own, so it becomes orphaned and
	// the human is told, which is the honest version of "still running".
	s.SweepWarn = jobs.Sweep()
	// A child is unattended by definition: it has no front end, and the
	// confirmation bubbling of §5.5 does not exist until step 7. Marking it here
	// is what lets a subagent delegate nothing while not pretending a batch run
	// has a human it never got.
	if sess.Kind == session.KindSubagent {
		s.unattended = true
	}

	l.Spawner = s

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		jcancel()
		return nil, errors.New("host: process is closed")
	}
	p.sessions[s.ID] = s
	p.order = append(p.order, s.ID)
	p.mu.Unlock()

	return s, nil
}

// row resolves or creates the session record for a spec.
func (p *Process) row(spec Spec, modelCfg map[string]any) (*session.Session, error) {
	if spec.Resume != nil {
		return spec.Resume, nil
	}
	kind := spec.Kind
	if kind == "" {
		kind = session.KindTop
	}
	// session.Create is what resolves root_id and enforces the two-level tree,
	// so the host passes the lineage through rather than computing it here: a
	// child inherits its parent's root, an unknown parent is an error, and a
	// grandchild is refused, whichever caller got here (§5.3, §5.6).
	return p.Store.Create(session.Row{
		ID:        uuid.NewV7(),
		Title:     spec.Name,
		Workdir:   spec.Workdir,
		Project:   spec.Project,
		ModelName: config.String(modelCfg, "model_name", ""),
		Kind:      kind,
		ParentID:  spec.ParentID,
	})
}

// trajectoryPath resolves the trajectory file. A subagent always gets a path of
// its own, even when its spec carries one: the parent's trajectory is the human's
// artifact, and §9.8's whole worry is that a child's config is derived from the
// parent's, so any path that survives that derivation is inherited by accident
// rather than chosen. A caller that genuinely wants to redirect one child's
// record asks with a path the parent is not using; nothing here can tell those
// two cases apart, so the safe reading wins and the leak becomes impossible.
func (p *Process) trajectoryPath(spec Spec, sess *session.Session, configured string) string {
	kind := spec.Kind
	if kind == "" {
		kind = session.KindTop
	}
	if kind == session.KindTop {
		if spec.OutputPath != "" {
			return spec.OutputPath
		}
		if configured != "" {
			return configured
		}
	}
	if _, err := config.EnsureStateDir(); err != nil {
		return "mini-" + sess.ID.String() + ".traj.json"
	}
	return filepath.Join(config.StateDir(), "trajectories", sess.ID.String()+".traj.json")
}

// Sessions lists the live sessions in the order they were opened.
func (p *Process) Sessions() []*Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Session, 0, len(p.order))
	for _, id := range p.order {
		if s, ok := p.sessions[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Children returns the live sessions opened under one parent.
func (p *Process) Children(parent uuid.UUID) []*Session {
	var out []*Session
	for _, s := range p.Sessions() {
		if s.Parent == parent {
			out = append(out, s)
		}
	}
	return out
}

// Get returns a live session by id.
func (p *Process) Get(id uuid.UUID) (*Session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	return s, ok
}

// Close ends every session in the process, children before their parents.
//
// The order is the whole point (§9.2): a parent's reap reaches into its
// children, so ending the parent first means it waits on children that are
// already being torn down by someone else, and the one state that must never be
// reachable is a running process whose session row is already closed. Children
// first makes that ordering impossible rather than merely unlikely.
//
// It does not close the store, and it does not cancel Process.ProcessCtx - the
// caller owns both. The store in particular must outlive this call, so that the
// terminal states the reaps record are still writable when they are written.
func (p *Process) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	live := make([]*Session, 0, len(p.order))
	for _, id := range p.order {
		if s, ok := p.sessions[id]; ok {
			live = append(live, s)
		}
	}
	p.mu.Unlock()

	// Two passes, children then parents, because the tree is exactly two levels
	// deep by construction (§5.6) - no general topological sort is warranted for
	// a graph that cannot be three nodes tall, and saying so is better than
	// writing the loop that pretends it might be.
	for _, s := range live {
		if s.Kind != session.KindTop {
			_ = s.Close()
		}
	}
	for _, s := range live {
		if s.Kind == session.KindTop {
			_ = s.Close()
		}
	}
	return nil
}

// forget drops a closed session from the live set. The store keeps its row -
// this is a registry of what is running in this process, not of what ever
// happened, and conflating the two is how a UI ends up showing conversations the
// sweep already decided were dead.
func (p *Process) forget(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
	for i, o := range p.order {
		if o == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			return
		}
	}
}

// registerAgentJob records the child behind one of a parent's agent rows.
func (p *Process) registerAgentJob(parentID, jobID string, child *Session) {
	p.mu.Lock()
	if p.agentJobs == nil {
		p.agentJobs = map[string]*Session{}
	}
	p.agentJobs[parentID+"/"+jobID] = child
	p.mu.Unlock()
}

// childOfJob finds the session behind an agent row, scoped to the asking parent.
// The scope is a security property, not a nicety: job ids are unique per session,
// so an unscoped lookup would let one session send to another's subagent by
// guessing an id - and a send is a user message into somebody's conversation.
func (p *Process) childOfJob(parentID, jobID string) (*Session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.agentJobs[parentID+"/"+jobID]
	return c, ok
}

func (p *Process) processCtx() context.Context {
	if p.ProcessCtx != nil {
		return p.ProcessCtx
	}
	return context.Background()
}

// agentConfigOf and modelClientOf read the two sections Open needs from the
// merged config. They are separate functions because cmd/mini validates model
// presence in its own order and reports it its own way; the rules live here so a
// session assembled by any caller is assembled the same way.

func agentConfigOf(merged map[string]any) *agent.Config {
	return agent.ConfigFromMap(config.Sub(merged, "agent"))
}

func modelClientOf(modelCfg map[string]any) (*model.Sushi, error) {
	m := model.NewSushi(modelCfg)
	if m.Model == "" {
		return nil, errors.New("no model set. Pass -m NAME or run `mini config setup`.")
	}
	return m, nil
}

// NewEnvironment builds the local interpreter from the `environment:` section.
// Exported so cmd/mini can keep constructing one for its own startup checks;
// Open does it for every session it creates.
func NewEnvironment(cfg map[string]any) *env.Local {
	l := &env.Local{
		CWD:     config.String(cfg, "cwd", ""),
		Env:     map[string]string{},
		Timeout: time.Duration(config.Int(cfg, "timeout", int(env.DefaultTimeout.Seconds()))) * time.Second,
	}
	for k, v := range config.Sub(cfg, "env") {
		if s, ok := v.(string); ok {
			l.Env[k] = s
			continue
		}
		l.Env[k] = fmt.Sprint(v)
	}
	return l
}
