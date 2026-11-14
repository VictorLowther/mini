package model

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Compaction is a rare, singular event (see docs/compaction-research.md):
// while a session lives its context is append-only, so nothing here rewrites
// history. A compaction is recorded as one transcript turn of role
// RoleCompaction carrying a summary of everything before its kept tail, and
// ProjectContext is the pure projection every model request sees: the
// original prompt plumbing, the summary standing in for the elided middle,
// and the kept turns verbatim. The full transcript in the store never
// changes; only the projection does, which keeps the warmed prompt-cache
// prefix intact for every step that does not compact.

// RoleCompaction is the transcript row that anchors a compaction. Like exit
// rows it is never sent to the model as itself; the projection replaces the
// turns it summarizes with a synthesized user message.
const RoleCompaction = "compaction"

// Keys of a compaction row's extra map.
const (
	// ExtraKeptN is how many turns of the projected context the compaction
	// preserved verbatim, counted in projected space so re-projection needs
	// no row numbering and can never resurrect a previously elided turn.
	ExtraKeptN = "kept_n"
	// ExtraFirstKeptTurn is the store seq of the first kept row, when one
	// exists: debugging and GUI cross-reference only, never load-bearing.
	ExtraFirstKeptTurn = "first_kept_turn"
	// ExtraTokensBefore/ExtraTokensAfter are the estimated projected-context
	// sizes around the anchor, for the GUI card and for humans reading the
	// session; the next real usage report supersedes them.
	ExtraTokensBefore = "tokens_before"
	ExtraTokensAfter  = "tokens_after"
	// ExtraCompactionModel names the model that wrote the summary.
	ExtraCompactionModel = "compaction_model"
)

// summaryHead frames a stored summary for the wire. The text is constant so
// the rebuilt prefix is byte-identical across every request after the
// compaction; the steering sentences are little-coder's lesson (#68): a
// model that re-explores what the summary already tells it re-inflates the
// context toward the next compaction. mini's promise is stronger than other
// harnesses': raw outputs are not lost, they are on disk.
const summaryHead = `The conversation before this point was compacted into the summary below; the turns that follow it are verbatim.

Do not re-read files or re-run commands whose results already appear in the summary or in the recent turns. The complete, untruncated output of every earlier command remains on disk at the result paths the summary lists; read those with grep, sed -n, or tail instead of regenerating them.

<summary>
`

const summaryTail = `
</summary>`

// ProjectContext rebuilds the context a model call should receive from the
// full transcript: exit rows drop out, and each compaction row replaces the
// turns it elided with its synthesized summary ahead of the kept tail. The
// scan is single-pass over raw transcript order, and every anchor operates on
// the already-projected prefix, so a compaction can never resurrect turns an
// earlier compaction elided. Projecting a projection is a no-op (the rule is
// positional and idempotent, see prefixEnd), which lets callers hand
// already-projected context straight back - the summarize side-query rides
// the same code path.
func ProjectContext(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		switch roleOf(m) {
		case "exit":
			// Transcript bookkeeping, not model context (see Sushi.requestBody).
		case RoleCompaction:
			// The anchor replaces the turns it elided with its summary, and
			// can never cut into the prefix: an earlier summary inside the
			// elided middle is superseded (the new side-query saw it), while
			// one inside the kept tail rides along verbatim.
			head := prefixEnd(out)
			n := int(numExtra(m, ExtraKeptN))
			if n > len(out)-head {
				n = len(out) - head
			}
			rebuilt := make([]Message, 0, head+n+1)
			rebuilt = append(rebuilt, out[:head]...)
			rebuilt = append(rebuilt, SummaryTurn(m))
			rebuilt = append(rebuilt, out[len(out)-n:]...)
			out = rebuilt
		default:
			out = append(out, m)
		}
	}
	return out
}

// prefixEnd is how many leading projected turns a compaction may never
// elide: the prompt plumbing - the system prompt and the rendered instance
// template, which are exactly the rows a fresh run appends without an extra
// map and the rows Transcript() replays without one. The system prompt stays
// first because the server's chat template hoists leading system text; the
// task text stays verbatim because a paraphrase of the request is how agents
// drift off-task. Synthesized summaries are NOT prefix: a newer summary
// supersedes any older one its side-query covered, so summaries roll instead
// of stacking. A summary whose predecessor sits inside the kept tail stays -
// it still stands for elided history the new summary never covers.
func prefixEnd(msgs []Message) int {
	head := 0
	for head < len(msgs) {
		if _, has := msgs[head]["extra"]; !has {
			head++
			continue
		}
		break
	}
	return head
}

// isSyntheticSummary reports whether a projected turn is a compaction's
// synthesized summary (never a stored row: stored compaction rows carry their
// own role, and only ProjectContext stamps this marker).
func isSyntheticSummary(m Message) bool {
	extra, _ := m["extra"].(map[string]any)
	return extra["synthetic"] == "compaction_summary"
}

// SummaryTurn synthesizes the user-role message a compaction row becomes on
// the wire: the stored summary wrapped in constant framing.
func SummaryTurn(m Message) Message {
	content, _ := m["content"].(string)
	return Message{
		"role":    "user",
		"content": SummaryTurnText(content),
		// Marks the synthesized turn for the estimator's anchor reset;
		// extra never reaches the wire (requestBody strips it).
		"extra": map[string]any{"synthetic": "compaction_summary"},
	}
}

