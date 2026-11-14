package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/VictorLowther/mini/env"
)

// Deterministic is the port of models/test_models.py DeterministicModel: it
// replays canned messages in sequence, which is what makes the loop and the
// interactive behaviour testable without a server.
//
// On top of the Python original's pseudo-actions it understands two markers in
// a canned message's extra, which replace the Flaky/Billed subclasses the
// Python suite defines:
//
//	"format_error": true     -> raise FormatError carrying cost + response
//	"raise": "text"          -> return a plain error (the uncaught path)
type Deterministic struct {
	Outputs     []Message
	CostPerCall float64
	ObsTemplate string
	ErrTemplate string

	// Summaries are the canned replies to the compaction side-query,
	// consumed in order by Summarize; running out is an error the same way
	// running out of Outputs is.
	Summaries []string

	// Queries records the message slice handed to each Query, and
	// SummarizeQueries the slice handed to each Summarize, so tests can
	// assert on the exact context the loop sent - including that a
	// summarize request is the live prefix plus one directive turn.
	Queries             [][]Message
	SummarizeQueries    [][]Message
	SummarizeDirectives []string

	current        int
	summaryCurrent int
}

// NewDeterministic builds a replay model from (content, commands) pairs, the
// shape the Python test helpers use.
func NewDeterministic(turns ...string) *Deterministic {
	var outputs []Message
	for _, out := range turns {
		i := strings.Index(out, "\x00")
		content, command := out, out
		if i >= 0 {
			content, command = out[:i], out[i+1:]
		}
		id := fmt.Sprintf("call_%d", len(outputs))
		outputs = append(outputs, Message{
			"role":    "assistant",
			"content": content,
			"extra": map[string]any{
				"actions": []Action{{Command: command, ToolCallID: id}},
				"cost":    1.0,
			},
			"tool_calls": []map[string]any{{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      "bash",
					"arguments": mustJSONString(map[string]any{"command": command}),
				},
			}},
		})
	}
	return &Deterministic{Outputs: outputs, CostPerCall: 1.0}
}

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Query returns the next canned message.
func (d *Deterministic) Query(_ context.Context, msgs []Message, _ *Handler) (Message, error) {
	d.Queries = append(d.Queries, msgs)
	if d.current >= len(d.Outputs) {
		return nil, fmt.Errorf("deterministic model ran out of outputs after %d calls", len(d.Outputs))
	}
	out := cloneMessage(d.Outputs[d.current])
	d.current++
	extra := Extra(out)
	retry := processTestActions(ActionsOf(out))
	if retry {
		return d.Query(context.Background(), msgs, nil)
	}
	if raise, ok := extra["raise"].(string); ok {
		return nil, errors.New(raise)
	}
	cost := d.CostPerCall
	if c, ok := extra["cost"].(float64); ok {
		cost = c
	}
	billed := true
	if b, ok := extra["format_error"].(bool); ok && b {
		billed, _ = extra["billed"].(bool)
		if !billed {
			// FlakyModel: the turn dies in parsing and is never charged.
			return nil, &Interrupt{Kind: KindFormatError, Messages: []Message{{
				"role":    "user",
				"content": "No tool calls found in the response.",
				"extra":   map[string]any{"interrupt_type": "FormatError"},
			}}}
		}
	}
	if err := GLOBAL_STATS.Add(cost, 0); err != nil {
		return nil, err
	}
	if ff, _ := extra["format_error"].(bool); ff {
		msg := Message{
			"role":    "user",
			"content": "No tool calls found in the response.",
			"extra": map[string]any{
				"interrupt_type": "FormatError",
				"cost":           cost,
				"response":       out,
			},
		}
		return nil, &Interrupt{Kind: KindFormatError, Messages: []Message{msg}}
	}
	extra["cost"] = cost
	extra["timestamp"] = float64(time.Now().UnixMilli()) / 1000
	return out, nil
}

// processTestActions handles the /sleep and /warning pseudo-actions; the return
// value means "this turn was consumed, take the next output" (Python's
// recursive query).
func processTestActions(actions []Action) bool {
	for _, a := range actions {
		if s, found := strings.CutPrefix(a.Command, "/sleep "); found {
			var secs float64
			if _, err := fmt.Sscanf(s, "%g", &secs); err == nil {
				time.Sleep(time.Duration(secs * float64(time.Second)))
				return true
			}
		}
		if rest, found := strings.CutPrefix(a.Command, "/warning"); found {
			slog.Warn("deterministic model warning", "text", strings.TrimSpace(rest))
			return true
		}
	}
	return false
}

// Summarize replays a canned summary, recording the exact context it was
// given: the cache-reading property of the real side-query is asserted
// against that recording in the compaction tests.
func (d *Deterministic) Summarize(_ context.Context, msgs []Message, directive string, _ int) (string, error) {
	d.SummarizeQueries = append(d.SummarizeQueries, msgs)
	d.SummarizeDirectives = append(d.SummarizeDirectives, directive)
	if d.summaryCurrent >= len(d.Summaries) {
		return "", fmt.Errorf("deterministic model ran out of summaries after %d calls", len(d.Summaries))
	}
	out := d.Summaries[d.summaryCurrent]
	d.summaryCurrent++
	if out == "" {
		return "", fmt.Errorf("deterministic summary was empty")
	}
	return out, nil
}

func (d *Deterministic) FormatObservations(msg Message, results []env.Result, tvars map[string]any) []Message {
	tmpl := d.ObsTemplate
	if tmpl == "" {
		// DeterministicModelConfig.observation_template, translated.
		tmpl = "{{ if .output.exception_info }}<exception>{{ .output.exception_info }}</exception>\n{{ end -}}" +
			"<returncode>{{ .output.returncode }}</returncode>\n<output>\n{{ .output.output }}</output>"
	}
	return formatObservations(ActionsOf(msg), results, tmpl, tvars)
}

func (d *Deterministic) TemplateVars() map[string]any {
	return map[string]any{"model_name": "deterministic", "cost_per_call": d.CostPerCall}
}

func (d *Deterministic) Serialize() map[string]any {
	return map[string]any{
		"info": map[string]any{
			"config": map[string]any{
				"model": map[string]any{
					"outputs":       d.Outputs,
					"model_name":    "deterministic",
					"cost_per_call": d.CostPerCall,
				},
				"model_type": "mini/model.Deterministic",
			},
		},
	}
}

func cloneMessage(m Message) Message {
	out := make(Message, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
