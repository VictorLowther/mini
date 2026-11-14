package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/tpl"
)

// parseToolCallActions is the port of parse_toolcall_actions, with one
// deliberate departure from the Python original: a response that ended
// naturally with no tool calls is not an error but a submission - the model
// putting its tools down and going quiet, which the prompts define as "the
// task is done, I am ready for the user's next prompt". Only a call-less
// response that did NOT end naturally (truncated, or a stream that died
// without a finish reason) is a FormatError. Error texts are reproduced
// verbatim otherwise, concatenated in the same order and stripped, and every
// failure becomes a FormatError carrying the rendered template plus the
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
		}
		if name != "bash" {
			errMsgs = append(errMsgs, fmt.Sprintf("Unknown tool '%s'.", name))
		}
		if _, ok := args["command"]; !ok {
			errMsgs = append(errMsgs, "Missing 'command' argument in bash tool call.")
		}
		if len(errMsgs) > 0 {
			return nil, formatError(errTemplate, strings.Join(errMsgs, " "), true, ctx)
		}
		// A non-string command survives as its JSON spelling, like Python
		// passing the value straight through.
		command, ok := args["command"].(string)
		if !ok {
			b, _ := json.Marshal(args["command"])
			command = string(b)
		}
		actions = append(actions, Action{Command: command, ToolCallID: id})
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
			"command": action.Command,
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
