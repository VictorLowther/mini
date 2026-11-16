package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// The compaction tests drive the loop with model.NewDeterministic, whose
// canned turns carry real tool_calls arrays (ids call_N), so the store
// pre-allocates slots exactly as a live run does and the projected context
// exercises the full assistant+tool group structure.

// compactionConfig builds the small-window config the tests compact against:
// turns are ~1550 estimated tokens (bigTurns), so with window 5000 the
// trigger (4500) is crossed exactly once, comfortably clear of both sides.
func compactionConfig(t *testing.T, mutate ...func(*Config)) *Config {
	t.Helper()
	c := agentConfig(t)
	c.SystemTemplate = "s"
	c.InstanceTemplate = "{{.task}}"
	c.ContextWindow = 5000
	c.CompactKeepTokens = 60
	c.CompactSummaryMaxTokens = 100
	for _, fn := range mutate {
		fn(c)
	}
	return c
}

func compactionLoop(t *testing.T, d *model.Deterministic, mutate ...func(*Config)) (*Loop, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{dir: t.TempDir()}
	return &Loop{
		Cfg:      compactionConfig(t, mutate...),
		Model:    d,
		Env:      &env.Local{Timeout: 5 * time.Second},
		Recorder: rec,
	}, rec
}

// bigTurns scripts steps that each add ~1550 estimated tokens: a 3000-char
// command in the tool_calls array plus a 3000-char echoed result view.
func bigTurns(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("working %d\x00echo %s", i, strings.Repeat("x", 3000)))
	}
	return out
}

func det(n int, summaries ...string) *model.Deterministic {
	d := model.NewDeterministic(bigTurns(n)...)
	d.Outputs = append(d.Outputs, done("final answer"))
	d.Summaries = summaries
	return d
}

// assertWireLegal checks the chat-completions invariants the sushi server
// enforces: only wire roles, the system prompt leads, and every tool result
// answers a call in the assistant turn immediately ahead of it.
func assertWireLegal(t *testing.T, msgs []model.Message) {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("empty context")
	}
	if r, _ := msgs[0]["role"].(string); r != "system" {
		t.Fatalf("first wire turn is %q, want system", r)
	}
	var pending []string
	for i, m := range msgs {
		role, _ := m["role"].(string)
		switch role {
		case "tool":
			if len(pending) == 0 {
				t.Fatalf("turn %d: tool result with no open calls: %v", i, rolesOf(msgs))
			}
			id, _ := m["tool_call_id"].(string)
			if id == "" || id != pending[0] {
				t.Fatalf("turn %d: tool result %q does not answer the oldest open call %q", i, id, pending)
			}
			pending = pending[1:]
		case "exit", "compaction":
			t.Fatalf("turn %d: role %q must never reach the wire", i, role)
		default:
			pending = nil
			if role == "assistant" {
				for _, raw := range toolCallsOf(m) {
					tc, _ := raw.(map[string]any)
					id, _ := tc["id"].(string)
					pending = append(pending, id)
				}
			}
		}
	}
}

// toolCallsOf normalizes the two shapes a turn's tool_calls array arrives in:
// []any from JSON decoding, []map[string]any from the canned fixtures.
func toolCallsOf(m model.Message) []any {
	switch t := m["tool_calls"].(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, 0, len(t))
		for _, tc := range t {
			out = append(out, tc)
		}
		return out
	}
	return nil
}

func rolesOf(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		r, _ := m["role"].(string)
		out = append(out, r)
	}
	return out
}

