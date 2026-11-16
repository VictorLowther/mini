package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/tpl"
)

// optionalSeconds reads an optional timeout_s. Models emit the same value as a
// number, a string, or a float with a zero fraction depending on how the turn
// was sampled, and all three mean the same thing; anything else means the model
// did not ask, so the default budget applies. Negative and zero both mean
// "no opinion" - there is no way to ask for an uncapped foreground wait, because
// an uncapped wait would be the harness hanging on a command nobody is watching.
func optionalSeconds(v any) int {
	switch n := v.(type) {
	case float64:
		return clampSeconds(int(n))
	case int:
		return clampSeconds(n)
	case int64:
		return clampSeconds(int(n))
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return clampSeconds(int(i))
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		return clampSeconds(i)
	}
	return 0
}

func clampSeconds(n int) int {
	if n < 1 {
		return 0
	}
	return n
}

// optionalBool reads an optional background flag, accepting the "true" spelling
// a model sometimes quotes.
func optionalBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		s := strings.ToLower(strings.TrimSpace(b))
		return s == "true" || s == "1" || s == "yes"
	}
	return false
}

// parseToolCallActions turns one response's tool calls into actions. A response
// that ended naturally with no tool calls is not an error but a submission - the
// model putting its tools down and going quiet, which the prompts define as "the
// task is done, I am ready for the user's next prompt". Only a call-less response
// that did NOT end naturally (truncated, or a stream that died without a finish
// reason) is a FormatError.
//
// The error texts are worth keeping stable for their own reason, not for
// anyone's benefit: they are the only feedback a model gets when it malformed a
// call, so a rewrite that loses "Missing 'command' argument in bash tool call"
// costs a turn to rediscover what the message already said. All of them arrive
// concatenated in order through the FormatError template with the
// {error, actions, has_tool_calls, finish_reason} context.
func parseToolCallActions(toolCalls []map[string]any, errTemplate string, ctx map[string]any) ([]Action, error) {
	if len(toolCalls) == 0 {
		if fr, _ := ctx["finish_reason"].(string); fr == "stop" {
			return nil, nil // a valid submission turn: no actions, no error
		}
		return nil, formatError(errTemplate, "No tool calls found in the response. Every response must either include at least one tool call or be a final answer without one.", false, ctx)
	}
	actions := make([]Action, 0, len(toolCalls))
	for _, tc := range toolCalls {
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		rawArgs, _ := fn["arguments"].(string)
		id, _ := tc["id"].(string)

		var errMsgs []string
		var args map[string]any
		if err := json.Unmarshal([]byte(orNull(rawArgs)), &args); err != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("Error parsing tool call arguments: %s.", err))
			args = map[string]any{}
		}
		if name == "" {
			name = ToolBash
		}
		switch name {
		case ToolBash, ToolJobs, ToolJobKill, ToolJobWait:
		default:
			errMsgs = append(errMsgs, fmt.Sprintf("Unknown tool '%s'.", name))
		}
		if name == ToolBash {
			if _, ok := args["command"]; !ok {
				errMsgs = append(errMsgs, "Missing 'command' argument in bash tool call.")
			}
		}
		if name == ToolJobKill {
			// "kill it" with no target is not a request the harness can guess
			// at: all: true ends every job in the session, so it has to be the
			// model's own word, and an id it actually read from jobs.
			if id, _ := args["id"].(string); strings.TrimSpace(id) == "" && !optionalBool(args["all"]) {
				errMsgs = append(errMsgs, "job_kill needs an 'id' (from the jobs tool) or all: true.")
			}
		}
		if len(errMsgs) > 0 {
			return nil, formatError(errTemplate, strings.Join(errMsgs, " "), true, ctx)
		}
		action := Action{Tool: name, ToolCallID: id, Args: args}
		switch name {
		case ToolBash:
			// A non-string command survives as its JSON spelling, like Python
			// passing the value straight through.
			command, ok := args["command"].(string)
			if !ok {
				b, _ := json.Marshal(args["command"])
				command = string(b)
			}
			action.Command = command
			// Both optional parameters are permissive on purpose, matching the
			// rule in docs/jobs-research.md §4 that a job which failed to start
			// is worse than one missing a knob: a value that is not a number
			// falls back to "no opinion" rather than failing the call.
			action.TimeoutS = optionalSeconds(args["timeout_s"])
			action.Background = optionalBool(args["background"])
			action.Description = action.Str("description")
		default:
			// The job tools carry one addressing argument, and Command is where
			// the ledger and the panels already know to look.
			action.Command = action.Str("id")
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func orNull(s string) string {
	if strings.TrimSpace(s) == "" {
		return "null"
	}
	return s
}

func formatError(errTemplate, errMsg string, hasToolCalls bool, ctx map[string]any) error {
	renderCtx := map[string]any{"error": errMsg, "actions": []any{}, "has_tool_calls": hasToolCalls}
	for k, v := range ctx {
		renderCtx[k] = v
	}
	if fr, ok := renderCtx["finish_reason"]; !ok || fr == nil {
		// text/template cannot compare nil to a string; Jinja can, and treats
		// it as "not a truncation", which is what "" does here.
		renderCtx["finish_reason"] = ""
	}
	content, err := tpl.Render("format_error", errTemplate, renderCtx)
	if err != nil {
		content = errMsg + fmt.Sprintf(" [format_error_template failed to render: %v]", err)
	}
	return &Interrupt{
		Kind: KindFormatError,
		Messages: []Message{{
			"role":    "user",
			"content": content,
			"extra":   map[string]any{"interrupt_type": "FormatError"},
		}},
	}
}

// formatObservations is the port of format_toolcall_observation_messages:
// missing outputs are padded with "action was not executed", each observation
// renders the observation template with `output` plus the full agent template
// vars. The result text itself never sits here: `output` is the bounded view
// and extra names the result file that holds everything.
func formatObservations(actions []Action, results []env.Result, obsTemplate string, tvars map[string]any) []Message {
	padded := make([]env.Result, len(actions))
	notExecuted := env.Result{ReturnCode: -1, ExceptionInfo: "action was not executed"}
	for i := range padded {
		if i < len(results) {
			padded[i] = results[i]
		} else {
			padded[i] = notExecuted
		}
	}
	out := make([]Message, 0, len(actions))
	for i, action := range actions {
		result := padded[i]
		ctx := make(map[string]any, len(tvars)+1)
		for k, v := range tvars {
			ctx[k] = v
		}
		ctx["output"] = result.AsMap()
		content, err := tpl.Render("observation", obsTemplate, ctx)
		if err != nil {
			content = fmt.Sprintf("[observation_template failed to render: %v]\noutput_path: %s", err, result.OutputPath)
		}
		extra := map[string]any{
			"returncode":     result.ReturnCode,
			"timestamp":      float64(time.Now().UnixMilli()) / 1000,
			"exception_info": result.ExceptionInfo,
			// Transcript metadata: the UI shows what ran and where without
			// having to re-read the assistant message. Never sent to the API,
			// since extra is stripped before the request.
			// Display is the script for bash and "job_kill j1" for the job
			// tools, so a card never shows a bare id with no verb.
			"tool":    action.ToolName(),
			"command": action.Display(),
			"cwd":     result.CWD,
			// The complete output, untrimmed, in this file; the loop adds
			// tcno/tcr_offset/elided_lines when the call has a database row.
			"tcr_path": result.OutputPath,
		}
		for k, v := range result.Extra {
			extra[k] = v
		}
		msg := Message{"content": content, "extra": extra}
		if action.ToolCallID != "" {
			msg["tool_call_id"] = action.ToolCallID
			msg["role"] = "tool"
		} else {
			msg["role"] = "user" // commands issued by the user in human mode
		}
		out = append(out, msg)
	}
	return out
}
