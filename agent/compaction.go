package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/VictorLowther/mini/model"
)

// Compaction in mini is a rare, singular event (docs/compaction-research.md
// §4): while a session lives its context is append-only — nothing here ever
// rewrites turns that are already in the server's prompt cache. When the
// projected context reaches the trigger, one side-query asks the model to
// checkpoint its own conversation, riding the byte-identical live prefix
// (model.Summarizer), and a single compaction row anchors the new smaller
// projection. The full transcript, with every raw output in the result
// files, remains untouched in the store and on disk.

// maxCompactFailures is the circuit breaker: that many consecutive failed
// summarize calls turn automatic compaction off for the rest of the run
// (manual /compact still works) rather than paying for a failing side-query
// on every step.
const maxCompactFailures = 3

// maxOverflowCompactions bounds the reactive path: if the server keeps
// rejecting the prompt after that many forced compactions, the error is not
// compaction's to fix.
const maxOverflowCompactions = 3

// compactionDirective is the side-query's user turn: it must be the ONLY
// appended turn, so the request's prefix stays byte-identical to the live
// one, and it must fully specify the output. The section skeleton is pi's
// structured checkpoint, with the file/command ledger duties merged in -
// mini keeps no plan tool, so the summary itself is the state document.
const compactionDirective = `Your context window is about to be compacted. Write the checkpoint summary that you — a fresh instance who will keep only this summary plus the most recent turns verbatim — continue the work from.

Output the summary directly, in exactly this structure:

## Goal
## Constraints & Preferences
## Progress
### Done
### In Progress
### Blocked
## Key Decisions
## Files & Commands
## Current State
## Next Steps
## Critical Context

Rules:
- Do not call any tool. Output only the summary text.
- The most recent turns will be kept verbatim; spend your detail on everything before them.
- If an earlier part of this conversation was already summarized, treat that summary as established fact and carry every still-relevant detail forward.
- Preserve exact file paths, function names, commands, ids, and error messages. Never guess what you cannot recall.
- "Files & Commands" lists every file created or modified (with the key change) and the commands whose outcomes still matter.
- "Current State" says precisely what was in flight at the end; "Next Steps" is the ordered list to resume from.
- "Critical Context" holds the facts that are expensive to rediscover: environment quirks, exact error strings, values under test.
- Be dense. Omit conversational filler.`

// compactThreshold is the projected-context size at which automatic
// compaction fires; 0 means automatic compaction is off (no window
// configured, or disabled), matching crush's protection for models whose
// window is unknown.
func (c *Config) compactThreshold() int {
	if !c.CompactEnabled || c.ContextWindow <= 0 {
		return 0
	}
	ratio := c.CompactThresholdRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 0.9
	}
	return int(float64(c.ContextWindow) * ratio)
}

// RequestCompaction asks the loop to compact before the next model call no
// matter what the estimate says. Safe from any goroutine; the GUI wires it
// to /compact. A second request while one is pending is a no-op, and a
// request with no run in flight does nothing until the next Query.
func (l *Loop) RequestCompaction() {
	l.mu.Lock()
	l.manualCompact = true
	l.mu.Unlock()
}

// prepareContext returns the projected context for the next model call and
// its estimated size, running compaction first when the estimate has crossed
// the trigger (or the user asked for one). It runs inside Query, before the
// call counter and the model request, so compaction lands at the same
// tool-loop boundary every step - the little-coder lesson is that an
// autonomous run must never wait for a user-prompt boundary to notice it has
// outgrown its window.
func (l *Loop) prepareContext(ctx context.Context) ([]model.Message, int) {
	proj := model.ProjectContext(l.Messages)
	est, reported := model.ContextTokens(proj)
	// Reported usage wins outright (sushi answers every completion with
	// token counts); the chars/4 estimate is only the stopgap for turns
	// appended since the last report.
	l.logger().Debug("context estimate", "tokens", est, "reported", reported)

	l.mu.Lock()
	manual := l.manualCompact
	l.manualCompact = false
	l.mu.Unlock()

	threshold := l.Cfg.compactThreshold()
	if manual || (threshold > 0 && est >= threshold && !l.autoCompactOff) {
		if l.compact(ctx, proj, est, manual) {
			proj = model.ProjectContext(l.Messages)
			est, _ = model.ContextTokens(proj)
		}
	}
	l.lastCtxTokens = est
	return proj, est
}

