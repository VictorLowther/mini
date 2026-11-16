package ui

import (
	"context"
	"regexp"
	"strconv"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// Interactive is the port of InteractiveAgent: it holds the mode state, the
// action whitelist and the confirmation/exit/limit prompts, and delegates all
// actual input and output to a UI.
type Interactive struct {
	UI        UI
	Cfg       *agent.Config
	Loop      *agent.Loop // read for the limits banner and the human-mode prompt
	whitelist []*regexp.Regexp
}

// New compiles the whitelist once. Go regexes are unanchored, so the prefix
// semantics of Python's re.match are restored by prepending "^" (and wrapping
// in a group, so `echo a|echo b` means either whole command).
func New(ui UI, cfg *agent.Config, l *agent.Loop) (*Interactive, error) {
	h := &Interactive{UI: ui, Cfg: cfg, Loop: l}
	for _, pattern := range cfg.WhitelistActions {
		re, err := regexp.Compile("^(?:" + pattern + ")")
		if err != nil {
			return nil, err
		}
		h.whitelist = append(h.whitelist, re)
	}
	return h, nil
}

func (h *Interactive) shouldAskConfirmation(command string) bool {
	if h.Cfg.Mode != "confirm" {
		return false
	}
	for _, re := range h.whitelist {
		if re.MatchString(command) {
			return false
		}
	}
	return true
}

// BeforeExecute implements the confirm mode.
func (h *Interactive) BeforeExecute(ctx context.Context, actions []model.Action, tvars map[string]any) error {
	ask := false
	for _, a := range actions {
		// The job tools never reach the prompt: reading the registry asks
		// nothing of anyone, and job_kill can only end work that already
		// passed the gate when it started. Asking before letting a runaway
		// be stopped would make the mitigation slower than the problem.
		if a.NeedsConfirmation() && h.shouldAskConfirmation(a.Command) {
			ask = true
			break
		}
	}
	if !ask {
		return nil
	}
	ans, err := h.UI.Ask(ctx, Question{
		Kind:    AskConfirm,
		Title:   confirmationTitle(len(actions)),
		Actions: actions,
		Cfg:     h.Cfg,
		// Whose commands these are. The child's loop puts its own name in the
		// template variables it hands to BeforeExecute, so the label travels
		// with the request instead of being looked up from an id that cannot be
		// compared across layers (decision 13).
		Origin: askerOf(tvars),
	})
	if err != nil {
		return err
	}
	if !ans.Reject {
		return nil
	}
	if ans.Text == "/u" {
		// /u: skip the batch and hand control back to the user.
		return interruptUser("Commands not executed. Switching to human mode", "UserRejection")
	}
	return interruptUser(rejectMessage(ans.Text), "UserRejection")
}

// OnSubmit implements confirm_exit: the run only ends when the user says so.
func (h *Interactive) OnSubmit(ctx context.Context, exitMsg model.Message) error {
	if !h.Cfg.ConfirmExit {
		return nil
	}
	for {
		ans, err := h.UI.Ask(ctx, Question{Kind: AskNewTask, Title: newTaskTitle, Cfg: h.Cfg})
		if err != nil {
			return err
		}
		switch {
		case ans.RePrompt:
			continue // /h, or /c on a config already in confirmation mode
		case ans.Text == "":
			// Enter is a no-op. Leaving is deliberate: ctrl-c or /quit. A
			// stray keystroke at the end of a turn must not end the run,
			// so re-ask instead.
			continue
		case ans.Text == quitCommand:
			return nil // the explicit way to agree the run is over
		case ans.Text == "/u":
			// Continue in human mode, without recording a new task.
			return interruptUser("Switched to human mode.", model.KindUserInterruption)
		case isModeCommand(ans.Text):
			continue // /y or /c: the mode changed, ask again (Python recurses)
		default:
			return interruptUser(newTaskMessage(ans.Text), "UserNewTask")
		}
	}
}

// OnLimits asks for a new step and cost limit, but only when a terminal is
// attached; an unattended run stops cleanly with LimitsExceeded.
func (h *Interactive) OnLimits(ctx context.Context, cfg *agent.Config, pending *model.Interrupt) error {
	if !h.UI.Interactive() {
		return pending
	}
	ans, err := h.UI.Ask(ctx, Question{
		Kind:      AskLimits,
		Title:     limitsBanner(cfg, h.Loop.NCalls, h.Loop.Cost),
		Cfg:       cfg,
		StepLimit: cfg.StepLimit,
		CostLimit: cfg.CostLimit,
		NCalls:    h.Loop.NCalls,
		Cost:      h.Loop.Cost,
	})
	if err != nil {
		return err
	}
	if !ans.NewLimits {
		return pending
	}
	cfg.StepLimit = ans.Steps
	cfg.CostLimit = ans.Cost
	return nil
}

// NextHumanCommand implements human mode.
func (h *Interactive) NextHumanCommand(ctx context.Context) (string, bool) {
	if h.Cfg.Mode != "human" {
		return "", false
	}
	for {
		ans, err := h.UI.Ask(ctx, Question{Kind: AskHuman, Title: promptLine(), Cfg: h.Cfg})
		if err != nil {
			return "", false
		}
		if ans.RePrompt {
			continue
		}
		if h.Cfg.Mode != "human" {
			// /y or /c switched mode: query the model instead.
			return "", false
		}
		return ans.Text, true
	}
}

// OnInterrupt turns a cancelled turn into the interruption message, after
// letting the user comment on it (Python's step() except KeyboardInterrupt).
func (h *Interactive) OnInterrupt(ctx context.Context) error {
	if ctx.Err() != nil || !h.UI.Interactive() {
		return interruptUser("Temporary interruption caught.", model.KindUserInterruption)
	}
	ans, err := h.UI.Ask(ctx, Question{Kind: AskInterrup, Title: interruptionTitle, Cfg: h.Cfg})
	text := ans.Text
	// Python treats an empty comment or a bare mode command as a temporary
	// interruption; only real prose becomes "Interrupted by user: …".
	if err != nil || ans.RePrompt || text == "" || isModeCommand(text) {
		return interruptUser("Temporary interruption caught.", model.KindUserInterruption)
	}
	return interruptUser("Interrupted by user: "+text, model.KindUserInterruption)
}

// OnMessage forwards every history addition to the UI.
func (h *Interactive) OnMessage(m model.Message) { h.UI.Message(m) }

// ParseLimitLine reads one of the numeric limit answers; empty or invalid input
// disables that limit, like entering 0.
func ParseLimitLine(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// askerOf pulls the origin label out of a loop's template variables. The loop
// owns that map, so the seam that already carries the prompt context carries the
// attribution too, rather than Hooks growing a parameter every implementation
// would have to reason about.
func askerOf(tvars map[string]any) string {
	if tvars == nil {
		return ""
	}
	if s, ok := tvars["asker"].(string); ok {
		return s
	}
	return ""
}
