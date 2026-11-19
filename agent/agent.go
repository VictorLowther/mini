// Package agent runs the linear query-execute loop. The control flow began as a
// port of minisweagent/agents/default.py, which was the shortest path to a loop
// worth testing against, and it has grown past that shape: compaction, the
// session store, and background jobs each answer a problem the ported agent does
// not have. Provenance, not a ceiling.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
	"github.com/VictorLowther/mini/tpl"
)

// Version is reported as mini_version in every trajectory.
const Version = "0.1.0"

// Config is AgentConfig plus InteractiveAgentConfig.
//
// It carries no budgets. Step limits, dollar limits and wall clocks are relics of
// the one-shot benchmark harness this port came from, where bounding an
// autonomous run was the only thing standing between a task and forever. Here a
// human is watching a persistent session, `jobs` reports elapsed time, and the
// brakes that work are job_kill, Quit and closing the process - so a budget
// stopped being a guardrail and became a second, invisible author of the report
// (docs/parallel-sessions.md §5.14). mode, whitelist_actions
// and confirm_exit are read by the interactive hooks and ignored when the loop
// has no hooks.
type Config struct {
	SystemTemplate   string
	InstanceTemplate string
	// No step, cost or wall-clock budget. See Cost below for what replaced them
	// and why - docs/parallel-sessions.md §5.14.
	MaxConsecutiveFormatErrors int
	OutputPath                 string

	Mode             string
	WhitelistActions []string
	ConfirmExit      bool

	// Compaction (see docs/compaction-research.md §4 and agent/compaction.go):
	// the live context is append-only; a summary event fires near the edge of
	// the window, reads the warm prompt cache, and anchors the new projection.
	// ContextWindow is what the server actually runs - native extent, or the
	// YaRN-extended one. A run started by the CLI never trusts this number
	// over what the server's model list says; agent.context_window is the
	// fallback (see model.DiscoverContextWindow). The zero it can still hold
	// when a Config is built directly disables automatic compaction, crush's
	// rule for models whose window is unknown. 0 < CompactThresholdRatio <= 1
	// picks the trigger point; CompactKeepTokens is the verbatim tail budget.
	ContextWindow           int
	CompactEnabled          bool
	CompactThresholdRatio   float64
	CompactKeepTokens       int
	CompactSummaryMaxTokens int

	// MaxParallelCalls bounds how many tool calls from one assistant turn run
	// at the same time. The commands in a batch are independent by contract -
	// every action is its own shell, so nothing carries over between them - and
	// the loop now relies on that by running them together. This is the one
	// number that says how far to take it: 0 means DefaultMaxParallelCalls, a
	// negative value starts all of them at once, and a batch is never given more
	// workers than it has actions.
	MaxParallelCalls int
}

// InteractiveModes are the only values agent.mode may hold. "confirm" asks a
// human before running anything not whitelisted; "yolo" never asks; "human"
// issues the human's own commands instead of querying the model.
var InteractiveModes = []string{"human", "confirm", "yolo"}

// ValidateMode rejects a mode the port does not implement.
//
// This is the invariant upstream got for free from pydantic's Literal and the Go
// port silently dropped (A-2). Checking it at the config boundary is the fix;
// the gate now also fails closed, so a value that reaches it anyway asks rather
// than approving. Neither alone is enough: without this one, a typo is a silent
// policy change; without that one, any future caller that skips validation is a
// silent policy change again.
func ValidateMode(mode string) error {
	for _, m := range InteractiveModes {
		if mode == m {
			return nil
		}
	}
	return fmt.Errorf("agent.mode %q is not one of %s", mode, strings.Join(InteractiveModes, ", "))
}

// ConfigFromMap builds the agent section, with the upstream defaults.
func ConfigFromMap(m map[string]any) *Config {
	return &Config{
		SystemTemplate:             config.String(m, "system_template", ""),
		InstanceTemplate:           config.String(m, "instance_template", ""),
		MaxConsecutiveFormatErrors: config.Int(m, "max_consecutive_format_errors", 3),
		OutputPath:                 config.String(m, "output_path", ""),
		Mode:                       config.String(m, "mode", "confirm"),
		WhitelistActions:           config.StringSlice(m, "whitelist_actions"),
		ConfirmExit:                config.Bool(m, "confirm_exit", true),
		ContextWindow:              config.Int(m, "context_window", 0),
		CompactEnabled:             config.Bool(m, "compact_enabled", true),
		CompactThresholdRatio:      config.Float(m, "compact_threshold_ratio", 0.9),
		CompactKeepTokens:          config.Int(m, "compact_keep_tokens", 40000),
		CompactSummaryMaxTokens:    config.Int(m, "compact_summary_max_tokens", 16384),
		MaxParallelCalls:           config.Int(m, "max_parallel_calls", 0),
	}
}

// Dump is the "agent" section of the trajectory config.
func (c *Config) Dump() map[string]any {
	outPath := any(nil)
	if c.OutputPath != "" {
		outPath = c.OutputPath
	}
	return map[string]any{
		"system_template":               c.SystemTemplate,
		"instance_template":             c.InstanceTemplate,
		"max_consecutive_format_errors": c.MaxConsecutiveFormatErrors,
		"output_path":                   outPath,
		"mode":                          c.Mode,
		"whitelist_actions":             c.WhitelistActions,
		"confirm_exit":                  c.ConfirmExit,
		"context_window":                c.ContextWindow,
		"compact_enabled":               c.CompactEnabled,
		"compact_threshold_ratio":       c.CompactThresholdRatio,
		"compact_keep_tokens":           c.CompactKeepTokens,
		"compact_summary_max_tokens":    c.CompactSummaryMaxTokens,
		"max_parallel_calls":            c.MaxParallelCalls,
	}
}

// Hooks is the user-in-the-loop seam: nil means yolo with no prompts, which is
// DefaultAgent's behaviour. Each method returns either nil (proceed), or an
// *model.Interrupt whose messages the loop appends.
type Hooks interface {
	// BeforeExecute runs before any action is executed. Returning an
	// interrupt skips the whole batch.
	BeforeExecute(ctx context.Context, actions []model.Action, tvars map[string]any) error
	// OnSubmit is asked whether the run really ends here; an interrupt (a new
	// task, or a mode switch) continues the run instead.
	OnSubmit(ctx context.Context, exitMsg model.Message) error
	// OnLimits went away with the budgets it existed to lift (§5.14). It is not
	// replaced here on purpose: a hook nothing can call is still a method every
	// implementation has to answer for, and there were four of them.
	// NextHumanCommand asks the user for a command to run instead of querying
	// the model; ok is false whenever the mode is not "human".
	NextHumanCommand(ctx context.Context) (command string, ok bool)
	// OnMessage is called for every message appended to the history, which is
	// where an interactive front end prints things.
	OnMessage(m model.Message)
	// OnInterrupt turns a cancelled turn into the messages that record it.
	OnInterrupt(ctx context.Context) error
}

// CallOpener is the optional half of Hooks: it tells a front end that one of the
// actions it announced has its result file open, and where that file is.
//
// Optional for the same reason Issuer is on the UI side: the half that wants it is
// a window that streams a running command's output into the card that call was
// issued into, and a front end that prints text has nothing to do with the path.
// The loop opens the slot before executing, so this is the only moment the
// information exists to hand over - after the call there is only the result.
type CallOpener interface {
	OnCallOpened(action model.Action, path string)
}

// Recorder is the session store's write side. One call records one thing;
// every method is called from the agent goroutine only. *session.Session
// satisfies it structurally; agent never imports session, only model, so
// storage can be dropped or replaced without touching the loop.
type Recorder interface {
	// RecordMessage appends one turn and returns its number.
	RecordMessage(m model.Message) (turn int64, err error)
	RecordProgress(cost float64, nCalls int) error
	// OpenToolCall acquires action idx of msg - whose row and transcript slot
	// the recording of the issuing turn already allocated - and returns the
	// call with its result file open for writing.
	OpenToolCall(msg model.Message, idx int, action model.Action) (model.ToolCall, error)
	// CompleteToolCall closes the call's row with the outcome the loop
	// measured on the closed file, including the byte offset of the model's
	// view.
	CompleteToolCall(c model.ToolCall, out model.ToolOutcome) error
}

