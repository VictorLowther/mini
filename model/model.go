// Package model talks to the sushi server and turns its replies into agent
// messages and actions.
package model

import (
	"context"

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

// BASH_TOOL is the only tool the agent advertises, byte-for-byte the shape
// mini-swe-agent sends (models/utils/actions_toolcall.py).
var BASH_TOOL = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "bash",
		"description": "Execute a bash command",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute",
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
