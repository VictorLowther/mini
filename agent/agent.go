// Package agent runs the linear query-execute loop. The control flow began as a
// port of minisweagent/agents/default.py, which was the shortest path to a loop
// worth testing against, and it has grown past that shape: compaction, the
// session store, and background jobs each answer a problem the ported agent does
// not have. Provenance, not a ceiling.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/tpl"
)

// Version is reported as mini_version in every trajectory.
const Version = "0.1.0"

// Config is AgentConfig plus InteractiveAgentConfig. mode, whitelist_actions
// and confirm_exit are read by the interactive hooks and ignored when the loop
// has no hooks.
type Config struct {
	SystemTemplate             string
	InstanceTemplate           string
	StepLimit                  int
	CostLimit                  float64
	WallTimeLimitSeconds       int
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
}

// ConfigFromMap builds the agent section, with the upstream defaults.
func ConfigFromMap(m map[string]any) *Config {
	return &Config{
		SystemTemplate:             config.String(m, "system_template", ""),
		InstanceTemplate:           config.String(m, "instance_template", ""),
		StepLimit:                  config.Int(m, "step_limit", 0),
		CostLimit:                  config.Float(m, "cost_limit", 3.0),
		WallTimeLimitSeconds:       config.Int(m, "wall_time_limit_seconds", 0),
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
		"step_limit":                    c.StepLimit,
		"cost_limit":                    c.CostLimit,
		"wall_time_limit_seconds":       c.WallTimeLimitSeconds,
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
	// OnLimits offers the user a chance to lift the step/cost limits. nil means
	// "limits raised, query again"; returning pending (or another interrupt)
	// ends the run with it. A wall-clock limit never calls this.
	OnLimits(ctx context.Context, cfg *Config, pending *model.Interrupt) error
	// NextHumanCommand asks the user for a command to run instead of querying
	// the model; ok is false whenever the mode is not "human".
	NextHumanCommand(ctx context.Context) (command string, ok bool)
	// OnMessage is called for every message appended to the history, which is
	// where an interactive front end prints things.
	OnMessage(m model.Message)
	// OnInterrupt turns a cancelled turn into the messages that record it.
	OnInterrupt(ctx context.Context) error
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
	// Handler receives streamed reasoning/content while Model.Query runs.
	Handler *model.Handler
	Logger  *slog.Logger

	Messages []model.Message
	Cost     float64
	NCalls   int

	consecutiveFormatErrors int
	start                   time.Time
	resumed                 []model.Message
	extraVars               map[string]any
	mu                      sync.Mutex
	cancelTurn              context.CancelFunc
	running                 bool
	snap                    Snapshot
	pending                 Snapshot
	runCtx                  context.Context
	queued                  []model.Message
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

// startJob hands the command to the job registry and writes the handoff into the
// tool call's own result file.
//
// Two files, deliberately: the call's file keeps the harness's note and is
// finished here like any other observation, while the job appends to its own
// for as long as it lives. Rewriting the call's file when the job ended would
// break the append-at-EOF rule that keeps every stored tcr_offset true, and a
// model reading the transcript later sees the handoff exactly as it was told it.
func (l *Loop) startJob(action model.Action, tc model.ToolCall, sink io.Writer) (env.Result, error) {
	if l.Jobs == nil {
		return env.Result{
			ReturnCode:    -1,
			ExceptionInfo: "this run has no job registry: run the command without background, or restart mini with sessions enabled",
			Extra:         map[string]any{"exception_type": "JobsUnavailable"},
		}, nil
	}
	r, err := l.Jobs.Start(job.Spec{Command: action.Command, TCNO: tc.Number})
	if err != nil {
		// A refusal is an observation, not a crash: the cap error names the
		// remedy, and the model can act on it in the next turn.
		return env.Result{
			ReturnCode:    -1,
			ExceptionInfo: err.Error(),
			Extra:         map[string]any{"exception_type": "JobRefused"},
		}, nil
	}
	path := r.OutputPath()
	res := env.Result{ReturnCode: 0, Extra: map[string]any{
		"job_id":          r.ID(),
		"job_output_path": path,
	}}
	if sink == nil {
		return res, nil
	}
	// The handoff is the only place the model learns where the bytes are going,
	// so it states the path twice: once as a fact, once inside a command it can
	// copy. The window in the recipe is the one the prompt already teaches, so a
	// first read of a job log cannot waste a turn re-truncating.
	fmt.Fprintf(sink, "started in the background as job %s; the command is running in its own process group.\n", r.ID())
	fmt.Fprintf(sink, "output is appended to %s\n", path)
	fmt.Fprintf(sink, "read it with: tail -n %d %s   (or grep -n PATTERN %s; prefer narrowing to cat)\n", model.MaxViewLines, path, path)
	fmt.Fprintf(sink, "the job keeps running when this turn ends, and is killed when the session closes.\n")
	return res, nil
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

// Snapshot is the immutable view of the loop's progress that a UI may read from
// another goroutine. Reading Loop.Messages / NCalls / Cost directly races with
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

func (l *Loop) setCancelTurn(cancel context.CancelFunc) {
	l.mu.Lock()
	l.cancelTurn = cancel
	l.mu.Unlock()
}

func (l *Loop) setRunning(v bool) {
	l.mu.Lock()
	l.running = v
	l.mu.Unlock()
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
		},
		l.extraVars,
	)
	for _, kw := range kwargs {
		vars = config.Merge(vars, kw)
	}
	return vars
}

func (l *Loop) render(name, template string) (string, error) {
	return tpl.Render(name, template, l.TemplateVars())
}

func (l *Loop) addMessages(msgs ...model.Message) []model.Message {
	l.Messages = append(l.Messages, msgs...)
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
	l.setRunning(true)
	l.runCtx = ctx
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
	l.Messages = nil

	// Resume seeds the history from the session store. The stored transcript
	// already carries the system and instance messages, so Run must not
	// render or append them again; the stored system prompt wins even when
	// the config changed under it, because the database is the source of
	// truth for a resumed run.
	if l.resumed != nil {
		l.Messages = l.resumed
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
			l.addMessages(in.Messages...)
		default:
			l.handleUncaught(err)
			l.save()
			return nil, err
		}
		l.save()
		if l.lastRole() == "exit" {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	last := model.Message{}
	if len(l.Messages) > 0 {
		last = l.Messages[len(l.Messages)-1]
	}
	extra, _ := last["extra"].(map[string]any)
	return extra, nil
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
	if len(l.Messages) == 0 {
		return ""
	}
	role, _ := l.Messages[len(l.Messages)-1]["role"].(string)
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
func (l *Loop) runContext() context.Context {
	if l.runCtx != nil {
		return l.runCtx
	}
	return context.Background()
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
	if err := l.checkLimits(); err != nil {
		var in *model.Interrupt
		errors.As(err, &in)
		if in.Kind != model.KindTimeExceeded && l.Hooks != nil {
			// A wall-clock limit cannot be lifted by raising the step/cost
			// limits, so only the other one is worth prompting for.
			if hookErr := l.Hooks.OnLimits(ctx, l.Cfg, in); hookErr != nil {
				return nil, hookErr
			}
			if err := l.checkLimits(); err != nil {
				return nil, err
			}
		} else {
			return nil, err
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
			proj = model.ProjectContext(l.Messages)
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

func (l *Loop) checkLimits() error {
	if (0 < l.Cfg.StepLimit && l.Cfg.StepLimit <= l.NCalls) || (0 < l.Cfg.CostLimit && l.Cfg.CostLimit <= l.Cost) {
		return &model.Interrupt{Kind: model.KindLimitsExceeded, Messages: []model.Message{exitMessage(model.KindLimitsExceeded, "")}}
	}
	if 0 < l.Cfg.WallTimeLimitSeconds && l.Cfg.WallTimeLimitSeconds <= int(time.Since(l.start).Seconds()) {
		return &model.Interrupt{Kind: model.KindTimeExceeded, Messages: []model.Message{exitMessage(model.KindTimeExceeded, "")}}
	}
	return nil
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
		var sink io.Writer
		if tc.File != nil {
			sink = tc.File
		}
		var res env.Result
		var err error
		if action.Background {
			res, err = l.startJob(action, tc, sink)
		} else {
			res, err = l.runForeground(ctx, action, sink)
		}
		// Stop the shell's writes and measure the file once, before any
		// branch returns: this is the only place tail arithmetic happens.
		// Closing first keeps the fd from leaking on the cancellation path;
		// res is returned by value, so these fields survive an early return.
		if tc.File != nil {
			_ = tc.File.Sync()
			_ = tc.File.Close()
			res.OutputPath = tc.Path
			res.OutputBytes = sizeOf(tc.Path)
			view, offset, elided, verr := model.ViewOf(tc.Path)
			res.View, res.ViewOffset, res.ElidedLines, res.Truncated = view, offset, elided, offset > 0
			if verr != nil {
				res.ViewError = verr.Error()
			}
		}
		if err != nil {
			// Context cancellation: a user interruption, not an observation.
			// The call row keeps status='started' - the honest record that the
			// command never finished - and its pre-allocated slot takes the
			// padded "action was not executed" observation the addMessages call
			// below produces, so the transcript stays legal either way.
			if l.Hooks == nil {
				return err
			}
			l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
			return l.interrupted(err)
		}
		if l.Recorder != nil {
			if err := l.Recorder.CompleteToolCall(tc, model.ToolOutcome{
				Bytes:      res.OutputBytes,
				ViewOffset: res.ViewOffset,
				ViewElided: res.ElidedLines,
				ReturnCode: res.ReturnCode,
				CWD:        res.CWD,
			}); err != nil {
				l.logger().Error("session store: could not record tool call outcome", "err", err)
			}
		}
		if tc.Number != 0 {
			if res.Extra == nil {
				res.Extra = map[string]any{}
			}
			res.Extra["tcno"] = tc.Number
			res.Extra["tcr_offset"] = res.ViewOffset
			res.Extra["tcr_bytes"] = res.OutputBytes
			res.Extra["elided_lines"] = res.ElidedLines
		}
		results = append(results, res)
	}

	l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
	return nil
}

// Serialize builds the trajectory document (rule 12).
func (l *Loop) Serialize(extra ...map[string]any) map[string]any {
	lastExtra := map[string]any{}
	if len(l.Messages) > 0 {
		// Read without creating: Extra() would add an "extra" key to the
		// messages that never had one, and the trajectory would gain a field
		// mid-document.
		if e, ok := l.Messages[len(l.Messages)-1]["extra"].(map[string]any); ok {
			lastExtra = e
		}
	}
	messages := withRawOutputs(l.Messages)
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