func TestCompactionFiresAtTriggerAndProjects(t *testing.T) {
	d := det(4, "## Goal\nCarry on. Paths are in the ledger.")
	l, rec := compactionLoop(t, d)
	info, err := run(t, l, "the task")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != model.KindSubmitted {
		t.Fatalf("exit_status = %v", info)
	}

	// Exactly one compaction row landed in the transcript, and no later
	// turn count (5 steps) crossed the trigger a second time: the rebuild
	// is small enough for the rest of the run.
	var rows []model.Message
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			rows = append(rows, m)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("%d compaction rows, want 1 (turns: %v)", len(rows), rolesOf(rec.seen))
	}
	row := rows[0]
	extra, _ := row["extra"].(map[string]any)
	if kept := extraInt(extra[model.ExtraKeptN]); kept < 1 {
		t.Errorf("kept_n = %v, want at least one turn", extra[model.ExtraKeptN])
	}
	if extraInt(extra[model.ExtraTokensBefore]) < l.Cfg.compactThreshold() {
		t.Errorf("tokens_before %v never crossed the trigger", extra[model.ExtraTokensBefore])
	}
	if _, ok := extra[model.ExtraFirstKeptTurn]; !ok {
		t.Error("compaction row lacks first_kept_turn")
	}

	// Queries: the compacted call is the fourth, and its input is the
	// projected context - plumbing, the synthesized summary, then the kept
	// tail. It must be SHORTER than the call before it, and legal.
	if len(d.Queries) != 5 {
		t.Fatalf("%d queries, want 5", len(d.Queries))
	}
	before, after := d.Queries[2], d.Queries[3]
	if len(after) >= len(before) {
		t.Fatalf("post-compaction context not smaller: %d turns before, %d after: %v",
			len(before), len(after), rolesOf(after))
	}
	synth := after[2]
	content, _ := synth["content"].(string)
	if role, _ := synth["role"].(string); role != "user" || !strings.Contains(content, "Carry on") {
		t.Fatalf("turn 2 = %v, want the synthesized summary user turn", synth)
	}
	for _, q := range d.Queries {
		assertWireLegal(t, q)
	}
	// The summarize side-query rode the full pre-compaction projection: the
	// context the next turn would have sent, elided and kept alike.
	if len(d.SummarizeQueries) != 1 {
		t.Fatalf("%d summarize calls, want 1", len(d.SummarizeQueries))
	}
	if got := len(d.SummarizeQueries[0]); got != len(before)+2 {
		t.Errorf("side query carried %d turns, want %d (the last turn's pair included)", got, len(before)+2)
	}
}

// manualHooks is the steering seam the manual test uses to arm a compaction
// the moment the first observation lands.
type manualHooks struct {
	l    *Loop
	n    int
	once bool
}

func (h *manualHooks) BeforeExecute(context.Context, []model.Action, map[string]any) error {
	return nil
}
func (h *manualHooks) OnSubmit(context.Context, model.Message) error             { return nil }
func (h *manualHooks) OnLimits(context.Context, *Config, *model.Interrupt) error { return nil }
func (h *manualHooks) NextHumanCommand(context.Context) (string, bool)           { return "", false }
func (h *manualHooks) OnInterrupt(context.Context) error                         { return nil }
func (h *manualHooks) OnMessage(m model.Message) {
	// Arm after the SECOND observation: with a single assistant group there
	// is no elidable middle, and "nothing to compact" must not be confused
	// with the manual path working.
	if h.once || !model.IsToolResult(m) {
		return
	}
	h.n++
	if h.n < 2 {
		return
	}
	h.once = true
	h.l.RequestCompaction()
}

func TestManualCompactIgnoresThreshold(t *testing.T) {
	d := det(3, "## Goal\nmanual checkpoint")
	l, rec := compactionLoop(t, d, func(c *Config) { c.ContextWindow = 0 }) // auto off
	l.Hooks = &manualHooks{l: l}
	if _, err := run(t, l, "task"); err != nil {
		t.Fatal(err)
	}
	// With no window configured, only the manual request compacts - and it
	// compacts even though the estimate is far under any threshold.
	var n int
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d compaction rows, want the manual 1 (window was 0)", n)
	}
	if len(d.SummarizeQueries) != 1 {
		t.Errorf("%d side queries, want 1", len(d.SummarizeQueries))
	}
}

func TestCompactionNothingToCompact(t *testing.T) {
	d := det(3, "unused")
	l, rec := compactionLoop(t, d, func(c *Config) { c.CompactKeepTokens = 1_000_000 })
	if _, err := run(t, l, "task"); err != nil {
		t.Fatal(err)
	}
	// A tail budget larger than the conversation leaves no elidable middle:
	// the loop must not ask for a summary at all (pi's "session too small").
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			t.Fatal("compacted despite nothing to compact")
		}
	}
	if len(d.SummarizeQueries) != 0 {
		t.Errorf("%d side queries, want 0 - the guard must run BEFORE paying for the call", len(d.SummarizeQueries))
	}
}

