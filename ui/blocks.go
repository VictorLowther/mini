package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// This file is the transcript model every frontend shares: the Fyne GUI
// renders it as cards, the GUI's resume replay walks it message by message,
// and the string shapes (headers, notes) are defined here so no two surfaces
// can drift.

// streamState accumulates the text chunks of the generation in flight.
type streamState struct {
	reasoning strings.Builder
	content   strings.Builder
}

// metrics names the generation facts of one message that a frontend header
// may show: generated tokens, wall seconds of the model call, and its cost.
// Zero means "not recorded", and frontends omit those parts.
type metrics struct {
	Tokens  int
	Elapsed float64
	Cost    float64
}

// block is one entry in the transcript. kind is a message role, or one of
// "note" / "error" for UI-generated lines. Blocks keep their source text so
// the view can be re-rendered when the thinking toggle flips.
type block struct {
	kind  string
	title string
	text  string
	// reasoning is the model's thinking text for this turn, shown only while
	// the thinking toggle is on.
	reasoning string
	// transcript holds the rendered shell lines of an executed command.
	transcript []string
	// stats are the message's generation metrics (assistant turns).
	stats metrics
	// subtype is the message's interrupt_type for user-role rows: frontends
	// use it to tell entries the human typed (steering, human commands) from
	// feedback the harness authored (rejections, interruption records,
	// format-error prompts, new-task wrappers).
	subtype string
	// tcid is the tool call this card tracks, and "" for everything that is not
	// a tool card. It is what lets the card drawn when a call is *issued* be the
	// same card its result lands in, instead of a second one appearing below it.
	tcid string
	// cmd is the command the card is about, kept because a provider that omits
	// tool-call ids (and every scripted model) still must not end up with two
	// cards for one call: the created card and its result are matched by command
	// when there is no id to match on.
	cmd string
	// state is where a tool card is in its life: "created" when the model asked
	// for it, "running" while it or its job is going, "finished" once its
	// observation arrived, and "ended" for a call the run stopped without
	// answering. Non-tool cards leave it "".
	state string
	// log is the file a running call is appending its output to, and "" once it
	// has stopped. The loop hands it over when it opens the call's slot, which is
	// the only moment a foreground command has a live path to show - after the
	// call there is only the result.
	log string
	// job is the id this card is tracking for a call that handed off, and ""
	// for every other block. The view refreshes cards that carry one.
	job string
}

// messageBlock is the message→block mapping every frontend shares (GUI live,
// GUI resume, replay): prompt plumbing never shows, the user row without a
// built instance is the task note, assistant rows carry the per-turn header,
// and tool results render from the stored observation. The caller supplies
// the header (it is per-frontend state), reads task for the note, and owns
// the stream reset that committing an assistant turn performs.
func messageBlock(m model.Message, header, task string, clearStream func(), jobs JobReader) (b block, show, taskNote bool) {
	role, _ := m["role"].(string)
	content, _ := m["content"].(string)
	if _, built := m["extra"]; !built {
		return block{}, false, role == "user"
	}
	b = block{kind: role, text: content}
	if r, ok := m["reasoning_content"].(string); ok {
		b.reasoning = r
	}
	switch role {
	case "assistant":
		b.title = header
		if extra, ok := m["extra"].(map[string]any); ok {
			b.stats.Cost, _ = extra["cost"].(float64)
			b.stats.Elapsed, _ = extra["elapsed_seconds"].(float64)
			if u, ok := extra["model_stats"].(map[string]any); ok {
				b.stats.Tokens = asIntDefault(u["completion_tokens"])
			}
		}
		clearStream()
	case "tool", "user":
		if extra, ok := m["extra"].(map[string]any); ok {
			b.subtype, _ = extra["interrupt_type"].(string)
		}
		if id, ok := m["tool_call_id"].(string); ok {
			b.tcid = id
		}
		if extra, ok := m["extra"].(map[string]any); ok {
			b.cmd, _ = extra["command"].(string)
		}
		if obs, ok := observationOf(m); ok {
			if obs.Pending && obs.JobID != "" {
				// A call that handed off is not finished, so the card tracks
				// the job instead of printing an exit status it never got.
				b.job = obs.JobID
				b.transcript = pendingCardLines(obs, jobs)
			} else {
				b.transcript = obs.lines()
			}
			b.state = "running"
			if !obs.Pending {
				b.state = "finished"
			}
		}
	case "exit":
		extra, _ := m["extra"].(map[string]any)
		status, _ := extra["exit_status"].(string)
		b.title = status
	case "compaction":
		// The visual transcript never restarts (omp's divider lesson): the
		// anchor shows as its own card carrying the summary, and the tokens it
		// traded away ride in the title.
		extra, _ := m["extra"].(map[string]any)
		b.title = compactionTitle(extra)
	}
	return b, true, false
}

