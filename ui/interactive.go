package ui

import (
	"context"
	"regexp"

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

// New compiles the whitelist once, as whole-command matches (see whitelist.go).
func New(ui UI, cfg *agent.Config, l *agent.Loop) (*Interactive, error) {
	patterns, err := compileWhitelist(cfg.WhitelistActions)
	if err != nil {
		return nil, err
	}
	return &Interactive{UI: ui, Cfg: cfg, Loop: l, whitelist: patterns}, nil
}

// shouldAskConfirmation is the gate, and it reports why it asked.
//
// The mode test is inverted on purpose. It used to read `if Mode != "confirm" {
// return false }`, which made every value the port never defined - "Confirm",
// "confrim", "", a stray tab-swallowed YAML string - a *silent* yolo, because the
// only value that meant "ask" was spelled exactly right. Upstream could afford
// that: it declares mode as Literal["human","confirm","yolo"] and pydantic rejects
// anything else at config load. The Go port kept the comparison and dropped the
// type, so the typo became an authorization (A-2).
//
// So: only the two modes that genuinely have no one to ask skip the prompt -
// "yolo" because the human opted out, "human" because the command came *from* the
// human - and everything else asks.
func (h *Interactive) shouldAskConfirmation(command string) (bool, string) {
	switch h.Cfg.Mode {
	case "yolo", "human":
		return false, ""
	}
	v := whitelistChecks(command, h.whitelist)
	if v.Allowed {
		return false, ""
	}
	return true, v.Reason
}

// BeforeExecute implements the confirm mode.
func (h *Interactive) BeforeExecute(ctx context.Context, actions []model.Action, tvars map[string]any) error {
	// Announce the calls first, and before the prompt: "issued" is the moment a
	// card should exist, and a human deciding whether to approve something is
	// exactly when they want to see what is already running behind it.
	if is, ok := h.UI.(Issuer); ok {
		is.IssueCalls(actions)
	}
	ask := false
	why := ""
	for _, a := range actions {
		// The job tools never reach the prompt: reading the registry asks
		// nothing of anyone, and job_kill can only end work that already
		// passed the gate when it started. Asking before letting a runaway
		// be stopped would make the mitigation slower than the problem.
		if !a.NeedsConfirmation() {
			continue
		}
		if need, reason := h.shouldAskConfirmation(a.Command); need {
			ask, why = true, reason
			break
		}
	}
	if !ask {
		return nil
	}
	ans, err := h.UI.Ask(ctx, Question{
		Kind:    AskConfirm,
		Title:   confirmationTitle(len(actions), why),
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

// askWaking is Ask with the harness's own debt as a second way to be answered.
//
// A prompt is a step boundary. The session is between turns, nothing is
// streaming, the transcript is settled - which is exactly the state compaction
// waits for and the state a job result waits to be delivered into. Both waits
// here (the ready prompt and human mode) run inside Run, on the agent
// goroutine, so every drain in Run sits *downstream* of them: a firing written
// while the human is away stayed in the durable queue until the next keystroke,
// and a requested compaction stayed armed until a turn that might never come.
//
// So the prompt returns early, with woke=true, when the signal says there is
// something to do. The signal is only a hint: PendingWork is checked before
// waking, so a stale or duplicate notify cannot cut a prompt short for nothing,
// and an answer that arrived in the same instant is always preferred to the
// wake - a keystroke must not lose to a job that finished a millisecond earlier.
//
// loop==nil (a front end with no run to hand a step back to) degrades to a plain
// Ask rather than to a prompt that cannot be woken and cannot act.
func (h *Interactive) askWaking(ctx context.Context, q Question) (Answer, bool, error) {
	if h.Loop == nil {
		a, err := h.UI.Ask(ctx, q)
		return a, false, err
	}
	loop := h.Loop

	// Already-armed work must not have to wait for a keystroke to be noticed.
	if loop.PendingWork() {
		return Answer{}, true, nil
	}

	// Ask runs inline, on the agent goroutine, exactly as it did before this
	// function existed. That is not a style preference: an earlier version of
	// this helper called Ask from a second goroutine behind a cancellable
	// context, and it passed every wake test while intermittently deadlocking
	// the window's own trajectory test. The window hands an answer to whichever
	// Ask currently owns g.pending, so relocating the call reorders
	// publish-vs-answer across a goroutine boundary and a keystroke can land in
	// the gap and be lost. The only thing allowed off-goroutine here is the
	// watcher below, and it never touches the UI - its sole effect is cancel().
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watched := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(watched)
		select {
		case <-loop.WorkSignal():
			// A hint, not an instruction: if the debt is already gone (another
			// boundary discharged it), do not cut this prompt short for
			// nothing - keep waiting and let the human finish answering.
			if loop.PendingWork() {
				cancel()
				return
			}
			select {
			case <-stop:
			case <-loop.WorkSignal():
				if loop.PendingWork() {
					cancel()
				}
			}
		case <-stop:
		case <-ctx.Done():
		}
	}()

	ans, err := h.UI.Ask(wctx, q)
	if err == nil {
		return ans, false, nil
	}
	// The prompt was cut short. Distinguish "the harness owes a step" from a
	// genuine cancellation of the run itself: only the first is a wake, and
	// reporting the other as one would end a cancelled run politely instead of
	// unwinding it.
	if wctx.Err() != nil && ctx.Err() == nil && loop.PendingWork() {
		return Answer{}, true, nil
	}
	return Answer{}, false, err
}

// OnSubmit implements confirm_exit: the run only ends when the user says so.
func (h *Interactive) OnSubmit(ctx context.Context, exitMsg model.Message) error {
	if !h.Cfg.ConfirmExit {
		return nil
	}
	for {
		ans, woke, err := h.askWaking(ctx, Question{Kind: AskNewTask, Title: newTaskTitle, Cfg: h.Cfg})
		if err != nil {
			return err
		}
		if woke {
			// A result the harness owes outranks the wait for a task: give the
			// step back to Run, whose next boundary drains the wake and asks the
			// model, so the report reaches the conversation without a human
			// having to type to unblock it. No messages: the wake text is
			// authored by the drain, not by this prompt.
			if h.Loop.OwedDeliveries() > 0 {
				return &model.Interrupt{Kind: model.KindUserInterruption}
			}
			// Compaction only: run it here, because here *is* the safe boundary
			// it was waiting for, and stay at the prompt. The user asked for a
			// summary, not for a turn.
			if h.Loop.CompactionPending() {
				h.Loop.CompactHere(ctx)
			}
			continue
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
			// The next thing typed at the ready line is the human continuing the
			// conversation, not the harness announcing a discrete task.
			return userWords(newTaskMessage(ans.Text), "UserNewTask")
		}
	}
}

// NextHumanCommand implements human mode.
func (h *Interactive) NextHumanCommand(ctx context.Context) (string, bool) {
	if h.Cfg.Mode != "human" {
		return "", false
	}
	for {
		ans, woke, err := h.askWaking(ctx, Question{Kind: AskHuman, Title: promptLine(), Cfg: h.Cfg})
		if err != nil {
			return "", false
		}
		if woke {
			// Human mode never passes the drain at the top of Query - a user
			// command replaces the model's turn before it - so discharge the
			// debt here and then let Query run with it already in the history.
			if h.Loop.CompactionPending() {
				h.Loop.CompactHere(ctx)
			}
			if h.Loop.DeliverOwedNow() {
				return "", false // ask the model, now that it has something to read
			}
			continue // compaction alone: still the human's turn
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

// OnCallOpened implements agent.CallOpener: the loop has just opened the result
// file for one of the calls this front end announced, so a window that streams live
// output into the card can start pointing at it. Print mode does not implement the
// other half and gets nothing forwarded, which is why this is an assertion and not
// a UI method - an empty method on every front end is the dead-code shape this
// package keeps deleting elsewhere.
func (h *Interactive) OnCallOpened(action model.Action, path string) {
	if c, ok := h.UI.(callObserver); ok {
		c.CallStarted(action, path)
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
	// The comment is the human's prose; the prefix is what the model is
	// promised. Authorship travels with it so the transcript can show it on
	// their side. The branches above - "Temporary interruption caught." - are
	// the harness reporting a cancellation and stay where they are.
	return userWords("Interrupted by user: "+text, model.KindUserInterruption)
}

// OnMessage forwards every history addition to the UI.
func (h *Interactive) OnMessage(m model.Message) {
	// Settle before forwarding: the forward is what rebuilds the transcript with
	// the real card, and the running placeholder has to be gone by then or the
	// step appears twice. It runs for every role rather than only tool results,
	// because the paths that end a step without a result - cancellation, an
	// interpreter panic, a refused action - are precisely the ones that would
	// otherwise strand a card reading "running" on a session that already
	// stopped.
	if is, ok := h.UI.(Issuer); ok {
		is.SettleCalls()
	}
	h.UI.Message(m)
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