// extraInt reads an extra count the way the loop writes it (int) or the way
// a store replay returns it (float64).
func extraInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func TestSummarizerFailureBreaker(t *testing.T) {
	d := det(5) // no summaries: every side-query errors
	l, rec := compactionLoop(t, d)
	if _, err := run(t, l, "task"); err != nil {
		t.Fatal(err) // the run itself must survive compaction failures
	}
	// The trigger is crossed from the second query on, so the breaker spends
	// exactly its strikes and never more: three failed side-queries, then
	// automatic compaction is off for the rest of the run.
	if len(d.SummarizeQueries) != maxCompactFailures {
		t.Errorf("%d side queries, want exactly %d before the breaker trips",
			len(d.SummarizeQueries), maxCompactFailures)
	}
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			t.Fatal("a failed compaction must never append a row")
		}
	}
	if l.lastRole() != "exit" {
		t.Error("the run should have completed after the breaker stopped compaction")
	}
}

func TestCompactionProgressGuard(t *testing.T) {
	// A summary so large that the rebuilt context would still cross the
	// trigger is a misconfiguration, not a compaction: it must be refused
	// (once) and stop automatic attempts, never committed.
	d := det(4, strings.Repeat("enormous summary ", 2000))
	l, rec := compactionLoop(t, d, func(c *Config) { c.ContextWindow = 2000 })
	if _, err := run(t, l, "task"); err != nil {
		t.Fatal(err)
	}
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			t.Fatal("the no-progress compaction was committed anyway")
		}
	}
	if len(d.SummarizeQueries) != 1 {
		t.Errorf("%d side queries, want 1 - the guard must disable automatic compaction", len(d.SummarizeQueries))
	}
}

// overflowOnce answers the Nth query with a llama.cpp-shaped context-length
// rejection, then behaves normally.
type overflowOnce struct {
	*model.Deterministic
	at, n int
	fired bool
}

func (o *overflowOnce) Query(ctx context.Context, msgs []model.Message, h *model.Handler) (model.Message, error) {
	o.n++
	if o.n == o.at && !o.fired {
		o.fired = true
		return nil, &model.APIError{Status: 400, Type: "invalid_request_error",
			Message: "This model's maximum context length is 262144 tokens, however you requested 300000"}
	}
	return o.Deterministic.Query(ctx, msgs, h)
}

func TestReactiveCompactionOnOverflow(t *testing.T) {
	inner := det(3, "## Goal\nreactive checkpoint")
	l, rec := compactionLoop(t, inner, func(c *Config) { c.ContextWindow = 0 }) // auto never fires
	l.Model = &overflowOnce{Deterministic: inner, at: 3}
	info, err := run(t, l, "task")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != model.KindSubmitted {
		t.Fatalf("the overflow must have been recovered, not end the run: %v, %v", info, err)
	}
	var n int
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d compaction rows, want the reactive 1", n)
	}
	if l.overflowBudget != maxOverflowCompactions-1 {
		t.Errorf("overflow budget %d, want one spent", l.overflowBudget)
	}
	for _, q := range inner.Queries {
		assertWireLegal(t, q)
	}
}

func TestPickCutLandsOnAssistantGroups(t *testing.T) {
	proj := []model.Message{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": "a1", "extra": map[string]any{}},
		{"role": "tool", "content": strings.Repeat("t", 4000), "extra": map[string]any{}},
		{"role": "user", "content": "steer", "extra": map[string]any{}},
		{"role": "assistant", "content": "a2", "extra": map[string]any{}},
		{"role": "tool", "content": strings.Repeat("t", 4000), "extra": map[string]any{}},
	}
	if cut := pickCut(proj, 10); proj[cut]["role"] != "assistant" || cut != 5 {
		// The newest tool result alone blows the budget; the cut walks back
		// to the assistant that issued it, so the tail is the whole last
		// group (a tail may only grow, and only across complete groups).
		t.Errorf("cut = %d (%v), want the assistant at 5", cut, rolesOf(proj))
	}
	if cut := pickCut(proj, 1_000_000); cut != 2 {
		t.Errorf("infinite budget: cut = %d, want the first assistant", cut)
	}
	humanOnly := []model.Message{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "task"},
		{"role": "user", "content": "plain", "extra": map[string]any{}},
	}
	if cut := pickCut(humanOnly, 1); cut != len(humanOnly) {
		t.Errorf("no assistant in tail: cut = %d, want %d (keep nothing)", cut, len(humanOnly))
	}
}