// Loop is the DefaultAgent equivalent.
type Loop struct {
	Cfg   *Config
	Model model.Model
	Env   env.Environment
	// CommandBudget is how long a foreground command is waited on before it is
	// moved to the background. Set it from the environment's configured
	// timeout so there is one number; zero means env.DefaultTimeout. Keeping it
	// here rather than reading it off the Environment is deliberate: the
	// interface has no timeout to expose, and a container environment's idea of
	// a sane wait should not have to widen the contract.
	CommandBudget time.Duration
	// Jobs is the registry backgrounded commands are handed to. nil means this
	// run cannot background anything - a test harness, or a front end that has
	// not wired one up - and `background: true` then comes back as a reported
	// failure rather than a command that silently ran in the foreground.
	Jobs  *job.Manager
	Hooks Hooks
	// Recorder persists the transcript; nil turns persistence off, but never
	// the capture: the loop still writes each command's output to a scratch
	// file it reads back.
	Recorder Recorder
	// Tools is the set this session was advertised, and the authority for what
	// its own prompts may say about them (§5.6). Zero means the top-level set.
	// A subagent gets model.SubagentTools, which is how "no spawn tool for
	// children" reaches the instructions rather than only the request body: a
	// capability that is hidden from the array but discussed in the prose is a
	// capability the model will try to use and be refused for.
	Tools model.ToolSet
	// Origin names this session to a human when a question about it is asked
	// somewhere else. It is empty for a top-level session and the subagent's
	// chosen name for a child, and it reaches prompts through TemplateVars as
	// `asker` rather than through a new argument on Hooks, because the seam
	// already carries the loop's own variables (decision 13).
	//
	// Carried rather than derived is the point: qwen's `subagentId` exists because
	// ids from different layers cannot be compared to tell own work from nested,
	// and a prompt that cannot say who is waiting is how a human approves a
	// subagent's command believing it was their own (§5.5).
	Origin string
	// Spawner is the host seam the subagent tools need: the loop is not allowed
	// to know what a session is, in the same way it is not allowed to know what
	// a store is (Recorder) or what a terminal looks like (Hooks). nil means
	// this loop cannot spawn, and the tools say so rather than failing quietly -
	// they are not even advertised to it, since a loop built without a Spawner
	// gets the subagent tool set (§5.6).
	Spawner Spawner
	// Handler receives streamed reasoning/content while Model.Query runs.
	Handler *model.Handler
	Logger  *slog.Logger

	// messages is the transcript the conversation is built from. It is
	// unexported because the field being public was the bug (A-4): the loop
	// owns this slice, every write happens on the agent goroutine, and
	// `agent.go` has said for as long as it existed that reading it from
	// another goroutine races - a convention the type system could not enforce
	// and a second reader duly violated, which is what turned `go test -race
	// ./agent/` red. Outside the loop, read Messages().
	messages []model.Message
	Cost     float64
	NCalls   int

	consecutiveFormatErrors int
	start                   time.Time
	resumed                 []model.Message
	extraVars               map[string]any
	mu                      sync.Mutex
	cancelTurn              context.CancelFunc
	// paused is non-nil while the session is held at a step boundary; closing it
	// releases the wait. pauseMu guards the pointer only - a waiter holds its own
	// copy, so a Pause racing an Unpause cannot strand a second waiter on a
	// channel nobody will close.
	pauseMu sync.Mutex
	paused  chan struct{}
	running bool
	snap    Snapshot
	pending Snapshot
	runCtx  context.Context
	queued  []model.Message
	// work is the "the harness has armed a step" signal; see work.go. Lazily
	// created and mu-guarded, because a Loop is built by struct literal in
	// several places and a nil channel would silently never fire.
	work chan struct{}
	// scratchDir holds the temp result files of a run without a Recorder;
	// empty otherwise. It must live until Run returns, not merely until its
	// turn is rendered, because the trajectory written after every step
	// reads the result files back.
	scratchDir string

	// Compaction state (see compaction.go). Everything except manualCompact
	// belongs to the agent goroutine; manualCompact is the UI's cross-goroutine
	// request, guarded by mu.
	manualCompact  bool
	compactFails   int
	autoCompactOff bool
	overflowBudget int
	lastCtxTokens  int
}

// scratchToolCall is the no-store fallback: an anonymous temp file in the
// run's scratch directory. Number stays 0, which is the "not recorded"
// sentinel the loop and CompleteToolCall both respect.
func (l *Loop) scratchToolCall(action model.Action) (model.ToolCall, error) {
	f, err := os.CreateTemp(l.scratchDir, "*.log")
	if err != nil {
		return model.ToolCall{}, err
	}
	return model.ToolCall{Path: f.Name(), File: f, Action: action}, nil
}

// Resume seeds the history from the session store before Run. The stored
// transcript already carries the system and instance messages, so Run will not
// render or append them again; Cost and NCalls are seeded by the caller from
// the session row.
func (l *Loop) Resume(msgs []model.Message) { l.resumed = msgs }

// callContext applies the model's own budget for one command. The deadline
// travels on the context because that is how env.Local is told to prefer a
// caller's number over its default, which keeps the Environment signature free
// of a timeout argument a container environment would have to imitate blindly.
func (l *Loop) callContext(ctx context.Context, action model.Action) (context.Context, context.CancelFunc) {
	if action.TimeoutS <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(action.TimeoutS)*time.Second)
}

// runForeground is one command with the model's own budget applied and released.
func (l *Loop) runForeground(ctx context.Context, action model.Action, sink io.Writer) (env.Result, error) {
	cctx, cancel := l.callContext(ctx, action)
	defer cancel()
	return l.Env.Execute(cctx, action.Command, "", sink)
}

// runCommand executes one action, deciding what happens when it outruns its
// budget: the command becomes a job rather than a corpse.
//
// Both cases go through the job manager, and that is the load-bearing part.
// Waiting and running have to be separate things before the deadline arrives,
// because env's handler kills the process group when its context expires - so a
// design that ran the command normally and "promoted" it afterwards would be
// promoting something it had already killed. With the manager holding the run,
// expiring the wait means stopping the wait and keeping the run.
//
// detached reports that the caller handed off, which means the result file is
// still open for writing and the observation is the handoff note, not a tail.
func (l *Loop) runCommand(ctx context.Context, action model.Action, tc model.ToolCall, sink io.Writer) (res env.Result, detached bool, err error) {
	if l.Jobs == nil {
		if action.Background {
			// Refuse rather than obey the letter of the request and break its
			// intent: running a 30-second command in the foreground after being
			// told not to wait for it is worse than an error, because the model
			// would have no way to tell the two apart.
			return noRegistryResult(), false, nil
		}
		// No registry, so nothing can be promoted: the environment's own
		// deadline is the only bound, and it still ends the command.
		r, err := l.runForeground(ctx, action, sink)
		return r, false, err
	}
	run, err := l.Jobs.Launch(job.Spec{
		Command: action.Command, Label: action.Description,
		TCNO: tc.Number, File: tc.File, Path: tc.Path,
	})
	if err != nil {
		return refusedObservation(err), false, nil
	}

	if action.Background {
		return l.handoff(run, action, "started in the background")
	}

	budget := l.budget()
	if action.TimeoutS > 0 {
		budget = time.Duration(action.TimeoutS) * time.Second
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-run.Done():
		return run.Result(), false, nil
	case <-ctx.Done():
		// A user interruption ends the run too: the command was asked for by a
		// turn that the human just cancelled, and leaving it burning would be
		// the abandoned-job failure this design exists to prevent. Promotion is
		// for a budget running out, not for a person running out of patience.
		_ = run.Kill()
		return env.Result{}, false, ctx.Err()
	case <-timer.C:
		return l.handoff(run, action, fmt.Sprintf(
			"still running after %s", budget.Truncate(time.Second)))
	}
}

