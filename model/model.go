// Package model talks to the sushi server and turns its replies into agent
// messages and actions.
package model

import (
	"context"
	"os"

	"github.com/VictorLowther/mini/env"
)

// Message is an OpenAI-shaped chat message. It is an open map, not a struct,
// because sushi's chat template consumes `reasoning_content` and it MUST
// round-trip back in the history; a closed struct would silently drop the
// model's thinking on every turn (along with any future sibling field).
type Message = map[string]any

// Action is one parsed bash tool call.
type Action struct {
	Command    string `json:"command"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// TimeoutS is the model's own budget for this command, in seconds, from the
	// optional timeout_s parameter. 0 means "no opinion": the environment's
	// default applies. There is deliberately no ceiling - a training run is
	// longer than any number the harness could pick (docs/jobs-research.md §4,
	// decision 2) - and a value the model asks for expires into the same
	// handoff as the default, never into a silent kill.
	TimeoutS int `json:"timeout_s,omitempty"`
	// Background asks for the run as a job: start it, report the id and the
	// path its output is accumulating at, and return without waiting.
	Background bool `json:"background,omitempty"`
}

// ToolCall is an issued call plus the open file its complete merged output is
// written to. Number is the session's tool-call number and Path the file name;
// File is handed straight to the shell as stdout *and* stderr, so the file ends
// up exactly as `> out.log 2>&1` would leave it. The zero value means "not
// recorded", and the loop then omits the path from the result's extra and the
// observation renders without one.
type ToolCall struct {
	Number int
	Path   string
	File   *os.File
	Action Action
}

// ToolOutcome is what a finished command left behind, every field already
// measured on the closed result file so the database holds it once and nothing
// recalculates it: Bytes is the file size, ViewOffset the byte the model's view
// starts at, ViewElided the lines in front of it.
type ToolOutcome struct {
	Bytes      int64
	ViewOffset int64
	ViewElided int
	ReturnCode int
	CWD        string
}

// InterruptKind names the control-flow conditions the loop reacts to. They are
// the Python exception class names, which double as the exit_status strings.
const (
	KindSubmitted         = "Submitted"
	KindLimitsExceeded    = "LimitsExceeded"
	KindTimeExceeded      = "TimeExceeded"
	KindFormatError       = "FormatError"
	KindRepeatedFormatErr = "RepeatedFormatError"
	KindUserInterruption  = "UserInterruption"
)

// Interrupt carries the messages the loop appends before reacting. It lives in
// model (not agent) because the model raises it and the agent imports the
// model, so the dependency can only point this way.
type Interrupt struct {
	Kind     string
	Messages []Message
}

func (i *Interrupt) Error() string { return i.Kind }

// Handler receives streaming progress. nil means a non-streaming caller: the
// request still streams (there is only one decode path), the callbacks are
// simply not made.
type Handler struct {
	Reasoning func(string)
	Content   func(string)
}

func (h *Handler) reasoning(s string) {
	if h != nil && h.Reasoning != nil && s != "" {
		h.Reasoning(s)
	}
}

func (h *Handler) content(s string) {
	if h != nil && h.Content != nil && s != "" {
		h.Content(s)
	}
}

// BASH_TOOL is the only tool the agent advertises. The shape is mini's own to
// change: it arrived as a port of mini-swe-agent's, which was the fastest way
// to tell whether the loop worked, and that scaffolding is not a constraint.
// A command that outlives its tool call needs parameters the ported agent never
// had, and the tool description - not a reference implementation - is where the
// model learns what this one does (docs/jobs-research.md §4).
var BASH_TOOL = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name": "bash",
		"description": "Execute a bash command. Output is merged stdout+stderr. " +
			"A command that is still running after 30 seconds is interrupted and " +
			"comes back as a timeout observation, so for anything longer ask for " +
			"the time it needs with timeout_s, or set background: true to get a " +
			"job id and an output path back immediately and keep working. A job " +
			"outlives a turn, never a session.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute",
				},
				"timeout_s": map[string]any{
					"type":        "integer",
					"description": "Seconds to wait for this command before giving up on it. Optional and uncapped: set it to what the command actually needs, such as a build or a training run. 0 or omitted means the default of 30 seconds. On expiry the command is interrupted and its output so far comes back as a timeout observation, so ask for more time rather than re-running something that was half done.",
				},
				"background": map[string]any{
					"type":        "boolean",
					"description": "Start the command as a job and return immediately with its job id and output path, instead of waiting for it. Use for anything that outlives a few minutes: training, quantising, servers, soak tests, long builds. The job keeps its own output file; read it with tail, sed -n, or grep.",
				},
			},
			"required": []string{"command"},
		},
	},
}

// Model is what the agent loop needs from a model. Both Sushi and the
// deterministic test model implement it.
type Model interface {
	Query(ctx context.Context, msgs []Message, h *Handler) (Message, error)
	FormatObservations(msg Message, results []env.Result, tvars map[string]any) []Message
	TemplateVars() map[string]any
	Serialize() map[string]any
}

// Summarizer is what compaction asks of a model on top of Model: given the
// exact projected context plus one directive turn, return the summary text.
// Sushi implements it by riding the main request's prefix - the warmed prompt
// cache reads the whole conversation, so the compaction event costs the
// directive's prefill and the summary's decode, never a re-read of the
// session. Models without it (tests, unusual backends) simply never
// summarize: the loop logs and moves on.
type Summarizer interface {
	// Summarize returns the summary text. It is an error (not a truncated
	// success) when the reply ran out of output budget or reached for a tool
	// instead of writing prose.
	Summarize(ctx context.Context, msgs []Message, directive string, maxTokens int) (string, error)
}

// Extra returns (creating if needed) a message's extra map.
func Extra(m Message) map[string]any {
	if e, ok := m["extra"].(map[string]any); ok {
		return e
	}
	e := map[string]any{}
	m["extra"] = e
	return e
}

// ActionsOf returns the parsed actions carried by a message.
func ActionsOf(m Message) []Action {
	e, _ := m["extra"].(map[string]any)
	switch a := e["actions"].(type) {
	case []Action:
		return a
	case []any:
		var out []Action
		for _, item := range a {
			if m, ok := item.(map[string]any); ok {
				act := Action{}
				act.Command, _ = m["command"].(string)
				act.ToolCallID, _ = m["tool_call_id"].(string)
				out = append(out, act)
			}
		}
		return out
	}
	return nil
}
