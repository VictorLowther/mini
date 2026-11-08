// Package agent runs the linear query-execute loop: the port of
// minisweagent/agents/default.py (and the interactive subclass's control flow).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
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

// Loop is the DefaultAgent equivalent.
type Loop struct {
	Cfg   *Config
	Model model.Model
	Env   env.Environment
	Hooks Hooks
	// Handler receives streamed reasoning/content while Model.Query runs.
	Handler *model.Handler
	Logger  *slog.Logger

	Messages []model.Message
	Cost     float64
	NCalls   int

	consecutiveFormatErrors int
	start                   time.Time
	extraVars               map[string]any
	mu                      sync.Mutex
	cancelTurn              context.CancelFunc
	running                 bool
	snap                    Snapshot
	pending                 Snapshot
	runCtx                  context.Context
	queued                  []model.Message
}

// QueueMessage appends a user message to the history before the next model
// call. This is the TUI's steering input: the user can always type, even while
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
}

func (l *Loop) publish() {
	l.mu.Lock()
	l.snap = l.pending
	l.mu.Unlock()
}

// Snapshot returns the most recently published progress view.
func (l *Loop) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

// Running reports whether a Run is in progress. The TUI uses it to decide
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
	l.start = time.Now()
	l.extraVars = map[string]any{"task": task}
	for _, kw := range kwargs {
		for k, v := range kw {
			l.extraVars[k] = v
		}
	}
	l.Messages = nil

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
	// Publish before the call so a UI streaming this turn labels it with the
	// step it belongs to, instead of the previous one.
	l.publish()
	msg, err := l.Model.Query(ctx, l.Messages, l.Handler)
	if err != nil {
		return nil, err
	}
	l.Cost += config.Float(model.Extra(msg), "cost", 0)
	l.pending = progressOf(l.NCalls, l.Cost, msg)
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

// progressOf lifts the numbers a UI wants out of a model reply, so the pane
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

// ExecuteActions runs the model's commands and appends the observations. The
// branching follows the two Python classes it ports: DefaultAgent lets a
// submission skip the observation formatting entirely, while InteractiveAgent
// records the (padded) observations in its finally block, after asking the user
// whether the run really ends.
func (l *Loop) ExecuteActions(ctx context.Context, msg model.Message) error {
	actions := model.ActionsOf(msg)
	var results []env.Result

	if l.Hooks != nil {
		if err := l.Hooks.BeforeExecute(ctx, actions, l.TemplateVars()); err != nil {
			l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
			return err
		}
	}

	var submitMsg model.Message
	for _, action := range actions {
		res, err := l.Env.Execute(ctx, action.Command, "")
		if err != nil {
			// Context cancellation: a user interruption, not an observation.
			if l.Hooks == nil {
				return err
			}
			l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
			return l.interrupted(err)
		}
		results = append(results, res)
		if exitMsg, ok := env.SubmitMarker(res); ok {
			submitMsg = exitMsg
			break
		}
	}

	if submitMsg != nil {
		if l.Hooks == nil {
			return &model.Interrupt{Kind: model.KindSubmitted, Messages: []model.Message{submitMsg}}
		}
		err := l.Hooks.OnSubmit(ctx, submitMsg)
		l.addMessages(l.Model.FormatObservations(msg, results[:len(results)-1], l.TemplateVars())...)
		if err != nil {
			return err
		}
		return &model.Interrupt{Kind: model.KindSubmitted, Messages: []model.Message{submitMsg}}
	}

	l.addMessages(l.Model.FormatObservations(msg, results, l.TemplateVars())...)
	return nil
}

// Serialize builds the trajectory document (rule 12).
func (l *Loop) Serialize(extra ...map[string]any) map[string]any {
	lastExtra := map[string]any{}
	if len(l.Messages) > 0 {
		// Read without creating: Extra() would add an "extra" key to messages
		// the reference leaves bare.
		if e, ok := l.Messages[len(l.Messages)-1]["extra"].(map[string]any); ok {
			lastExtra = e
		}
	}
	messages := l.Messages
	if messages == nil {
		messages = []model.Message{}
	}
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
