package model

import (
	"fmt"
	"sort"
	"strings"
)

// ToolSet is one session's advertised tools, and the single source of truth for
// them (docs/parallel-sessions.md §5.6).
//
// Before this, the same list was written in three places: the request body
// (`Sushi.requestBody` spliced BASH_TOOL, JOBS_TOOL and JOB_KILL_TOOL into every
// query), the prompt (`config/configs/mini.yaml` names the tools in prose), and
// the parser (`parseToolCallActions` switches on the names it will accept). The
// third is what makes it a correctness problem rather than a tidiness one: an
// advertised tool the parser does not know cannot be used, and a tool hidden from
// the array but left in the prose is a tool the model is told about and then
// refused for wanting.
//
// So there is one value, and three renderings of it. A subagent's set omits the
// spawn tool - which is how "subagents cannot spawn subagents" is enforced
// (§5.6): a capability is not hidden until neither the array nor the instructions
// mention it, and a listing assembled from the set cannot drift from the set.
type ToolSet []Tool

// Tool pairs an OpenAI function schema with the two things derived from it: the
// name the parser accepts, and the one-line purpose the generated prompt listing
// shows. The brief is deliberately short - this text is read by a model on every
// turn, and the full description already travels in the schema.
type Tool struct {
	Name   string
	Brief  string
	Schema map[string]any
}

// TopLevelTools is what a session started by a human is offered. The spawn tool
// joins it in step 6 (docs/parallel-sessions.md §7); until then a top-level and a
// subagent set differ only by that future entry, which is the point of building
// the mechanism before the capability it distinguishes.
func TopLevelTools() ToolSet {
	return ToolSet{
		Tool{Name: ToolBash, Brief: "run a bash command; long work moves to the background instead of being killed", Schema: BASH_TOOL},
		Tool{Name: ToolJobs, Brief: "list this session's jobs and where their output is accumulating", Schema: JOBS_TOOL},
		Tool{Name: ToolJobKill, Brief: "end a job, or all of them", Schema: JOB_KILL_TOOL},
		Tool{Name: ToolAgent, Brief: "start a subagent to finish one task and report back", Schema: AGENT_TOOL},
		Tool{Name: ToolJobSend, Brief: "send a message to one of your subagents, running or finished", Schema: JOB_SEND_TOOL},
	}
}

// SubagentTools is what a session spawned by another session is offered: every
// tool a top-level session has except the two that only make sense for a session
// that can have children.
//
// Dropping the spawn tool is the ruling (§5.6) - a subagent that cannot see the
// capability cannot reach for it, and there is no refusal for it to retry past.
// Dropping `job_send` is the same argument one step on, and worth stating
// because it is not in the ruling's words: sends are addressed to agent-kind
// jobs, sends come from a session's own registry, and a subagent can create no
// agent jobs at all. So it is a tool with no possible target - and a tool with
// no possible target is one the model should not be told exists, for exactly the
// reason §5.6 gives for hiding the spawn tool rather than refusing it.
func SubagentTools() ToolSet {
	set := ToolSet{}
	for _, t := range TopLevelTools() {
		if t.Name == ToolAgent || t.Name == ToolJobSend {
			continue
		}
		set = append(set, t)
	}
	return set
}

// toolsForKind picks the set by session kind. The kind strings are defined in
// `session`, which imports this package, so the comparison is against the literal
// here rather than an imported constant - the dependency only ever points one way,
// and a caller that already knows its ToolSet should pass it instead of routing
// through a string.
func toolsForKind(kind string) ToolSet {
	if kind == "subagent" {
		return SubagentTools()
	}
	return TopLevelTools()
}

// schemas is rendering one: the `tools` array of the request body.
func (t ToolSet) schemas() []any {
	if len(t) == 0 {
		t = TopLevelTools()
	}
	out := make([]any, 0, len(t))
	for _, tool := range t {
		out = append(out, tool.Schema)
	}
	return out
}

// accepts is rendering two: the parser's set. An empty set means the default, so
// a client built without one - every test, and any future caller that did not
// think about it - still parses what it advertises.
func (t ToolSet) accepts(name string) bool {
	set := t
	if len(set) == 0 {
		set = TopLevelTools()
	}
	for _, tool := range set {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// Has reports whether the set advertises a tool by name. Prompt gates read this
// rather than testing a kind, so the instructions and the array cannot disagree.
func (t ToolSet) Has(name string) bool { return t.accepts(name) }

// Names lists the advertised tools, sorted - for logs, assertions, and any
// caller that needs to say which set it handed out.
func (t ToolSet) Names() []string {
	set := t
	if len(set) == 0 {
		set = TopLevelTools()
	}
	out := make([]string, 0, len(set))
	for _, tool := range set {
		out = append(out, tool.Name)
	}
	sort.Strings(out)
	return out
}

// Listing is rendering three: the block the system prompt shows. It is generated
// from the same set the request carries, because a hand-written list of tools is
// a list that will one day name a tool the model was not given - and the codebase
// has already made this argument once about a number that was written down twice
// (agent/agent.go:790).
func (t ToolSet) Listing() string {
	set := t
	if len(set) == 0 {
		set = TopLevelTools()
	}
	var b strings.Builder
	for _, tool := range set {
		fmt.Fprintf(&b, "- `%s` - %s\n", tool.Name, tool.Brief)
	}
	return strings.TrimRight(b.String(), "\n")
}

// AGENT_TOOL spawns a subagent: a second session, created with one directive,
// whose report is the text of its final turn. It is a job, which is the whole
// design decision behind it - the registry already owns the row, the output
// file, the terminal state, the firing and the delivery at a step boundary, so
// spawning costs one new kind rather than a new mechanism, and `jobs` and
// `job_kill` work on a subagent with no changes (§5.10, §9.5).
var AGENT_TOOL = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name": ToolAgent,
		// The description carries the contract, because this is the only place
		// a model learns that the result is not in this response.
		"description": "Start a subagent to complete one task and report back. It runs as a job in its own session: the call returns immediately with a job id, and the subagent's report - the text of its final turn - reaches you at a step boundary when it has finished, exactly like any other background result. Do not poll for it and do not start a second one to do the same work. Give it a short unique name: it is the label a human sees, and a name is held until that subagent is genuinely done. Write the prompt so it stands alone - a subagent cannot see this conversation, and cannot ask you anything once it starts.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "A short descriptive name for this subagent, unique among your running and finished subagents. It is what any human-facing prompt will say, so 'quantise 70b' and not 'task 3'.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The complete task, self-contained. Include the directory, what to find out or change, what a finished job looks like, and what the report should contain - the subagent sees neither this conversation nor the reasoning that led here.",
				},
			},
			"required": []string{"name", "prompt"},
		},
	},
}

// JOB_SEND_TOOL delivers text to a running job. For an agent job it appends a
// user message at the next step boundary - the same path typed input takes - and
// for a job that has already finished it restores the session and starts another
// run with the message queued first, which is what §9.3's "revival is
// restore-plus-send" means in practice.
var JOB_SEND_TOOL = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        ToolJobSend,
		"description": "Send text to one of your subagents. A running subagent receives it as a user message at its next step boundary - steer it, correct it, or answer a question it has not asked yet. A finished one is resumed with its full history and continues from there. Only agent jobs accept input for now; sending to a background command is refused. The job id comes from `jobs`.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The job id to speak to, exactly as `jobs` printed it.",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "The message. It arrives as a user turn, so write it as an instruction, not a note to self.",
				},
			},
			"required": []string{"id", "text"},
		},
	},
}