func TestConfigThresholds(t *testing.T) {
	c := &Config{ContextWindow: 1000, CompactEnabled: true, CompactThresholdRatio: 0.9}
	if got := c.compactThreshold(); got != 900 {
		t.Errorf("threshold = %d, want 900", got)
	}
	c.ContextWindow = 0
	if got := c.compactThreshold(); got != 0 {
		t.Errorf("windowless threshold = %d, want 0 (auto off, crush's rule)", got)
	}
	c.ContextWindow, c.CompactEnabled = 1000, false
	if got := c.compactThreshold(); got != 0 {
		t.Errorf("disabled threshold = %d, want 0", got)
	}
	c.CompactEnabled, c.CompactThresholdRatio = true, 42.0
	if got := c.compactThreshold(); got != 900 {
		t.Errorf("absurd ratio fell to %d, want the 0.9 default", got)
	}
	// The embedded config ships a real window, which is the fallback the CLI
	// uses when the server declines to report one (model.DiscoverContextWindow
	// asks /v1/models first); 0 here would leave only the built-in default.
	m, err := config.FromSpec("mini")
	if err != nil {
		t.Fatal(err)
	}
	if got := ConfigFromMap(config.Sub(m, "agent")).ContextWindow; got == 0 {
		t.Error("mini.yaml sets no context_window; a silent server would leave only the default")
	}
}

// TestCheckWindowParams: a run whose window cannot hold the tail plus the
// summary says so at startup instead of quietly never compacting. This became a
// live concern when the window started arriving from the server: the config
// file a human set compact_keep_tokens in no longer shows the number it has to
// fit under.
func TestCheckWindowParams(t *testing.T) {
	warns := func(c *Config) string {
		var buf strings.Builder
		l := &Loop{Cfg: c, Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
		l.checkWindowParams()
		return buf.String()
	}
	shipped := func(window int) *Config {
		return &Config{ContextWindow: window, CompactEnabled: true, CompactThresholdRatio: 0.9,
			CompactKeepTokens: 40000, CompactSummaryMaxTokens: 16384}
	}
	if out := warns(shipped(262144)); out != "" {
		t.Errorf("the shipped budgets against their own window warned: %s", out)
	}
	if out := warns(shipped(1048576)); out != "" {
		t.Errorf("a bigger window warned: %s", out)
	}
	// 8192 leaves a trigger of 7372, under the 56384 the compaction needs.
	out := warns(shipped(8192))
	for _, want := range []string{"no headroom", "context_window=8192", "keep_tokens=40000"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning %q missing %q", out, want)
		}
	}
	if out := warns(shipped(0)); out != "" {
		t.Errorf("an unresolved window warned; automatic compaction is already off: %s", out)
	}
	c := shipped(8192)
	c.CompactEnabled = false
	if out := warns(c); out != "" {
		t.Errorf("compaction the operator switched off warned: %s", out)
	}
}

// reportedLoop builds a transcript whose newest assistant turn carries a
// server-reported usage far above what chars/4 measures - the real ratio
// against sushi measured 1.4x on tool-output-heavy sessions - and returns it
// ready for a direct compact() call.
func reportedLoop(t *testing.T, d *model.Deterministic, reported int, mutate ...func(*Config)) (*Loop, *fakeRecorder) {
	t.Helper()
	l, rec := compactionLoop(t, d, mutate...)
	big := strings.Repeat("o", 400)
	l.Messages = []model.Message{
		{"role": "system", "content": strings.Repeat("s", 2400)},
		{"role": "user", "content": "the task"},
		{"role": "assistant", "content": "elided middle", "extra": map[string]any{"turn": int64(3)}},
		{"role": "tool", "content": big, "extra": map[string]any{
			"turn": int64(4), "command": "echo middle", "tcr_path": "/tmp/middle.log"}},
		{"role": "assistant", "content": "kept tail", "extra": map[string]any{
			"turn":        int64(5),
			"model_stats": map[string]any{"total_tokens": float64(reported)}}},
		{"role": "tool", "content": big, "extra": map[string]any{"turn": int64(6)}},
	}
	return l, rec
}

// The two numbers on a compaction row must be in the same unit. Reported usage
// and a chars/4 estimate differ by ~1.4x in practice, and the difference is
// exactly what let a compaction that shrank nothing look like a 30% cut - and
// slipped past the progress guard.
func TestCompactionScoresTheRebuildInTheReportedBasis(t *testing.T) {
	d := det(0, "## Goal\nshort checkpoint")
	l, _ := reportedLoop(t, d, 8000, func(c *Config) {
		c.ContextWindow = 2000 // threshold 1000, under the reported context
		c.CompactThresholdRatio = 0.5
		c.CompactKeepTokens = 60
	})
	proj := model.ProjectContext(l.Messages)
	estBefore, reported := model.ContextTokens(proj)
	if !reported {
		t.Fatal("the fixture lost its reported-usage anchor")
	}
	if l.compact(context.Background(), proj, estBefore, false) {
		t.Fatal("a rebuild whose reported-basis size still crosses the trigger must be refused")
	}
	if !l.autoCompactOff {
		t.Error("the refused rebuild must stop automatic compaction")
	}

	// The same rebuild, measured in the estimator's own units, is well under
	// that threshold: scoring in the reported basis is what catches it.
	raw := estimateRebuild(proj, pickCut(proj, 60), "## Goal\nshort checkpoint")
	if threshold := l.Cfg.compactThreshold(); raw >= threshold {
		t.Fatalf("fixture is not discriminating: raw %d already exceeds %d", raw, threshold)
	}
}