// compactionTitle is the compaction card's header: what it traded, and which
// model wrote the summary.
func compactionTitle(extra map[string]any) string {
	before := asIntDefault(extra["tokens_before"])
	after := asIntDefault(extra["tokens_after"])
	title := "context compacted"
	if before > 0 && after > 0 {
		title = fmt.Sprintf("context compacted · %dk → %dk tok", before/1000, after/1000)
	}
	if via, _ := extra["compaction_model"].(string); via != "" {
		title += " · " + via
	}
	return title
}

// assistantHeader and runStatsText are the shared string shapes every
// frontend renders identically; retryNotice is the shared stalled-call note.
func assistantHeader(calls int, cost float64) string {
	return fmt.Sprintf("mini-swe-agent (step %d, $%.2f)", calls, cost)
}

func retryNotice(attempt, max int, err error, wait time.Duration) string {
	return fmt.Sprintf("model call failed (attempt %d/%d), retrying in %s: %v", attempt, max, wait.Round(time.Second), err)
}

func runStatsText(snap agent.Snapshot, mode string, thinking bool, contextWindow int) string {
	parts := []string{fmt.Sprintf("step %d", snap.Calls), fmt.Sprintf("$%.2f", snap.Cost), mode,
		map[bool]string{true: "thinking: on", false: "thinking: off"}[thinking]}
	if snap.ModelTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok", snap.ModelTokens))
	}
	if snap.ModelTokPerSec > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", snap.ModelTokPerSec))
	}
	// The compaction trigger's number, shown before the model's own stats:
	// this is the one that decides whether the next turn re-reads a warm
	// prefix or walks toward a compaction.
	if contextWindow > 0 && snap.CtxTokens > 0 {
		pct := 100 * snap.CtxTokens / contextWindow
		part := fmt.Sprintf("ctx %dk (%d%%)", snap.CtxTokens/1000, pct)
		if pct >= 100 {
			part = fmt.Sprintf("ctx %dk OVER", snap.CtxTokens/1000)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "  ·  ")
}

// sessionHeaderLine is the transcript's first line, worded identically by
// every frontend. contextNote is the resolved context window and who reported
// it, so a screenshot of a session says which extent it was run against.
func sessionHeaderLine(modelName, baseURL, configName, sessionID, contextNote string) string {
	parts := []string{fmt.Sprintf("mini-go %s · %s @ %s · config %s",
		agent.Version, modelName, baseURL, configName)}
	if contextNote != "" {
		parts = append(parts, contextNote)
	}
	if sessionID != "" {
		parts = append(parts, "session "+sessionID)
	}
	return strings.Join(parts, " · ")
}

// The confirm prompt shows at most this many actions; the rest is elided with
// a count.
const panelMaxActions = 4

// panelConfirmHint is the confirm panel's key legend, shared by every frontend.
const panelConfirmHint = "enter to confirm · type a comment and enter to reject · /y to go yolo · /h for commands"

// panelActionLines is the confirm panel's content, free of styling and width
// handling: the first panelMaxActions actions (each truncated to 6 lines, which
// may make one entry span lines) plus an elision marker when actions are hidden.
func panelActionLines(actions []model.Action) []string {
	var lines []string
	shown := actions
	if len(shown) > panelMaxActions {
		lines = append(lines, fmt.Sprintf("+%d more commands", len(shown)-panelMaxActions))
		shown = shown[:panelMaxActions]
	}
	for _, a := range shown {
		lines = append(lines, truncateVisible(a.Display(), 6))
	}
	return lines
}

// truncateVisible keeps long command output from flooding a prompt panel.
func truncateVisible(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n… %d more lines", len(lines)-maxLines)
}

// collapseSpaces makes running text safe to wrap: model output arrives with
// soft line breaks sprinkled through it, and re-wrapping hard newlines
// mid-sentence is what made the thinking block ragged.
func collapseSpaces(s string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}