// jobsObservation answers the jobs tool by writing the registry listing into the
// call's own result file. That is deliberate: a session with two hundred
// finished jobs then gets mini's normal truncate-and-spill treatment instead of a
// special case, and the model gets a path it already knows how to page through.
func (l *Loop) jobsObservation(action model.Action, sink io.Writer) env.Result {
	if l.Jobs == nil {
		return noRegistryResult()
	}
	list, err := l.Jobs.List()
	if err != nil {
		return env.Result{ReturnCode: 1, ExceptionInfo: err.Error()}
	}
	want := action.Command
	running := 0
	var b strings.Builder
	for _, j := range list {
		if want != "" && j.ID != want {
			continue
		}
		if j.State == session.JobRunning {
			running++
		}
		fmt.Fprintf(&b, "%s  %s  %s", j.ID, j.State, jobAge(j))
		if j.ExitCode != nil {
			fmt.Fprintf(&b, "  exit %d", *j.ExitCode)
		}
		if why := jobWhy(l, j); why != "" {
			fmt.Fprintf(&b, "  why=%s", why)
		}
		fmt.Fprintf(&b, "\n    out=%s", j.OutputPath)
		if size, age, ok := jobFileStat(j.OutputPath); ok {
			fmt.Fprintf(&b, "  (%s, last write %s)", size, age)
		}
		if j.Label != "" {
			fmt.Fprintf(&b, "\n    label=%s", j.Label)
		}
		if j.Command != "" {
			fmt.Fprintf(&b, "\n    cmd=%s", oneLine(j.Command, 200))
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		if want != "" {
			return env.Result{ReturnCode: 1,
				View: fmt.Sprintf("no job %s in this session\n", want)}
		}
		return env.Result{ReturnCode: 0, View: "no jobs in this session. A command that outlives " +
			"its wait, or one started with background: true, appears here with its id and output path.\n"}
	}
	fmt.Fprintf(&b, "%d job(s), %d running. Read a log with tail -n %d <path>; end one with job_kill.\n",
		strings.Count(b.String(), "\n    out="), running, model.MaxViewLines)
	if sink != nil {
		_, _ = io.WriteString(sink, b.String())
	}
	return env.Result{ReturnCode: 0}
}

// killObservation ends jobs. It is the only way to do that, and the reason is
// structural rather than a permission: the interpreter is in-process, so a
// command may be holding several external children at once (a pipeline, a
// `&`, make -j, a loop) and none of them is addressable as "the job" from
// inside a shell. Cancellation reaches all of them, because the run owns the
// context every command it started was signalled through.
// spawnObservation answers an `agent` call the way a backgrounded bash call is
// answered: with the job's id and where its output is going, and no result,
// because there is not one yet. That is the contract the tool description sells
// and the reason the row exists before the report does.
func (l *Loop) spawnObservation(ctx context.Context, action model.Action, sink io.Writer) env.Result {
	if l.Spawner == nil {
		// Unreachable for a session assembled by host - a loop without a
		// Spawner is not offered the tool - but a hand-built Loop could get
		// here, and "this harness cannot spawn" is a truer answer than a panic.
		return refusedObservation(errors.New("subagent spawning is not available in this session"))
	}
	name, _ := action.Args["name"].(string)
	prompt, _ := action.Args["prompt"].(string)
	if strings.TrimSpace(name) == "" || strings.TrimSpace(prompt) == "" {
		return refusedObservation(errors.New("agent needs both a short 'name' and the full 'prompt' for the subagent"))
	}
	reply, err := l.Spawner.Spawn(ctx, SpawnRequest{Name: name, Prompt: prompt})
	if err != nil {
		fmt.Fprintf(sink, "refused: %v\n", err)
		return refusedObservation(err)
	}
	fmt.Fprintf(sink, "started subagent %q as job %s, output at %s\n", reply.Name, reply.JobID, reply.OutputPath)
	fmt.Fprintf(sink, "it runs in its own session; its report arrives as a background result when it finishes.\n")
	fmt.Fprintf(sink, "to steer it or ask it for more, use job_send with this id.\n")
	// Pending, not returncode 0 - the same lesson the handoff path already
	// records (see the comment on `detached` above). A freshly spawned subagent
	// has done nothing yet, and a 0 here is read by every front end as "this
	// call finished successfully", which would close the card on a conversation
	// that has not produced a line.
	return env.Result{
		Pending: true,
		Extra: map[string]any{
			"job_id": reply.JobID, "job_output_path": reply.OutputPath,
			"subagent": reply.Name,
		},
	}
}

// sendObservation answers `job_send`. Success is a bare ok: the message is not
// an result the model waits on, and inventing a shape for "it was delivered"
// would be one more thing to describe in the tool.
func (l *Loop) sendObservation(ctx context.Context, action model.Action, sink io.Writer) env.Result {
	if l.Spawner == nil {
		return refusedObservation(errors.New("this session cannot speak to subagents"))
	}
	id, _ := action.Args["id"].(string)
	text, _ := action.Args["text"].(string)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(text) == "" {
		return refusedObservation(errors.New("job_send needs an 'id' (from the jobs tool) and the 'text' to send"))
	}
	if err := l.Spawner.Send(ctx, id, text); err != nil {
		fmt.Fprintf(sink, "refused: %v\n", err)
		return refusedObservation(err)
	}
	fmt.Fprintf(sink, "delivered to %s\n", id)
	return env.Result{ReturnCode: 0}
}

func (l *Loop) killObservation(action model.Action, sink io.Writer) env.Result {
	if l.Jobs == nil {
		return noRegistryResult()
	}
	all := action.Bool("all")
	ids := []string{}
	switch {
	case all:
		list, err := l.Jobs.List()
		if err != nil {
			return env.Result{ReturnCode: 1, ExceptionInfo: err.Error()}
		}
		for _, j := range list {
			if j.State == session.JobRunning {
				ids = append(ids, j.ID)
			}
		}
	case action.Command != "":
		ids = append(ids, action.Command)
	default:
		return env.Result{ReturnCode: 1,
			View: "job_kill needs an id from the jobs tool, or all: true\n"}
	}

	var b strings.Builder
	failed := 0
	for _, id := range ids {
		run, err := l.Jobs.Get(id)
		if err != nil {
			// No live handle. It may be a row this process never started -
			// after a restart mini cannot prove a pid is still the job's, and
			// guessing is how you kill somebody else's recycled process.
			row, rerr := l.Jobs.Session.GetJob(id)
			switch {
			case rerr != nil:
				failed++
				fmt.Fprintf(&b, "%s: no such job in this session\n", id)
			case row.State == session.JobOrphaned:
				failed++
				fmt.Fprintf(&b, "%s: already orphaned when mini restarted; its process is outside this run's ownership and was not signalled\n", id)
			default:
				fmt.Fprintf(&b, "%s: already finished (%s)\n", id, row.State)
			}
			continue
		}
		if err := run.Kill(); err != nil {
			failed++
			fmt.Fprintf(&b, "%s: %v\n", id, err)
			continue
		}
		row, _ := run.Row()
		fmt.Fprintf(&b, "%s: killed\n", id)
		if row != nil && row.Command != "" {
			fmt.Fprintf(&b, "  stopped: %s\n", oneLine(row.Command, 120))
		}
	}
	if len(ids) == 0 {
		b.WriteString("no running jobs to kill\n")
	}
	code := 0
	if failed > 0 && failed == len(ids) {
		code = 1
	}
	if sink != nil {
		_, _ = io.WriteString(sink, b.String())
	}
	return env.Result{ReturnCode: code}
}

// wakeLimit caps how much one boundary reports. It is a batching rule for
// message size, not a limit on work: firings past the batch stay queued and are
// delivered at the following boundaries, so no job's result is ever refused or
// lost. Eight matches the default job
// cap, and it keeps a pathological queue from turning one message into a log
// dump; anything left over is still in the queue, claimed by nobody, and comes
// back at the next boundary.
const wakeLimit = 8

// jobWake drains the firing queue into the message the design promises: one
// appended user message at a step boundary, bounded, holding one delivery section
// per firing, and never an edit to anything already in history.
//
// The batch is what the shape is for. Every ending still owed at this boundary
// goes into the same message, each in its own labelled section, so eight jobs
// that finished during one turn cost one turn of attention rather than eight
// messages and eight holes in the transcript. That is only readable because a
// delivery is self-contained - id, exit, path, counts, guidance - so the sections
// concatenate in any order and any one of them survives being quoted back or
// clipped without losing which job it reported.
//
// It returns zero or one message - an empty slice when there is nothing to say,
// because a wake that says nothing costs a message, a turn of attention, and a
// permanent hole in the transcript that a resumed run has to reason around.
//
// This replaces asking the model to call a wait tool. Results arrive on their
// own, so the only thing the model has to decide is whether it has work left: if
// all that remains is waiting for a job, it should stop, and the harness will
// bring it back with the outcome. That is why the queue is durable and claimed
// only on delivery - a job that ends after a submission is still here at the next
// boundary, in this run or the next one.
func (l *Loop) jobWake() []model.Message {
	if l.Jobs == nil {
		return nil
	}
	fire, err := l.Jobs.ClaimFirings(wakeLimit)
	if err != nil {
		// A queue that cannot be read must not end the run: the events are still
		// there and the next boundary tries again.
		l.logger().Error("jobs: could not drain the wake queue", "err", err)
		return nil
	}
	if len(fire) == 0 {
		return nil
	}
	var b strings.Builder
	if len(fire) == 1 {
		b.WriteString("A background job ended while you were working on something else.\n")
	} else {
		fmt.Fprintf(&b, "%d background jobs ended while you were working on something else.\n", len(fire))
	}
	// The pending set is concatenated into this one message rather than appended
	// as one message per job: the model sees every ending in a single turn, and
	// the transcript keeps one harness-authored hole instead of one per job. Each
	// record carries the facts the others do not - id, exit, path, counts - which
	// is what lets a section be read on its own, quoted back, or survive a front
	// end clipping the message, without losing which job it was about.
	for _, f := range fire {
		row := f.Job
		b.WriteString(wakeSection(row.ID))
		b.WriteString(jobDelivery(&row))
	}
	b.WriteString("\n")
	if still := l.Jobs.StillRunning(); still > 0 {
		fmt.Fprintf(&b, "%d other job(s) still running: call jobs to list them or job_kill to stop one. \n", still)
		b.WriteString("Do not poll their logs - you will be told when each one ends.\n")
	} else {
		b.WriteString("Nothing else is running in this session.\n")
	}
	return []model.Message{{
		"role":    "user",
		"content": b.String(),
		// Marked as harness-authored, so the front ends show it as feedback
		// rather than something the human typed, and a resumed run can tell the
		// two apart without parsing prose.
		"extra": map[string]any{
			"interrupt_type": "JobUpdate",
			"job_updates":    len(fire),
		},
	}}
}

// jobDelivery is one ended job's half of a wake: the record of an ending that
// happened while the model was busy elsewhere, and nothing else. It carries the
// facts that decide the next move - which job, how it ended, how long it ran,
// where its bytes are, and how many of them there are - and then the guidance
// for reading the file itself.
//
// It does not carry the output. An earlier version inlined a subagent's report
// body up to the observation window, on the argument that making the parent pay
// a turn to fetch the answer handed back the cost the child was meant to save.
// That reasoning held only while a wake was a one-job event. A boundary now
// reports the whole pending set at once, so applying it eight times over turns
// the design's cheapest message into eight bodies, competes with the live
// transcript for the context window, and spends it on jobs the model may not
// care about - and the one kind it never applied to, a bash job, was already
// just a pointer. So the delivery is uniform: a pointer for every kind, with the
// counts that make reading it a decision instead of a guess. "142 lines, 9 KB"
// says the whole thing fits in one read; "410 thousand lines, 900 MB" says do not
// read this, grep it. The body is not lost, only moved to the one place that can
// hold all of it: the file, at the path in the record.
//
// It returns a string rather than writing into a builder, because a delivery is
// now a unit that gets concatenated - see jobWake - and a function with a
// builder in its signature cannot be joined to anything. A monitor firing will
// want the same record for the same reason, which is why it is a function at all
// and not a few lines inside the wake.
func jobDelivery(j *session.JobRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "job %s ended: %s after %s", j.ID, j.State, jobAge(*j))
	switch {
	case j.ExitCode != nil && *j.ExitCode != 0:
		fmt.Fprintf(&b, " (exit %d)", *j.ExitCode)
	case j.ExitCode != nil:
		b.WriteString(" (exit 0)")
	case j.State == session.JobKilled:
		b.WriteString(" (killed; no exit code - the run was interrupted, not finished)")
	}
	if j.ExitReason != "" && j.ExitReason != "killed on request" {
		fmt.Fprintf(&b, "\n  reason=%s", oneLine(j.ExitReason, 120))
	}
	if j.Label != "" {
		fmt.Fprintf(&b, "\n  label=%s", j.Label)
	}
	if j.Command != "" {
		fmt.Fprintf(&b, "\n  cmd=%s", oneLine(j.Command, 200))
	}
	info := jobOutputStat(j.OutputPath)
	fmt.Fprintf(&b, "\n  out=%s", j.OutputPath)
	if !info.Exists {
		b.WriteString("  (no file at that path: the job ended before writing anything)")
	} else {
		fmt.Fprintf(&b, "  (%s)", info.String())
	}
	fmt.Fprintf(&b, "\n%s\n", jobReadGuidance(j, info))
	return b.String()
}