// And when the rebuild does fit, the row must carry the reported-basis number:
// the GUI card compares tokens_before with tokens_after, and printing a
// reported count against a raw estimate is what once advertised "64k -> 44k"
// for a compaction that saved about thirty tokens.
func TestCompactionRowReportsTheReportedBasis(t *testing.T) {
	d := det(0, "## Goal\nshort checkpoint")
	l, _ := reportedLoop(t, d, 8000, func(c *Config) {
		c.ContextWindow = 20000 // threshold 10000: the scaled rebuild fits
		c.CompactThresholdRatio = 0.5
		c.CompactKeepTokens = 60
	})
	proj := model.ProjectContext(l.Messages)
	estBefore, _ := model.ContextTokens(proj)
	raw := estimateRebuild(proj, pickCut(proj, 60), "## Goal\nshort checkpoint")
	if !l.compact(context.Background(), proj, estBefore, false) {
		t.Fatal("a rebuild that fits under its trigger in reported units should commit")
	}
	var row model.Message
	for _, m := range l.Messages {
		if model.IsCompaction(m) {
			row = m
		}
	}
	extra, _ := row["extra"].(map[string]any)
	after := extraInt(extra[model.ExtraTokensAfter])
	before := extraInt(extra[model.ExtraTokensBefore])
	if after <= raw {
		t.Errorf("tokens_after %d was not raised into the reported basis (raw estimate %d)", after, raw)
	}
	if after >= before {
		t.Errorf("card claims %d -> %d, which is no saving at all", before, after)
	}
}

// A manual /compact early in a session summarizes a handful of turns into a
// summary of the same size. The threshold guard cannot see that (both numbers
// sit far under it, which is exactly how a real 64k-token /compact once traded
// 11 turns for a summary worth 12), so the shrink guard must.
func TestCompactionRefusesASummaryThatDoesNotShrink(t *testing.T) {
	d := det(2, "## Goal\n"+strings.Repeat("a summary as long as what it replaced. ", 200))
	l, rec := compactionLoop(t, d, func(c *Config) { c.ContextWindow = 0 }) // threshold 0: no guard but this one
	l.Hooks = &manualHooks{l: l}
	if _, err := run(t, l, "task"); err != nil {
		t.Fatal(err)
	}
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			t.Fatal("committed a compaction that made the context larger")
		}
	}
	if len(d.SummarizeQueries) != 1 {
		t.Errorf("%d side queries, want 1", len(d.SummarizeQueries))
	}
	// A user asking once says nothing about what a full window would do, so a
	// manual refusal must leave automatic compaction armed.
	if l.autoCompactOff {
		t.Error("a manual no-op switched automatic compaction off")
	}
}

// The same no-progress outcome on the automatic path must switch compaction
// off: the estimate is unchanged, so the trigger would fire and pay for the
// same side-query on every remaining step.
func TestCompactionNonShrinkStopsTheAutomaticPath(t *testing.T) {
	d := det(4, "## Goal\n"+strings.Repeat("a summary as long as what it replaced. ", 200))
	l, rec := compactionLoop(t, d, func(c *Config) {
		c.ContextWindow = 3000 // crossed once, after the second big turn
		c.CompactThresholdRatio = 0.9
	})
	if _, err := run(t, l, "task"); err != nil {
		t.Fatal(err)
	}
	for _, m := range rec.seen {
		if model.IsCompaction(m) {
			t.Fatal("committed a compaction that made the context larger")
		}
	}
	if !l.autoCompactOff {
		t.Error("automatic compaction stayed armed after a no-progress compaction")
	}
	if n := len(d.SummarizeQueries); n != 1 {
		t.Errorf("%d side queries, want 1 - the run must not re-pay for the same summary", n)
	}
}