// SummaryTurnText is the wire text of a compaction row's synthesized summary
// turn; exposed so the loop can estimate the rebuilt context exactly as the
// projection will emit it.
func SummaryTurnText(content string) string {
	return summaryHead + content + summaryTail
}

// ProjectContextHead reports how many leading turns of a projected context
// the prefix rule protects from ever being elided (see prefixEnd); a
// compaction whose cut would fall inside it has nothing to compact.
func ProjectContextHead(msgs []Message) int { return prefixEnd(msgs) }

// IsCompaction reports whether a message is a compaction anchor row.
func IsCompaction(m Message) bool { return roleOf(m) == RoleCompaction }

func roleOf(m Message) string {
	r, _ := m["role"].(string)
	return r
}

// numExtra reads one of a compaction row's integer-ish extra keys: live rows
// carry Go ints (RecordMessage JSON-marshals them; Transcript decodes them
// back as float64), replayed rows carry float64.
func numExtra(m Message, key string) float64 {
	extra, _ := m["extra"].(map[string]any)
	switch v := extra[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// ------------------------------------------------------------- estimation --

// EstimateTextTokens is the tokenizer-free estimate: bytes above space
// (plus newlines, which tokenize) divided by four - zero's measured shape,
// beating naive len/4 on mixed code/prose. It is the stopgap for turns
// appended since the last usage report; where the model reports token
// counts, ContextTokens uses those directly.
func EstimateTextTokens(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c > ' ' || c == '\n' {
			n++
		}
	}
	return (n + 3) / 4
}

const messageFrameTokens = 8

// EstimateMessageTokens sizes one message as the server would see it: the
// wire-visible text (content, round-tripped reasoning, the tool_calls array)
// plus a flat per-turn framing allowance.
func EstimateMessageTokens(m Message) int {
	t := messageFrameTokens
	if c, _ := m["content"].(string); c != "" {
		t += EstimateTextTokens(c)
	}
	if r, _ := m["reasoning_content"].(string); r != "" {
		t += EstimateTextTokens(r)
	}
	if tc, ok := m["tool_calls"]; ok && tc != nil {
		if b, err := json.Marshal(tc); err == nil {
			t += EstimateTextTokens(string(b))
		}
	}
	return t
}

// ContextTokens sizes a projected context the way the harnesses that run
// local servers learned to: the newest assistant turn's REPORTED usage
// (total_tokens already covers the whole prompt plus that reply) anchored at
// its position, plus the estimate of everything appended after it. Usage
// from before a compaction is stale by construction — it counted a context
// that no longer projects — so a synthesized summary turn resets the anchor:
// only turns at or after the newest anchor can be covered by reported usage.
// With no reported usage at all the return is a full estimate; reported says
// whether an anchor was found.
func ContextTokens(proj []Message) (tokens int, reported bool) {
	anchor := -1
	for i, m := range proj {
		if extra, _ := m["extra"].(map[string]any); extra["synthetic"] == "compaction_summary" {
			// Usage left of an anchor is history of a context that no longer
			// projects: reset, and let the estimate below cover what survives.
			anchor = -1
			tokens = 0
			reported = false
		}
		if roleOf(m) != "assistant" {
			continue
		}
		if u := usageOf(m); u != nil {
			if tt := num(u["total_tokens"]); tt > 0 {
				anchor = i
				tokens = int(tt)
				reported = true
			}
		}
	}
	for i := anchor + 1; i < len(proj); i++ {
		tokens += EstimateMessageTokens(proj[i])
	}
	return
}

// usageOf digs the usage object out of a live or replayed assistant turn.
func usageOf(m Message) map[string]any {
	extra, _ := m["extra"].(map[string]any)
	if extra == nil {
		return nil
	}
	if resp, _ := extra["response"].(map[string]any); resp != nil {
		if u, _ := resp["usage"].(map[string]any); u != nil {
			return u
		}
	}
	if u, _ := extra["model_stats"].(map[string]any); u != nil {
		return u
	}
	return nil
}

// ------------------------------------------------------------ overflow ----

// overflowTells are the substrings servers use to say the prompt did not
// fit: the llama.cpp-family wordings (sushi included), the OpenAI spellings,
// and the DashScope phrasing. Case-insensitive against the error message.
var overflowTells = []string{
	"exceeds the available context",
	"exceeds the context",
	"exceeds model context",
	"context length exceeded",
	"context_length_exceeded",
	"maximum context length",
	"context window",
	"context size",
	"prompt is too long",
	"too long for the model",
	"too many tokens",
	"range of input length",
	"reduce the length",
}

// IsContextOverflow classifies a model error as the prompt not fitting, the
// way pi and zero do from error strings: the reactive compaction path fires
// on this and nothing else. A 413 is a byte-shaped yes.
func IsContextOverflow(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	if api.Status == http.StatusRequestEntityTooLarge {
		return true
	}
	msg := strings.ToLower(api.Message)
	for _, tell := range overflowTells {
		if strings.Contains(msg, tell) {
			return true
		}
	}
	return false
}