// jobOutputInfo is what a job's output file amounts to. Bytes and lines are
// exact as far as the scan got - Short says when it did not - and Age is the
// file's own, so an empty log, a stalled one, and a finished one are three
// different answers rather than one path and a hope.
type jobOutputInfo struct {
	Exists bool
	Lines  int64
	Bytes  int64
	Age    string
	Short  error
}

func (i jobOutputInfo) String() string {
	if !i.Exists {
		return "no output file"
	}
	s := fmt.Sprintf("%s, %s, last write %s", countOf(i.Lines, "line"), countOf(i.Bytes, "byte"), i.Age)
	if i.Short != nil {
		s += fmt.Sprintf(", counts stop here: %v", i.Short)
	}
	return s
}

// jobOutputStat counts a job's output bytes and lines.
//
// Only a delivery calls it, and that is the whole reason it exists next to
// jobFileStat rather than replacing it: the listing runs over every row in the
// registry, where a line count would be a full scan per job over logs that are
// routinely hundreds of megabytes, and jobFileStat's comment says so. A wake
// counts at most wakeLimit files, and it counts them because the model is about
// to decide whether to read them - which is exactly when the line count stops
// being a luxury and becomes the answer to the question being asked.
//
// The scan is streaming over a fixed buffer, so no file is ever held in memory
// and the cost is one pass of disk bandwidth. Lines counts newline-terminated
// lines plus a final partial one, so a report that does not end in a newline
// still reports the line it has; that can be one above what `wc -l` says, and
// the figure is meant to size a read, not to agree with a shell builtin.
//
// A read that fails halfway reports what was confirmed and names the error in
// Short rather than passing off a truncated scan as a complete count.
func jobOutputStat(path string) jobOutputInfo {
	var info jobOutputInfo
	if path == "" {
		return info
	}
	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()
	info.Exists = true
	if st, err := f.Stat(); err == nil {
		info.Age = time.Since(st.ModTime()).Truncate(time.Second).String()
	} else {
		info.Age = "unknown"
	}
	const chunk = 256 << 10
	buf := make([]byte, chunk)
	var last byte
	for {
		n, err := f.Read(buf)
		if n > 0 {
			info.Bytes += int64(n)
			info.Lines += int64(bytes.Count(buf[:n], jobNewline))
			last = buf[n-1]
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				info.Short = err
			}
			break
		}
	}
	if info.Bytes > 0 && last != '\n' {
		info.Lines++
	}
	return info
}

var jobNewline = []byte{'\n'}

// jobReadGuidance is the "so how do I look at this" line, and it is why the
// counts are in the record at all: with no body inline, the guidance is the only
// way in, so it has to be actionable without a guess beforehand.
//
// Two things vary, both from what the file is. A build log ends with its answer,
// so it is tailed; a subagent's report leads with it, so it is headed - the
// excerpt rule the inline body used to apply on the model's behalf now shapes the
// command instead of costing a message. And a file small enough for one read is
// called what it is, because instructing the model to tail twelve lines is worse
// than telling it the whole output is right there.
func jobReadGuidance(j *session.JobRow, info jobOutputInfo) string {
	if !info.Exists {
		return "  nothing to read yet: there is no output file at that path."
	}
	if info.Bytes == 0 && info.Short == nil {
		return fmt.Sprintf("  the job wrote nothing: %s is empty.", j.OutputPath)
	}
	if info.Short != nil {
		// The counts are what the scan confirmed before it failed, so the one
		// thing the guidance must not do is promise a clean read of a file the
		// harness could not count. The model can still try; it should know the
		// size figures came with an asterisk.
		return fmt.Sprintf("  the output could not be counted (%v) - read it with: tail -n %d %s",
			info.Short, model.MaxViewLines, j.OutputPath)
	}
	if info.Lines <= model.MaxViewLines && info.Bytes <= model.MaxViewChars {
		return fmt.Sprintf("  read it with: cat %s   (the whole output is %s, so one read holds it all)",
			j.OutputPath, countOf(info.Lines, "line"))
	}
	read, end := fmt.Sprintf("tail -n %d", model.MaxViewLines), "last"
	if j.Kind == session.KindSubagent {
		// A report leads with its answer; a log ends with it.
		read, end = fmt.Sprintf("sed -n '1,%dp'", model.MaxViewLines), "first"
	}
	return fmt.Sprintf("  read it with: %s %s   (or grep -n PATTERN %s for the %s %d lines)",
		read, j.OutputPath, j.OutputPath, end, model.MaxViewLines)
}

// wakeSection is the seam between two delivered jobs in one message. A boundary
// reports the whole pending set, and a concatenation without seams reads as one
// long job that mentions several ids, so each section is introduced by a rule
// naming the job it opens. Naming it twice - in the separator and again in the
// first line of the record - is deliberate: a section quoted back, or clipped by
// a front end, still says which job it belongs to.
func wakeSection(id string) string {
	return "\n" + strings.Repeat("-", 20) + " job " + id + " " + strings.Repeat("-", 20) + "\n"
}

// lastJobSummary picks out the most recent ended job for the "nothing is
// running" answer, so a model that waited after its job finished learns the
// difference between "already done" and "never started".
func lastJobSummary(list []session.JobRow) (id, ago string) {
	if len(list) == 0 {
		return "-", "unknown"
	}
	first := list[0]
	ago = jobAge(first)
	return first.ID, ago
}

func jobStateOf(list []session.JobRow) string {
	if len(list) == 0 {
		return "none"
	}
	return list[0].State
}

// jobWhy is the answer to "why is this still here". For a live run the manager
// knows whether the model backgrounded it or it outran its budget; for one that
// has already ended, or an orphan from before a restart, the row's own reason is
// all that survives, and the difference is worth having in the listing rather
// than inferred.
func jobWhy(l *Loop, j session.JobRow) string {
	if run, err := l.Jobs.Get(j.ID); err == nil {
		if r := run.Reason(); r != "" {
			return r
		}
	}
	return j.ExitReason
}

// jobAge is how long the run has been around, ending at its end time.
func jobAge(j session.JobRow) string {
	end := j.StartedAt
	if j.EndedAt != nil {
		end = *j.EndedAt
	}
	d := time.Duration(end-j.StartedAt) * time.Millisecond
	if d < 0 {
		d = 0
	}
	return d.Truncate(100 * time.Millisecond).String()
}

// jobFileStat reports a job log's size and how long since its last write, which
// is the cheap version of the stall question: a running job that has not written
// in twenty minutes is worth looking at, and nothing here needs to read the file
// to say so. Line counts are deliberately not reported - that would be a full
// scan per listing over logs that are routinely hundreds of megabytes, and
// `wc -l` on the path is exact and local to the one job being asked about.
func jobFileStat(path string) (size, age string, ok bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", "", false
	}
	return humanBytes(st.Size()), time.Since(st.ModTime()).Truncate(time.Second).String(), true
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// countOf renders a count with its unit and its plural, because a record that
// says "1 lines" on the first job of a batch is the kind of detail that makes a
// model wonder whether the number is trustworthy.
func countOf(n int64, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// oneLine keeps a multi-line command from breaking the listing's shape.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func noRegistryResult() env.Result {
	return env.Result{
		ReturnCode:    -1,
		ExceptionInfo: "this run has no job registry: restart mini with sessions enabled",
		Extra:         map[string]any{"exception_type": "JobsUnavailable"},
	}
}

// budget is the foreground wait, honouring whatever the configuration set for
// the environment: a user who raised environment.timeout to two minutes expects
// long commands to be *waited on* for two minutes, and promoting them at 30
// seconds anyway would make that setting lie in a new and more confusing way.
func (l *Loop) budget() time.Duration {
	if l.CommandBudget > 0 {
		return l.CommandBudget
	}
	return env.DefaultTimeout
}

// handoff detaches a run and composes what the model reads instead of output:
// the job id, the path its bytes are accumulating at, and a recipe that fits the
// observation window - so the first read of a job log cannot waste a turn on a
// re-truncation.
func (l *Loop) handoff(run *job.Run, action model.Action, why string) (env.Result, bool, error) {
	if _, err := l.Jobs.Detach(run, why); err != nil {
		// The run has already burned its whole foreground wait to get here -
		// that is the only reason a handoff on this path happens - so a
		// bookkeeping failure must not destroy it. Manager.Start kills for
		// exactly the opposite reason: nothing had run yet. Here the work is
		// real, its bytes are on disk, and the process is already in its own
		// group, so the honest answer is to say out loud that the job is
		// untracked and let the human and the model decide. KillAll at session
		// close still reaps it, so this leaks nothing that outlives the run.
		l.logger().Error("jobs: could not register a promoted run; leaving it burning and untracked",
			"id", run.ID(), "err", err)
		return untrackedObservation(run, err), false, nil
	}
	path := run.OutputPath()
	var b strings.Builder
	fmt.Fprintf(&b, "%s as job %s and is still running in its own process group.\n", why, run.ID())
	fmt.Fprintf(&b, "output is appended to %s\n", path)
	fmt.Fprintf(&b, "read it with: tail -n %d %s   (or grep -n PATTERN %s; prefer narrowing to cat)\n",
		model.MaxViewLines, path, path)
	fmt.Fprintf(&b, "the job keeps running when this turn ends, and is killed when the session closes.\n")
	if action.Background {
		b.WriteString("do not wait on it: continue, and it will be here when you look.")
	} else {
		b.WriteString("This is not a timeout - nothing was killed and the command does not need re-running.")
	}
	// No OutputPath or OutputBytes on the result: the observation template only
	// offers a spill pointer when the call's file was measured, and a file that
	// is still growing has no final size to name. Pending, not returncode 0:
	// the command has not finished, and a 0 here is what made a handed-off call
	// look like a succeeded one in every front end that reads it.
	return env.Result{Pending: true, View: b.String(),
		Extra: map[string]any{"job_id": run.ID(), "job_output_path": path}}, true, nil
}

