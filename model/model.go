// Package model talks to the sushi server and turns its replies into agent
// messages and actions.
package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/VictorLowther/mini/env"
)

// Message is an OpenAI-shaped chat message. It is an open map, not a struct,
// because sushi's chat template consumes `reasoning_content` and it MUST
// round-trip back in the history; a closed struct would silently drop the
// model's thinking on every turn (along with any future sibling field).
type Message = map[string]any

// Tool names the agent advertises. bash is the workhorse; the job_* set exists
// because ending a run is something only the harness can do - see Needs-
// Confirmation for why the kill among them asks for nothing.
const (
	// ToolAgent is the name step 6 gives the spawn tool. It is declared with the
	// others now because the tool set and the prompt gates reference it today:
	// TopLevelTools will include it and SubagentTools will drop it, and the
	// distinction has to exist before the capability does, or the first
	// implementation of spawning ships with subagents able to spawn.
	ToolAgent   = "agent"
	ToolBash    = "bash"
	ToolJobs    = "jobs"
	ToolJobKill = "job_kill"
	ToolJobSend = "job_send"
)

// Action is one parsed tool call.
type Action struct {
	// Tool is the advertised name the call was made against; empty means bash,
	// which is what every model turn and every stored row predates.
	Tool string `json:"-"`
	// Command is the bash script. For the job tools it is the single string
	// argument they take (a job id), so the ledger and the confirm panel still
	// have one column of "what this call asked for" to show.
	Command    string `json:"command"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Args is the parsed argument object, kept for the optional parameters the
	// job tools take and read through Str/Bool rather than growing a field per
	// key on this struct.
	Args map[string]any `json:"-"`
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
	// Description is the model's own short name for the call. It lands on the
	// job's label, which is what `jobs` prints and what a wake will quote, so
	// "j3" becomes "quantise 70b" in anything a human or a later turn reads.
	Description string `json:"description,omitempty"`
}

// Str reads an optional string argument. Absent, null, or the wrong shape all
// come back "", which for these tools means "not specified" and never a
// rejected call.
func (a Action) Str(key string) string {
	switch v := a.Args[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return strings.TrimSpace(string(b))
	}
}

// Bool reads an optional boolean argument, forgiving the quoted spellings a
// model produces.
func (a Action) Bool(key string) bool {
	switch v := a.Args[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	case float64:
		return v != 0
	}
	return false
}

// ToolName is the advertised tool, defaulting to bash for actions built before
// the tool set grew (and for the human-mode path, which only ever runs shell).
func (a Action) ToolName() string {
	if a.Tool == "" {
		return ToolBash
	}
	return a.Tool
}

// Display is what a human reads when the call is listed: the panel that asks for
// confirmation, the transcript's tool card, and the ledger's command column. A
// job call has to say which tool it is, because `job_kill j1` and a bare `j1`
// are not the same thing to skim past.
func (a Action) Display() string {
	if a.ToolName() == ToolBash {
		return a.Command
	}
	if a.Command == "" {
		return a.ToolName()
	}
	return a.ToolName() + " " + a.Command
}

// NeedsConfirmation reports whether the action must reach a human before it can
// have its effect.
//
// Only starting work counts. `job_kill` deliberately does NOT, even though it
// signals processes: it can only end runs this session already started, and
// those already passed whatever gate applied to launching them. A kill cannot
// begin anything new, cannot touch a process the harness did not launch, and
// cannot lose data that was not already being written - it is the mitigation,
// and prompting for it is the one design that makes the mitigation slower than
// the problem. `jobs` reads a table.
//
// This is where the survey's advice stops applying: zero marks TaskStop
// SideEffectShell + PermissionPrompt, little-coder puts ShellStart in the
// gate's SHELL_TOOLS set, and both are right for a registry that can *launch*
// from a tool. Neither argument reaches ending a run that exists.
func (a Action) NeedsConfirmation() bool {
	return a.ToolName() == ToolBash
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
			"A command still running after 30 seconds is moved to the " +
			"background: nothing is killed, it keeps running, and you get its " +
			"job id and output path instead of a result - so a long command " +
			"costs a turn, not a re-run. Set background: true to skip the wait " +
			"when you already know, and timeout_s to wait longer on purpose. A " +
			"job's result reaches you automatically once it finishes: if your " +
			"turn has already ended, the run continues until it has been " +
			"reported, so never poll a log or sleep to wait for one. A job still " +
			"*running* is not a result owed - the run can end beside it, and " +
			"closing the session stops it, so end it yourself with job_kill when " +
			"you are done with it. A job outlives a turn, never a session.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute",
				},
				"timeout_s": map[string]any{
					"type":        "integer",
					"description": "Seconds to wait for this command before it is moved to the background. Optional and uncapped: set it to what the command actually needs, such as a build or a training run. 0 or omitted means the default of 30 seconds. The deadline ends the wait and never the command, so nothing is killed at it and a half-finished run must not be re-started - ask for the time instead, or background it on purpose.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "A few words naming what this command is for. Shown in the jobs listing and in anything that reports on the job later, so a long run stays identifiable after the turn that started it.",
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

// JOBS_TOOL answers "what is running, and where is its output". It exists
// because the registry, not the conversation, is the truth about live jobs:
// compaction keeps recency and can summarize a handoff away, and a model that
// cannot enumerate what it left on the GPU will either re-run it or forget it.
var JOBS_TOOL = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        ToolJobs,
		"description": "List the session's jobs: id, state, how long, why it is still around, exit status, and the path its output is accumulating at. Read-only. Endings are delivered to you automatically at your next step, so this is for checking in, not for watching. A job is the only way to end a command that outlived its turn - the shell runs in-process, so no pattern-based kill command can reach a whole job.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "One job id to describe instead of listing them all. Optional.",
				},
			},
		},
	},
}

// JOB_KILL_TOOL is the only way to end a job. It takes no confirmation, on
// purpose: see Action.NeedsConfirmation.
var JOB_KILL_TOOL = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        ToolJobKill,
		"description": "End a job, or all of them. Give it the job id from `jobs`, or all: true. This is the only reliable way to stop a backgrounded command: a job may be holding several processes at once and they are not addressable from inside a shell, because the shell is the agent process itself. No confirmation is required, and the command it stops does not need re-running.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The job id to end, exactly as `jobs` reported it.",
				},
				"all": map[string]any{
					"type":        "boolean",
					"description": "End every running job of this session. Use when the work is finished or when you are giving up on a batch.",
				},
			},
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