// compact runs the one compaction event. It returns true only when a
// compaction row was appended to the transcript; every failure path leaves
// the history exactly as it was - mini never drops messages on a failed
// compaction (crush's rule, and the right one for an append-only store).
// manual marks the event as user-requested, which re-arms the request if the
// turn is cancelled before it completes.
func (l *Loop) compact(ctx context.Context, proj []model.Message, estBefore int, manual bool) bool {
	sm, ok := l.Model.(model.Summarizer)
	if !ok {
		l.logger().Warn("compaction skipped: this model cannot summarize")
		return false
	}
	keep := l.Cfg.CompactKeepTokens
	if keep <= 0 {
		keep = 40000
	}
	cut := pickCut(proj, keep)
	head := model.ProjectContextHead(proj)
	if cut <= head {
		// The whole conversation after the prompt plumbing fits in the tail
		// budget: there is nothing to summarize. pi calls this "session too
		// small"; the manual path reports it, the auto path cannot reach it
		// (est would be under the trigger).
		l.logger().Warn("compaction: nothing to compact", "turns", len(proj), "cut", cut, "head", head)
		return false
	}

	summary, err := sm.Summarize(ctx, proj, compactionDirective, l.Cfg.CompactSummaryMaxTokens)
	if err != nil {
		if ctx.Err() != nil {
			// The user cancelled the compaction itself (esc): the history is
			// untouched and nobody is to blame - no breaker strike, and an
			// explicit /compact request stays armed for the next turn.
			if manual {
				l.RequestCompaction()
			}
			l.logger().Info("compaction cancelled", "err", err)
			return false
		}
		l.compactFails++
		if l.compactFails >= maxCompactFailures {
			l.autoCompactOff = true
			l.logger().Error("compaction disabled for this run after repeated summarizing failures",
				"failures", l.compactFails, "last_err", err)
		} else {
			l.logger().Warn("compaction: summarizing failed; history unchanged", "err", err)
		}
		return false
	}
	l.compactFails = 0

	content := summary
	if ledger := outputLedger(proj[head:cut]); ledger != "" {
		content += "\n\n" + ledger
	}
	kept := len(proj) - cut
	estAfter := estimateRebuild(proj, cut, content)
	// Progress guard (omp's recovery band, kept minimal): a compaction that
	// cannot land the context back under its trigger would only re-fire next
	// step and repeat the work. That means the window/keep/summary budget is
	// misconfigured, which a human has to hear about once, loudly.
	if threshold := l.Cfg.compactThreshold(); threshold > 0 && estAfter >= threshold {
		l.autoCompactOff = true
		l.logger().Error("compaction would not fit the context under its trigger; automatic compaction stopped - check context_window, compact_threshold_ratio, compact_keep_tokens",
			"estimated_after", estAfter, "threshold", threshold)
		return false
	}

	firstKept := turnOf(proj[cut])
	l.addMessages(model.Message{
		"role":    model.RoleCompaction,
		"content": content,
		"extra": map[string]any{
			model.ExtraKeptN:           kept,
			model.ExtraFirstKeptTurn:   firstKept,
			model.ExtraTokensBefore:    estBefore,
			model.ExtraTokensAfter:     estAfter,
			model.ExtraCompactionModel: l.modelName(),
			"timestamp":                float64(time.Now().UnixMilli()) / 1000,
		},
	})
	l.logger().Info("session compacted",
		"tokens_before", estBefore, "tokens_after", estAfter, "kept_turns", kept)
	return true
}