// refusedObservation is a job the registry would not start. It comes back as an
// observation rather than an error so a model that receives it as a crash still
// learns something it can act on.

// untrackedObservation is a run that is burning and has no row: it cannot be
// listed, woken, or stopped through the registry, so the note says that plainly,
// names the output path that does exist, and leaves the decision with the reader
// rather than quietly killing forty seconds of someone's build.
func untrackedObservation(run *job.Run, err error) env.Result {
	return env.Result{
		// Pending, not a return code: the process is running and has no
		// outcome, and a -1 here would read as a crash that never happened.
		Pending: true,
		View: fmt.Sprintf(
			"The command was promoted out of the foreground and is still running, but the registry could not open a row for it, so `jobs` will not list it and its ending will not be reported.\n"+
				"id: %s\noutput is appended to %s\n"+
				"It will be killed when the session closes. Stop it now with job_kill id %s if you do not want it to keep running.\nstore said: %v",
			run.ID(), run.OutputPath(), run.ID(), err),
		ExceptionInfo: err.Error(),
		Extra:         map[string]any{"exception_type": "JobUntracked", "job_id": run.ID(), "job_output_path": run.OutputPath()},
	}
}
func refusedObservation(err error) env.Result {
	return env.Result{
		ReturnCode:    -1,
		ExceptionInfo: err.Error(),
		Extra:         map[string]any{"exception_type": "JobRefused"},
	}
}

// QueueMessage appends a user message to the history before the next model
// call. This is the GUI's steering input: the user can always type, even while
// a command is running, and nothing races with the agent goroutine.
func (l *Loop) QueueMessage(m model.Message) {
	l.mu.Lock()
	l.queued = append(l.queued, m)
	l.mu.Unlock()
}

func (l *Loop) drainQueued() []model.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.queued
	l.queued = nil
	return q
}

// Messages returns a copy of the transcript, safe to read from any goroutine.
//
// It copies rather than handing back the slice because the loop appends to the
// original on its own goroutine: an aliased read would be a race on the header
// at best and on the array behind it at worst. Callers that walk the history are
// already O(n) over it, so the copy costs what the walk costs and buys the one
// guarantee the exported field never had.
func (l *Loop) Messages() []model.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]model.Message, len(l.messages))
	copy(out, l.messages)
	return out
}

// setHistory replaces the transcript under the same lock Messages() takes. It is
// for the two moments that legitimately rebuild the whole history - a fresh Run
// starting empty and a resume seeding itself from the store - and not for
// appending: use addMessages for that, or the recorder and the hooks get skipped.
func (l *Loop) setHistory(msgs []model.Message) {
	l.mu.Lock()
	l.messages = msgs
	l.mu.Unlock()
}

// Snapshot is the immutable view of the loop's progress that a UI may read from
// another goroutine. Reading Loop.messages / NCalls / Cost directly races with
// the agent goroutine, so the loop publishes this at every safe point instead.
type Snapshot struct {
	Calls          int
	Cost           float64
	ModelTokens    int
	ModelTokPerSec float64
	// CtxTokens estimates the projected context at the last model call, the
	// number behind the footer's ctx% and the compaction trigger.
	CtxTokens int
}

func (l *Loop) publish() {
	l.mu.Lock()
	l.snap = l.pending
	l.mu.Unlock()
	if l.Recorder != nil {
		// A killed run still has spend/call counters to resume from.
		if err := l.Recorder.RecordProgress(l.Cost, l.NCalls); err != nil {
			l.logger().Error("session store: could not record progress", "err", err)
		}
	}
}

// Snapshot returns the most recently published progress view.
func (l *Loop) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

// Running reports whether a Run is in progress. The GUI uses it to decide
// whether esc cancels the turn or closes a dialog.
func (l *Loop) Running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// CancelTurn aborts the command that is running right now, the way Ctrl+C
// interrupts a step in Python: the interruption message lands in the history
// and the loop goes on. A cancelled parent context ends the whole run instead.
func (l *Loop) CancelTurn() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancelTurn != nil {
		l.cancelTurn()
	}
}

// Pause holds this session at its next step boundary. Nothing already in flight
// is disturbed: the turn finishes, its observations land, and the loop waits
// before the next model call. This is §5.7's other remedy beside kill - a
// background session that should stop taking gate slots without losing its
// transcript, its jobs or its place in the conversation.
func (l *Loop) Pause() {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	if l.paused == nil {
		l.paused = make(chan struct{})
		l.logger().Info("session paused; it stops at its next step boundary")
	}
}

// Unpause releases a held session, which carries on at the step it had reached. A
// session that was never paused is untouched, so the control is idempotent and a
// release that raced a pause cannot close somebody else's channel.
func (l *Loop) Unpause() {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	if l.paused != nil {
		close(l.paused)
		l.paused = nil
		l.logger().Info("session resumed")
	}
}

// Paused reports whether the session is being held, including the window between
// asking and actually stopping at the boundary.
func (l *Loop) Paused() bool {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	return l.paused != nil
}

// waitUnpaused parks at a step boundary until the session is released or the run
// ends. Cancellation wins: a held session must be escapable by the same esc that
// ends any other turn, or pause becomes a way to make a run unkillable.
func (l *Loop) waitUnpaused(ctx context.Context) error {
	l.pauseMu.Lock()
	release := l.paused
	l.pauseMu.Unlock()
	if release == nil {
		return nil
	}
	select {
	case <-release:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Loop) setCancelTurn(cancel context.CancelFunc) {
	l.mu.Lock()
	l.cancelTurn = cancel
	l.mu.Unlock()
}

// SpawnRequest is one subagent as the model asked for it. Name is the
// human-facing label and Prompt the whole task, because a subagent starts with
// no context at all (docs/parallel-sessions.md §5, ruling 1).
type SpawnRequest struct {
	Name   string
	Prompt string
}

// SpawnReply is what the tool call returns immediately: the job the spawner can
// already list, kill and send to. The report is not here - it arrives later, at
// a step boundary, like any other background result (§5.10).
type SpawnReply struct {
	JobID      string
	OutputPath string
	Name       string
}

// Spawner is the host's side of the subagent tools. The loop calls it from the
// agent goroutine only, exactly like the rest of the seams.
type Spawner interface {
	// Spawn opens the child session, registers it as a job of this session, and
	// returns as soon as it is running.
	Spawn(ctx context.Context, req SpawnRequest) (SpawnReply, error)
	// Send delivers text to one of this session's agent jobs: a user message at
	// the next boundary if it is running, a resume-and-deliver if it has stopped
	// (§9.3). A job of any other kind is refused.
	Send(ctx context.Context, jobID, text string) error
}

// ErrRunInProgress is what Run returns when the loop is already driving a
// conversation. It names the alternative because the caller who hits it almost
// always wanted the alternative: to say something to a running session.
var ErrRunInProgress = errors.New("agent: a run is already in progress on this session; use QueueMessage to speak to it")

// beginRun claims the loop for one run, reporting whether it was free. The test
// and the set are one critical section - checking Running() and then setting it
// is the race, not a narrower version of it.
func (l *Loop) beginRun() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return false
	}
	l.running = true
	return true
}

// tools is the advertised set, defaulting to the top-level one so a Loop built
// by hand - every test, and any future caller that did not think about kinds -
// renders the prompt it always has.
func (l *Loop) tools() model.ToolSet {
	if len(l.Tools) > 0 {
		return l.Tools
	}
	return model.TopLevelTools()
}

