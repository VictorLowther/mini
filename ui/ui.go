// Package ui is the human end of the agent: what it prints and what it asks.
// Both the Fyne GUI and the plain --print mode satisfy the same UI interface,
// and ui.Interactive adapts a UI to agent.Hooks.
package ui

import (
	"context"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// Question kinds: the five prompts agents/interactive.py actually contains.
const (
	AskConfirm  = "confirm"      // execute these actions?
	AskNewTask  = "new-task"     // agent is ready for the next task
	AskLimits   = "limits"       // step/cost limit reached on a terminal
	AskHuman    = "human"        // human mode: command for the agent to run
	AskTask     = "task"         // what do you want to do?
	AskInterrup = "interruption" // Ctrl+C: type a comment
)

// Question is one blocking prompt.
type Question struct {
	Kind    string
	Title   string
	Actions []model.Action
	// Cfg lets the UI read and change the interactive mode: slash commands
	// write config.mode, so the mode a run started in is the mode the footer
	// reports and the one the next turn obeys.
	Cfg *agent.Config
	// Origin names the session that is actually waiting, when it is not the one
	// the human is looking at - a subagent's confirmation reaching its parent's
	// front end (docs/parallel-sessions.md §5.5, decision 13). Empty means the
	// conversation itself. Render it wherever the prompt is shown: a queue that
	// can hold two askers makes an unattributed prompt the default rather than
	// the exception.
	Origin string
	// Limits echoed back for the AskLimits prompt.
	StepLimit int
	CostLimit float64
	Cost      float64
	NCalls    int
}

// Answer is a UI's reply to a Question. Which fields matter depends on Kind.
type Answer struct {
	// Text is the rejection reason, the new task, or the human command.
	Text string
	// Reject means "do not execute these actions" (AskConfirm).
	Reject bool
	// RePrompt means the UI consumed a control command (help, mode switch on
	// the wrong mode) and the same question should be asked again.
	RePrompt bool
	// NewLimits carries raised limits (AskLimits); when false the run gives up.
	NewLimits bool
	Steps     int
	Cost      float64
}

// displayTitle is the one line a prompt is introduced by, in both front ends.
// When the question belongs to another session it says whose: the queue of §9.4
// means a subagent's ask can be the only thing on screen while the human believes
// they are watching their own conversation, and approving `rm -rf build/` under
// that belief is confirm mode's safety property quietly voided (§5.5).
func displayTitle(q Question) string {
	if q.Origin == "" {
		return q.Title
	}
	return "subagent " + q.Origin + " \u00b7 " + q.Title
}

// UI renders progress and asks questions. Reasoning and Content are called
// from the model's streaming goroutine with text chunks; the other methods
// come from the agent goroutine.
type UI interface {
	Reasoning(text string)
	Content(text string)
	// Message announces a message entering the history, which is where the
	// per-role headers are printed. It is called after any streamed chunks for
	// that message, so an implementation can skip text it already wrote.
	Message(m model.Message)
	// Ask blocks until the user has answered, or ctx is done.
	Ask(ctx context.Context, q Question) (Answer, error)
	// Interactive reports whether a human is on the other end (a terminal to
	// prompt on). Limits are never lifted without one.
	Interactive() bool
}