// pickCut chooses the first kept turn: walk backwards accumulating estimated
// message tokens until the tail budget is spent, then walk BACK to the
// nearest assistant turn - the tail may only grow, and it must grow past
// whole tool groups, never into one. Landing on an assistant is always
// legal: observations follow their issuing turn (the store's placeholder
// slots are adjacent to it), so keeping everything from an assistant row
// never splits a call from its result, and the turn ahead is the summary or
// the plumbing, keeping roles alternating for the server's chat template.
func pickCut(proj []model.Message, keepTokens int) int {
	acc, start := 0, 0
	for j := len(proj) - 1; j >= 0; j-- {
		acc += model.EstimateMessageTokens(proj[j])
		if acc >= keepTokens {
			start = j
			break
		}
	}
	// The budget never tipped: everything already fits the tail.
	for j := start; j >= 0; j-- {
		if role, _ := proj[j]["role"].(string); role == "assistant" {
			return j
		}
	}
	// Nothing assistant at or before the budget point (a newest group made
	// only of human-mode rows): advance to the next assistant boundary.
	for j := start; j < len(proj); j++ {
		if role, _ := proj[j]["role"].(string); role == "assistant" {
			return j
		}
	}
	return len(proj) // no assistant at all: nothing survives verbatim
}

// estimateRebuild sizes the context the compaction is about to produce:
// everything the anchor elides stays out, the summary turn comes in, the
// kept tail rides along.
func estimateRebuild(proj []model.Message, cut int, content string) int {
	t := model.EstimateTextTokens(model.SummaryTurnText(content))
	// Only the prefix (before the elided middle) and the tail survive; the
	// prefix is small and the tail is proj[cut:], so estimate those plus the
	// new summary turn.
	head := model.ProjectContextHead(proj)
	if head > cut {
		head = cut
	}
	for i := 0; i < head; i++ {
		t += model.EstimateMessageTokens(proj[i])
	}
	for i := cut; i < len(proj); i++ {
		t += model.EstimateMessageTokens(proj[i])
	}
	return t
}

// outputLedger turns the elided span's tool results into the list of disk
// addresses the summary closes with. mini's raw outputs were never lost -
// they are the result files - so the ledger hands the post-compaction model
// its memory back without spending a prompt token on it: this is the same
// output_path promise the system prompt already makes, kept alive across the
// compaction boundary.
func outputLedger(middle []model.Message) string {
	type entry struct{ command, path string }
	var kept []entry
	for _, m := range middle {
		if !model.IsToolResult(m) {
			continue
		}
		extra, _ := m["extra"].(map[string]any)
		path, _ := extra["tcr_path"].(string)
		if path == "" {
			continue
		}
		command, _ := extra["command"].(string)
		kept = append(kept, entry{briefCommand(command), path})
	}
	if len(kept) == 0 {
		return ""
	}
	const maxLedger = 60
	var b strings.Builder
	b.WriteString("## Earlier command outputs (complete files on disk)\n")
	if skip := len(kept) - maxLedger; skip > 0 {
		fmt.Fprintf(&b, "(%d older outputs elided)\n", skip)
		kept = kept[skip:]
	}
	for _, e := range kept {
		if e.command == "" {
			fmt.Fprintf(&b, "- %s\n", e.path)
			continue
		}
		fmt.Fprintf(&b, "- `%s` -> %s\n", e.command, e.path)
	}
	return strings.TrimRight(b.String(), "\n")
}

// briefCommand folds a command to one compact line for the ledger.
func briefCommand(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return s
}

// turnOf reads a projected message's stamped store seq (extra["turn"]),
// written the way RecordMessage and Transcript leave it (int64 or float64).
func turnOf(m model.Message) int64 {
	extra, _ := m["extra"].(map[string]any)
	switch n := extra["turn"].(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// modelName labels the compaction row with the model that wrote the summary.
func (l *Loop) modelName() string {
	if l.Model == nil {
		return ""
	}
	if s, _ := l.Model.TemplateVars()["model_name"].(string); s != "" {
		return s
	}
	return ""
}