func (l *Loop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// TemplateVars merges the config dump, the environment's variables, the
// model's variables, the run statistics and the run kwargs, in that precedence
// order (agents/default.py:get_template_vars).
func (l *Loop) TemplateVars(kwargs ...map[string]any) map[string]any {
	vars := config.Merge(
		l.Cfg.Dump(),
		l.Env.TemplateVars(),
		l.Model.TemplateVars(),
		map[string]any{
			"n_model_calls":   l.NCalls,
			"model_cost":      l.Cost,
			"elapsed_seconds": int(time.Since(l.start).Seconds()),
			"total_tokens":    0,
			// The observation window, handed to the prompts. Both the system
			// prompt and the observation template state how much of a command's
			// output the model can ever see, and a number written down twice is
			// a number that drifts: the shipped example advertised reading 200
			// lines through a 100-line window, which guarantees the re-read
			// comes back truncated again. Interpolate the consts instead.
			"view_lines": model.MaxViewLines,
			"view_chars": model.MaxViewChars,
			// The tools, generated from the one set rather than written again.
			// `subagent` is derived from the absence of the spawn tool rather
			// than stored beside it, so the two cannot disagree: visibility is
			// the mechanism, and this makes it the same fact in three places.
			"tool_list": l.tools().Listing(),
			"has_jobs":  l.tools().Has(model.ToolJobs),
			"has_agent": l.tools().Has(model.ToolAgent),
			"asker":     l.Origin,
			"subagent":  !l.tools().Has(model.ToolAgent),
		},
		l.extraVars,
	)
	for _, kw := range kwargs {
		vars = config.Merge(vars, kw)
	}
	return vars
}

func (l *Loop) render(name, template string) (string, error) {
	// Strict, unlike the observation templates: this context is fixed, so a key
	// the template asks for and the loop does not supply is a mistake in one of
	// the two, and it has to be loud. A system prompt that silently lost its
	// tool listing would otherwise be diagnosed as a bad model (A-7).
	return tpl.RenderStrict(name, template, l.TemplateVars())
}

func (l *Loop) addMessages(msgs ...model.Message) []model.Message {
	// The lock covers the append and nothing else. Holding it across the
	// Recorder and Hooks calls below would deadlock: OnMessage reaches the front
	// ends, and the window's OnMessage path takes l.mu itself to read Snapshot.
	l.mu.Lock()
	l.messages = append(l.messages, msgs...)
	l.mu.Unlock()
	if l.Recorder != nil {
		for _, m := range msgs {
			// The recorder stamps the turn number into the message's own
			// extra. Persistence failure must not kill the agent.
			if _, err := l.Recorder.RecordMessage(m); err != nil {
				l.logger().Error("session store: could not record turn", "err", err)
			}
		}
	}
	if l.Hooks != nil {
		for _, m := range msgs {
			l.Hooks.OnMessage(m)
		}
	}
	return msgs
}

// Run executes the task: render the prompts, then loop step() until an exit
// message lands at the end of the history. It returns the last message's extra
// (exit_status, submission), or the error that ended the run.
func (l *Loop) Run(ctx context.Context, task string, kwargs ...map[string]any) (map[string]any, error) {
	// Single-flight, and it refuses rather than queues. Running() has always
	// been advisory - the front ends use it to grey out a button - but a Loop
	// owns one history, one cost ledger and one cancel slot, so a second
	// concurrent Run would interleave turns into the first one's transcript and
	// both would believe they held the cancel. With one session per process the
	// UI's discipline was enough; with subagents and a multi-session window the
	// loop is reachable from goroutines that were never going to coordinate, so
	// the guarantee belongs here (docs/parallel-sessions.md §3.5.6).
	//
	// Refusing rather than blocking is the half that comes from mini having a
	// steering channel: QueueMessage is the supported way to reach a live loop
	// (agent.go:676-678), so a second caller has made a mistake the loop can
	// name, not a request to be queued. zero blocks for exactly this reason and
	// can afford to - its ACP sessions have nothing else to say with.
	if !l.beginRun() {
		return nil, ErrRunInProgress
	}
	l.setRunCtx(ctx)
	defer func() {
		l.mu.Lock()
		l.running = false
		l.cancelTurn = nil
		l.mu.Unlock()
	}()
	if l.Recorder == nil {
		// A store-backed run must not litter $TMPDIR: the real result files
		// outlive the run by design. The scratch directory lives for the
		// whole run because the trajectory written after every step reads the
		// result files back.
		l.scratchDir, _ = os.MkdirTemp("", "mini-tcr-")
		defer func() { _ = os.RemoveAll(l.scratchDir) }()
	}
	l.start = time.Now()
	l.extraVars = map[string]any{"task": task}
	// Compaction state is per run: the breaker and the reactive budget
	// restart, and a stale /compact request from a previous run must not
	// fire.
	l.compactFails = 0
	l.autoCompactOff = false
	l.overflowBudget = maxOverflowCompactions
	l.manualCompact = false
	l.checkWindowParams()
	for _, kw := range kwargs {
		for k, v := range kw {
			l.extraVars[k] = v
		}
	}
	l.setHistory(nil)

	// Resume seeds the history from the session store. The stored transcript
	// already carries the system and instance messages, so Run must not
	// render or append them again; the stored system prompt wins even when
	// the config changed under it, because the database is the source of
	// truth for a resumed run.
	if l.resumed != nil {
		l.setHistory(l.resumed)
		if task != "" {
			l.addMessages(model.Message{
				"role":    "user",
				"content": "The user added a new task: " + task,
				"extra":   map[string]any{"interrupt_type": "UserNewTask"},
			})
		}
	} else {
		system, err := l.render("system_template", l.Cfg.SystemTemplate)
		if err != nil {
			return nil, fmt.Errorf("rendering system_template: %w", err)
		}
		instance, err := l.render("instance_template", l.Cfg.InstanceTemplate)
		if err != nil {
			return nil, fmt.Errorf("rendering instance_template: %w", err)
		}
		l.addMessages(
			model.Message{"role": "system", "content": system},
			model.Message{"role": "user", "content": instance},
		)
	}

	for {
		// A step boundary. A held session stops here, with the turn it was in
		// allowed to finish: abandoning a command mid-write is what CancelTurn is
		// for, and pause means the opposite - keep everything, take no next step.
		if err := l.waitUnpaused(ctx); err != nil {
			l.save()
			return nil, err
		}
		// Anything a background job reported since the last one
		// is appended here, as one message, before the next model call - the
		// only place this design writes to history besides an observation, and
		// deliberately not in the middle of a turn: a wake is an appended
		// message like every other observation, never an edit (the cache-prefix
		// rule docs/compaction-research.md §4 puts first).
		l.addMessages(l.jobWake()...)

		stepCtx, cancel := context.WithCancel(ctx)
		l.setCancelTurn(cancel)
		err := l.step(stepCtx)
		cancel()
		l.setCancelTurn(nil)
		var in *model.Interrupt
		switch {
		case err == nil:
			l.consecutiveFormatErrors = 0
		case errors.As(err, &in) && in.Kind == model.KindFormatError:
			l.handleFormatError(in)
		case errors.As(err, &in):
			// The boundary where the agent has just decided it can stop. A job
			// that ended during the last turn would otherwise be recorded after
			// the run's final message - which is to say, never read by anyone.
			// Delivered first, so the exit row stays the last thing in history
			// and the run's return value keeps its shape.
			l.addMessages(l.jobWake()...)
			l.addMessages(in.Messages...)
		default:
			l.handleUncaught(err)
			l.save()
			return nil, err
		}
		l.save()
		if l.lastRole() == "exit" {
			// Ending a turn is the model saying *it* has nothing to do, not
			// that nothing remains to be done. Those were conflated, and the
			// conflation is what made a wait tool look necessary - and made
			// submitting with live jobs destroy them. A job that has finished
			// without its result reaching the conversation is an obligation of
			// the harness, and discharging it is what drives the next step.
			if l.deliverOwed() {
				continue
			}
			if n := l.liveJobCount(); n > 0 {
				l.logger().Info("the task ended with jobs still running; they will be reaped at session close",
					"running", n)
			}
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	last := model.Message{}
	if len(l.messages) > 0 {
		last = l.messages[len(l.messages)-1]
	}
	extra, _ := last["extra"].(map[string]any)
	return extra, nil
}

// deliverOwed discharges the harness's side of the job state machine and reports
// whether the run should take another step.
//
// A job moves started -> running -> finished -> delivered. Backgrounded and
// foreground jobs differ in only one respect: a foreground call returns its tool
// result *at* the delivered transition, because the waiting call is the
// delivery; a backgrounded call returns during running, and reaches delivered
// later, as a user message the harness authors at a step boundary. "Later" can
// mean after the model has stopped talking, which is the whole reason this
// function exists: the conversation is not finished while a result is owed, and
// nothing about that requires waiting. Undelivered work is not a thing to block
// on, it is a thing to say - so the loop takes the step that says it.
//
// Termination is structural rather than a timeout. Firings are claimed as they
// are delivered, so each continuation retires one obligation and cannot repeat
// itself; jobs only finish a finite number of times; and a job that is still
// *running* is not an owed result at all. That last case is why the run can end
// beside a server that never exits: the model stopped working, nothing is
// outstanding, and the session keeps the job - with its card live and its
// result queued for the next run - until a human closes it or the model stops it
// with job_kill.
func (l *Loop) deliverOwed() bool {
	if l.Jobs == nil {
		return false
	}
	w := l.jobWake()
	if len(w) == 0 {
		return false
	}
	l.addMessages(w...)
	l.save()
	return true
}

// liveJobCount is the run's view of the registry, and nil-safe: a Loop without
// a registry has no jobs by definition.
func (l *Loop) liveJobCount() int {
	if l.Jobs == nil {
		return 0
	}
	return l.Jobs.StillRunning()
}

// handleFormatError bills the failed call, counts it, and terminates the run
// once the consecutive-error threshold is reached.
func (l *Loop) handleFormatError(in *model.Interrupt) {
	if len(in.Messages) > 0 {
		l.Cost += config.Float(model.Extra(in.Messages[0]), "cost", 0)
	}
	l.consecutiveFormatErrors++
	l.addMessages(in.Messages...)
	if lim := l.Cfg.MaxConsecutiveFormatErrors; lim > 0 && lim <= l.consecutiveFormatErrors {
		l.addMessages(model.Message{
			"role":    "exit",
			"content": model.KindRepeatedFormatErr,
			"extra":   map[string]any{"exit_status": model.KindRepeatedFormatErr, "submission": ""},
		})
	}
}

// handleUncaught appends the exit message the Python agent records before
// letting the exception escape.
func (l *Loop) handleUncaught(err error) {
	l.addMessages(model.Message{
		"role":    "exit",
		"content": err.Error(),
		"extra": map[string]any{
			"exit_status":   exitStatusName(err),
			"submission":    "",
			"exception_str": err.Error(),
			"traceback":     string(debug.Stack()),
		},
	})
}

func exitStatusName(err error) string {
	var api *model.APIError
	if errors.As(err, &api) {
		return "APIError"
	}
	return "RuntimeError"
}

func (l *Loop) lastRole() string {
	if len(l.messages) == 0 {
		return ""
	}
	role, _ := l.messages[len(l.messages)-1]["role"].(string)
	return role
}

// step queries the model and executes what it asked for.
func (l *Loop) step(ctx context.Context) error {
	msg, err := l.Query(ctx)
	if err != nil {
		var in *model.Interrupt
		if errors.As(err, &in) {
			return err
		}
		if ctx.Err() != nil {
			// Ctrl+C during the model call, like Python's KeyboardInterrupt
			// inside step(): an interruption rather than a crash. The hook is
			// handed the run context, so it can still prompt for a comment.
			return l.interrupted(err)
		}
		return err
	}
	return l.ExecuteActions(ctx, msg)
}

// interrupted turns a cancellation into the interruption message the Python
// agent appends, or leaves it alone when nobody is listening.
func (l *Loop) interrupted(cause error) error {
	if l.Hooks == nil {
		return cause
	}
	return l.Hooks.OnInterrupt(l.runContext())
}

// runContext returns the context of the whole run (never a cancelled turn
// context), falling back to Background when no run is active.
//
// Both sides of runCtx take l.mu. The field used to be touched only from the
// agent goroutine, which is why it escaped the mutex, but a second reader now
// exists by design: anything watching a live loop from another goroutine - a
// parent polling a subagent, a front end with more than one session - asks this
// question while Run is writing the answer.
func (l *Loop) runContext() context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runCtx != nil {
		return l.runCtx
	}
	return context.Background()
}

func (l *Loop) setRunCtx(ctx context.Context) {
	l.mu.Lock()
	l.runCtx = ctx
	l.mu.Unlock()
}

// Query asks the model for the next turn, after checking the limits (which
// happen before the call counter is incremented, so a rejected query costs
// nothing).
func (l *Loop) Query(ctx context.Context) (model.Message, error) {
	// Steering typed while the previous turn ran lands here, so the model sees
	// it on the turn it is about to answer.
	l.addMessages(l.drainQueued()...)
	if l.Hooks != nil {
		// Human mode: the user's command replaces the model's turn, before any
		// limit check, exactly as InteractiveAgent.query does.
		if command, ok := l.Hooks.NextHumanCommand(ctx); ok {
			msg := model.Message{
				"role":    "user",
				"content": "User command: \n```bash\n" + command + "\n```",
				"extra":   map[string]any{"actions": []model.Action{{Command: command}}},
			}
			l.addMessages(msg)
			return msg, nil
		}
	}
	l.NCalls++
	// Project (and compact first if the projection outgrew its trigger) right
	// here: every step passes this point, which is what makes the trigger
	// tool-loop-mid-run instead of only between user prompts.
	proj, est := l.prepareContext(ctx)
	// Publish before the call so a UI streaming this turn labels it with the
	// step it belongs to, instead of the previous one.
	l.pending.CtxTokens = est
	l.publish()
	msg, err := l.Model.Query(ctx, proj, l.Handler)
	if err != nil && model.IsContextOverflow(err) && l.overflowBudget > 0 {
		// The reactive path: the server itself said the prompt did not fit.
		// Compact once (forced, breaker aside) and retry this call once; if it
		// still does not fit, the error is the run's to report (pi's one-shot
		// recovery, budgeted across the run because sushi does not evict the
		// cache between attempts).
		l.overflowBudget--
		l.logger().Warn("server rejected the prompt as too large; forcing compaction", "err", err, "remaining_attempts", l.overflowBudget)
		if l.compact(ctx, proj, est, true) {
			proj = model.ProjectContext(l.messages)
			est, _ = model.ContextTokens(proj)
			l.lastCtxTokens = est
			l.pending.CtxTokens = est
			msg, err = l.Model.Query(ctx, proj, l.Handler)
		}
	}
	if err != nil {
		return nil, err
	}
	l.Cost += config.Float(model.Extra(msg), "cost", 0)
	l.pending = progressOf(l.NCalls, l.Cost, msg)
	// progressOf built a fresh Snapshot; the estimate is not in a reply's
	// usage until this call's is folded in next step.
	l.pending.CtxTokens = est
	l.publish()
	l.addMessages(msg)
	return msg, nil
}

// progressOf lifts the numbers a UI wants out of a model reply, so the GUI
// never has to touch the message maps from another goroutine.
func progressOf(calls int, cost float64, msg model.Message) Snapshot {
	snap := Snapshot{Calls: calls, Cost: cost}
	extra, _ := msg["extra"].(map[string]any)
	resp, _ := extra["response"].(map[string]any)
	if usage, ok := resp["usage"].(map[string]any); ok {
		snap.ModelTokens = int(config.Float(usage, "total_tokens", 0))
	}
	if timings, ok := resp["timings"].(map[string]any); ok {
		snap.ModelTokPerSec = config.Float(timings, "predicted_per_second", 0)
	}
	return snap
}

func exitMessage(kind, submission string) model.Message {
	return model.Message{
		"role":    "exit",
		"content": kind,
		"extra":   map[string]any{"exit_status": kind, "submission": submission},
	}
}

// ExecuteActions runs the model's commands and appends the observations.
// A turn with no tool calls is the model putting its tools down: the prompt
// contract says that response - and only that response - ends the task, and
// its text becomes the submission. Turns that do run commands follow the
// Python InteractiveAgent's ordering: observations are recorded in the
// finally block, after asking the user whether the run really ends.
//
// The commands of one turn are a batch, and the batch runs concurrently. That is
// sound because the environment is stateless by contract - every action gets its
// own shell, which is why `cd` and `export` do not persist - so a call cannot
// depend on a sibling's side effects, and the prompt says so in those terms.
// Concurrency is confined to the waiting: opening a call, announcing it to the
// front end, completing its row and appending to the transcript all stay on this
// goroutine, in issue order, so the Hooks and Recorder contracts - both
// documented as agent-goroutine-only - are untouched by this function's speedup.
//
// What the model and the transcript see is unchanged: one observation per call,
// in the order the calls were issued, whatever order they finished in.
func (l *Loop) ExecuteActions(ctx context.Context, msg model.Message) error {
	actions := model.ActionsOf(msg)
	var results []env.Result

	if len(actions) == 0 {
		submission, _ := msg["content"].(string)
		exitMsg := model.Message{
			"role":    "exit",
			"content": submission,
			"extra":   map[string]any{"exit_status": model.KindSubmitted, "submission": submission},
		}
		if l.Hooks == nil {
			return &model.Interrupt{Kind: model.KindSubmitted, Messages: []model.Message{exitMsg}}
		}
		if err := l.Hooks.OnSubmit(ctx, exitMsg); err != nil {
			return err
		}
		return &model.Interrupt{Kind: model.KindSubmitted, Messages: []model.Message{exitMsg}}
	}

	if l.Hooks != nil {
		if err := l.Hooks.BeforeExecute(ctx, actions, l.TemplateVars()); err != nil {
			l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
			return err
		}
	}

	// ---- Phase 1: open and announce, serially, in issue order. ----
	//
	// The batch is prepared here rather than inside the workers on purpose, and
	// it is what keeps this change from widening any contract. Opening a call is
	// a store write and announcing it is a hook call, and both are documented as
	// agent-goroutine work: Recorder says so at its definition (agent.go:172),
	// and OnCallOpened's whole promise to the window is that the card a call is
	// issued into already exists when the call is announced. Fan either of those
	// out and every front end inherits an ordering problem it cannot see coming.
	//
	// Doing them up front costs a serial database write per call, which is
	// microseconds against the command that follows, and buys the expensive half
	// of the loop a clean run at concurrency. The rows also already exist:
	// recording the issuing turn allocated the ledger row and the transcript slot
	// for every call it carried (session/calls.go), so this opens what was
	// promised rather than inventing N promises at once.
	opens := make([]model.ToolCall, len(actions))
	for i, action := range actions {
		// One call row and one result file per action. The file is the sink
		// even when the database row failed: the observation must always
		// have something to read.
		var tc model.ToolCall
		if l.Recorder != nil {
			var err error
			tc, err = l.Recorder.OpenToolCall(msg, i, action)
			if err != nil {
				l.logger().Error("session store: could not record tool call", "err", err)
				tc = model.ToolCall{}
			}
		}
		if tc.File == nil {
			var err error
			if tc, err = l.scratchToolCall(action); err != nil {
				tc = model.ToolCall{}
			}
		}
		if o, ok := l.Hooks.(CallOpener); ok && tc.Path != "" {
			o.OnCallOpened(action, tc.Path)
		}
		opens[i] = tc
	}

	// ---- Phase 2: execute them all at once. ----
	runs := l.runCallsConcurrently(ctx, actions, opens)

	// ---- Phase 3: settle serially, in issue order. ----
	//
	// results[i] belongs to actions[i]. That pairing is not a convention here, it
	// is what model.formatObservations zips on, what the stored transcript is
	// built from, and what the model is told next turn - so it is filled by index
	// and never appended, and a command that finishes early cannot be credited
	// with a slower sibling's exit code, cwd or result file. Execution order is
	// whatever the machine manages; the reported order is the order issued.
	//
	// Every slot starts as the padding value, because "unfinished" and "finished
	// with exit 0" must not share a representation.
	results = make([]env.Result, len(actions))
	for i := range results {
		results[i] = model.NotExecutedResult()
	}
	// cause is the interruption the turn ends with, if any: the first action that
	// reported one, by issue order, which is the one a serial run would have
	// reached first and therefore the honest thing to name.
	var cause error

	for i := range actions {
		run, tc := runs[i], opens[i]
		// Stop the shell's writes and measure the file once, before any branch
		// returns: this is the only place tail arithmetic happens. Closing
		// first keeps the fd from leaking on the cancellation path; res is
		// returned by value, so these fields survive an early return.
		//
		// A detached run skips all of it. The job is still appending to that
		// file, so the loop must not close it, and the observation is the
		// handoff rather than a tail of a log that has no end yet - the model
		// is told the path and how to read it, and reads it when it wants.
		//
		// A call that never started still closes here, and must: phase 1 opened
		// every file in the batch, so a batch cut short by a cancellation would
		// otherwise leak one descriptor per command that never ran.
		if tc.File != nil && !run.detached {
			_ = tc.File.Sync()
			_ = tc.File.Close()
			run.res.OutputPath = tc.Path
			run.res.OutputBytes = sizeOf(tc.Path)
			view, offset, elided, verr := model.ViewOf(tc.Path)
			run.res.View, run.res.ViewOffset, run.res.ElidedLines, run.res.Truncated = view, offset, elided, offset > 0
			if verr != nil {
				run.res.ViewError = verr.Error()
			}
		}
		// Context cancellation: a user interruption, not an observation. The
		// call row keeps status='started' - the honest record that the command
		// never finished - and the slot keeps the padded "action was not
		// executed" observation, so the transcript stays legal either way.
		//
		// It does not stop here any more. Serially, an interruption ended the
		// batch and everything after it was simply never run; concurrently, the
		// commands issued alongside it are already in flight, and the choice is
		// between throwing away results that exist and reporting them. So the
		// loop settles every sibling, records the cause, and interrupts after
		// the batch: a cancelled turn still tells the model what its other
		// commands actually did.
		if run.err != nil {
			if cause == nil {
				cause = run.err
			}
			continue
		}
		// A detached run leaves its call row 'started', which is the truth: the
		// call did not complete, it handed off. The job's own row carries the
		// outcome, and closing the call row with a return code the command has
		// not produced yet would put a number in the ledger that nobody measured.
		if l.Recorder != nil && !run.detached {
			if err := l.Recorder.CompleteToolCall(tc, model.ToolOutcome{
				Bytes:      run.res.OutputBytes,
				ViewOffset: run.res.ViewOffset,
				ViewElided: run.res.ElidedLines,
				ReturnCode: run.res.ReturnCode,
				CWD:        run.res.CWD,
			}); err != nil {
				l.logger().Error("session store: could not record tool call outcome", "err", err)
			}
		}
		if tc.Number != 0 {
			if run.res.Extra == nil {
				run.res.Extra = map[string]any{}
			}
			run.res.Extra["tcno"] = tc.Number
			run.res.Extra["tcr_offset"] = run.res.ViewOffset
			run.res.Extra["tcr_bytes"] = run.res.OutputBytes
			run.res.Extra["elided_lines"] = run.res.ElidedLines
		}
		results[i] = run.res
	}

	l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
	if cause != nil {
		if l.Hooks == nil {
			return cause
		}
		return l.interrupted(cause)
	}
	return nil
}

// callRun is one action's share of a batch: what its execution produced, and the
// two facts only the worker knows. It is a value in a slice the workers index
// into, so no lock is involved - each action owns its own element and the
// elements are never touched by a goroutine that does not own one.
type callRun struct {
	res env.Result
	// detached reports a handoff: the run outlived the call, so its result file
	// stays open and belongs to the job, not the loop.
	detached bool
	// err is the caller's context ending, which is the only failure a command
	// execution reports as a Go error and the only one that interrupts a turn.
	err error
}

// DefaultMaxParallelCalls bounds how many of one batch run at once, and matches
// model.DefaultGate so the two concurrent halves of a turn - commands and
// subagent requests - are held to the same number.
const DefaultMaxParallelCalls = 4

// parallelBound is how many workers a batch of n actions gets: the configured
// ceiling when there is one, unbounded when the config says so, and never more
// than there is work for.
func (l *Loop) parallelBound(n int) int {
	max := DefaultMaxParallelCalls
	if l.Cfg != nil && l.Cfg.MaxParallelCalls != 0 {
		max = l.Cfg.MaxParallelCalls
	}
	if max < 0 || max > n {
		return n
	}
	return max
}

// runCallsConcurrently executes every action of a batch and does not return
// until nothing the batch started is still being written to. It hands back one
// callRun per action, in issue order, and it is the whole of the parallel half:
// no store write, no hook, no template variable and no transcript append happens
// here, because those all stay on the goroutine that owns the loop.
//
// What makes this safe rather than merely fast is that the work being fanned out
// owns nothing in common. env.Local is stateless by design - every action is a
// fresh shell, which is why `cd` and `export` do not persist - and Execute builds
// a new interpreter per call, reading l.CWD/l.Env/l.Timeout without writing them.
// Each command writes its own result file, holds its own call row keyed by
// (turn, idx), and is launched as its own job under an id derived from its own
// tcno, so Manager cannot see two of them collide. job.Manager and Run are
// documented and built for concurrent use, and the foreground wait holds no lock
// at all: it is a bare select over run.Done, the caller's context and the budget
// timer, so N waits are N goroutines parked on channels and nothing else.
//
// Cancelling the turn now reaches every command in the batch at once rather than
// the one it happened to be waiting on. That is the intended reading of a cancel,
// and it is why nothing here is left detached by accident: a run that outlives
// the batch is a run whose file the loop must not close, which the detached flag
// already says.
func (l *Loop) runCallsConcurrently(ctx context.Context, actions []model.Action, opens []model.ToolCall) []callRun {
	runs := make([]callRun, len(actions))
	// A bounded fan-out. The bound is not for the machine's sake so much as the
	// model's: a batch is whatever the turn asked for, and a turn that asked for
	// forty commands should get forty processes eventually rather than forty at
	// once and an unresponsive box.
	sem := make(chan struct{}, l.parallelBound(len(actions)))
	var wg sync.WaitGroup
	for i := range actions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// A batch already cancelled - by a sibling's interruption, or by a
			// human who pressed stop before this one got a worker - is not
			// started. Its slot keeps the padding, which is the truth: this
			// action was never executed.
			if ctx.Err() != nil {
				runs[i] = callRun{err: ctx.Err()}
				return
			}
			runs[i] = l.runCall(ctx, actions[i], opens[i])
		}(i)
	}
	wg.Wait()
	return runs
}

// runCall executes one action into its call's result file. It returns a value
// rather than writing anything down precisely so that the caller stays the only
// place any state changes hands.
func (l *Loop) runCall(ctx context.Context, action model.Action, tc model.ToolCall) callRun {
	var sink io.Writer
	if tc.File != nil {
		sink = tc.File
	}
	var run callRun
	switch action.ToolName() {
	case model.ToolJobs:
		run.res = l.jobsObservation(action, sink)
	case model.ToolJobKill:
		run.res = l.killObservation(action, sink)
	case model.ToolAgent:
		run.res = l.spawnObservation(ctx, action, sink)
	case model.ToolJobSend:
		run.res = l.sendObservation(ctx, action, sink)
	default:
		run.res, run.detached, run.err = l.runCommand(ctx, action, tc, sink)
	}
	return run
}

// Serialize builds the trajectory document (rule 12).
func (l *Loop) Serialize(extra ...map[string]any) map[string]any {
	lastExtra := map[string]any{}
	if len(l.messages) > 0 {
		// Read without creating: Extra() would add an "extra" key to the
		// messages that never had one, and the trajectory would gain a field
		// mid-document.
		if e, ok := l.messages[len(l.messages)-1]["extra"].(map[string]any); ok {
			lastExtra = e
		}
	}
	messages := withRawOutputs(l.messages)
	agentData := map[string]any{
		"info": map[string]any{
			"model_stats": map[string]any{
				"instance_cost": l.Cost,
				"api_calls":     l.NCalls,
			},
			"config": map[string]any{
				"agent":      l.Cfg.Dump(),
				"agent_type": "mini/agent.Loop",
			},
			"mini_version": "mini-go/" + Version,
			"exit_status":  stringOr(lastExtra["exit_status"]),
			"submission":   stringOr(lastExtra["submission"]),
		},
		"messages":          messages,
		"trajectory_format": "mini-swe-agent-1.1",
	}
	return config.Merge(append([]map[string]any{agentData, l.Model.Serialize(), l.Env.Serialize()}, extra...)...)
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}

// withRawOutputs copies the history and gives every observation turn an
// extra["raw_output"] holding its result file's complete bytes, which is what
// the trajectory format has always carried and the only reason anything reads
// a whole result file. Turns with no file (the "action was not executed"
// padding) get "", exactly as before. Live messages are never mutated: the
// copies own the key, so nothing else in the process sees tool text in memory.
func withRawOutputs(msgs []model.Message) []model.Message {
	out := make([]model.Message, 0, len(msgs))
	for _, m := range msgs {
		if !model.IsToolResult(m) {
			out = append(out, m)
			continue
		}
		cp := model.Message{}
		for k, v := range m {
			cp[k] = v
		}
		src, _ := m["extra"].(map[string]any)
		extra := make(map[string]any, len(src)+1)
		for k, v := range src {
			extra[k] = v
		}
		raw := ""
		if p, _ := extra["tcr_path"].(string); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				raw = string(b)
			}
		}
		extra["raw_output"] = raw
		cp["extra"] = extra
		out = append(out, cp)
	}
	return out
}

func sizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

// Save writes the trajectory; the parent directory is created on demand.
func (l *Loop) Save(path string, extra ...map[string]any) (map[string]any, error) {
	data := l.Serialize(extra...)
	if path == "" {
		return data, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return data, err
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return data, fmt.Errorf("encoding trajectory: %w", err)
	}
	// Written after every step, so a crash or a kill leaves the last complete
	// step on disk.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return data, err
	}
	return data, os.Rename(tmp, path)
}

func (l *Loop) save() {
	if _, err := l.Save(l.Cfg.OutputPath); err != nil {
		l.logger().Error("could not save trajectory", "path", l.Cfg.OutputPath, "err", err)
	}
}
