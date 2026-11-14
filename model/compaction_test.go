package model

import (
	"context"
	"strings"
	"testing"
)

// m builds a wire-ish message.
func m(role, content string, extra map[string]any) Message {
	out := Message{"role": role, "content": content}
	if extra != nil {
		out["extra"] = extra
	}
	return out
}

func compactionRow(summary string, keptN int) Message {
	return Message{
		"role":    RoleCompaction,
		"content": summary,
		"extra":   map[string]any{ExtraKeptN: keptN},
	}
}

func roles(msgs []Message) []string {
	var out []string
	for _, x := range msgs {
		out = append(out, roleOf(x))
	}
	return out
}

func TestProjectContextWithoutAnchor(t *testing.T) {
	msgs := []Message{
		m("system", "s", nil),
		m("user", "task", nil),
		m("assistant", "hi", map[string]any{"actions": []Action{}}),
		m("exit", "Submitted", map[string]any{"exit_status": "Submitted"}),
		m("user", "again", map[string]any{"interrupt_type": "UserNewTask"}),
	}
	got := ProjectContext(msgs)
	want := []string{"system", "user", "assistant", "user"}
	if strings.Join(roles(got), ",") != strings.Join(want, ",") {
		t.Errorf("roles = %v, want %v", roles(got), want)
	}
}

func TestProjectContextAnchor(t *testing.T) {
	msgs := []Message{
		m("system", "s", nil),
		m("user", "task", nil),
		m("assistant", "old work", map[string]any{"turn": 3}),
		m("tool", "old result", map[string]any{"turn": 4}),
		m("user", "steer", map[string]any{"turn": 5}),
		m("assistant", "recent", map[string]any{"turn": 6}),
		m("tool", "recent result", map[string]any{"turn": 7}),
		compactionRow("THE SUMMARY", 2),
		m("assistant", "after", map[string]any{"turn": 9}),
	}
	got := ProjectContext(msgs)
	// kept_n=2 keeps only the last two projected turns; the summary stands
	// in for everything between the plumbing and them.
	wantContent := []string{"s", "task", "THE SUMMARY", "recent", "recent result", "after"}
	if len(got) != len(wantContent) {
		t.Fatalf("projected %d turns %v, want %d", len(got), roles(got), len(wantContent))
	}
	for i, want := range wantContent {
		if c, _ := got[i]["content"].(string); !strings.Contains(c, want) {
			t.Errorf("turn %d = %q, want it to contain %q", i, c, want)
		}
	}
	// The synthesized turn is user-role and wrapped with the constant
	// framing; the stored row's role never reaches the projection.
	synth := got[2]
	if roleOf(synth) != "user" {
		t.Errorf("summary turn role = %q, want user", roleOf(synth))
	}
	content, _ := synth["content"].(string)
	if !strings.HasPrefix(content, summaryHead) || !strings.HasSuffix(content, summaryTail) {
		t.Errorf("summary turn lacks the constant wrapper: %q", content[:min(60, len(content))])
	}
	// Idempotent: projecting a projection is the identity - the summarize
	// side-query depends on this (it sends the projected context).
	if strings.Join(roles(ProjectContext(got)), ",") != strings.Join(roles(got), ",") {
		t.Error("ProjectContext is not idempotent")
	}
	for i := range got {
		c1, _ := got[i]["content"].(string)
		c2, _ := ProjectContext(got)[i]["content"].(string)
		if c1 != c2 {
			t.Errorf("reprojection changed turn %d's bytes", i)
		}
	}
}

func TestProjectContextStackedAnchors(t *testing.T) {
	// Anchor 1 elides turns 3-8; anchor 2 (larger tail budget) reaches back
	// into turns anchor 1 kept. Its summary must replace the old summary,
	// and nothing may resurrect the first elided middle.
	msgs := []Message{
		m("system", "s", nil),
		m("user", "task", nil),
		m("assistant", "A", map[string]any{"turn": 3}),
		m("assistant", "B", map[string]any{"turn": 4}),
		m("assistant", "C", map[string]any{"turn": 5}),
		m("assistant", "D", map[string]any{"turn": 6}),
		compactionRow("SUMMARY ONE", 2), // keeps C, D
		m("assistant", "E", map[string]any{"turn": 8}),
		compactionRow("SUMMARY TWO", 3), // keeps B? no: projected space tail is B? see below
	}
	got := ProjectContext(msgs)
	// After anchor 1: [s, task, S1, C, D, E]. Anchor 2 keeps the last 3 of
	// [s, task, S1, C, D, E] = [C, D, E]; the prefix rule keeps the plumbing
	// and S1's position is inside the elided middle (it precedes the kept
	// tail only if it is outside the kept_n window - it is), so S1 vanishes
	// and S2 replaces it: exactly one summary in the context.
	var summaries []string
	for _, x := range got {
		if isSyntheticSummary(x) {
			c, _ := x["content"].(string)
			summaries = append(summaries, c)
		}
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0], "SUMMARY TWO") {
		t.Errorf("summaries = %v, want exactly the second one", summaries)
	}
	for _, x := range got {
		c, _ := x["content"].(string)
		if strings.Contains(c, "SUMMARY ONE") {
			t.Error("the superseded summary survived the second anchor")
		}
	}
	want := []string{"s", "task", "SUMMARY TWO", "C", "D", "E"}
	if len(got) != len(want) {
		t.Fatalf("got %d turns: %v", len(got), contents(got))
	}
	for i, w := range want {
		if !strings.Contains(got[i]["content"].(string), w) {
			t.Errorf("turn %d = %v, want %q", i, got[i]["content"], w)
		}
	}
}

func contents(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		c, _ := m["content"].(string)
		out = append(out, c)
	}
	return out
}

func TestProjectContextKeptNBeyondHistory(t *testing.T) {
	msgs := []Message{m("system", "s", nil), m("user", "t", nil), compactionRow("S", 99)}
	got := ProjectContext(msgs)
	// The anchor can never cut into the prefix: the kept count clamps to
	// zero and the summary simply trails the plumbing.
	if len(got) != 3 || !isSyntheticSummary(got[2]) {
		t.Fatalf("got %d turns %v, want plumbing then the summary", len(got), roles(got))
	}
}

func withUsage(total float64) map[string]any {
	return map[string]any{"response": map[string]any{
		"usage": map[string]any{"total_tokens": total},
	}}
}

func TestContextTokensUsesReportedUsage(t *testing.T) {
	proj := []Message{
		m("system", strings.Repeat("x", 4000), nil),                         // would estimate ~1000
		m("assistant", "reply", withUsage(12345)),                           // the anchor
		m("tool", strings.Repeat("y", 800), map[string]any{"command": "y"}), // ~200 + frame
	}
	tokens, reported := ContextTokens(proj)
	if !reported {
		t.Fatal("reported = false, want the usage anchor to win")
	}
	if tokens < 12345 || tokens > 12345+300 {
		t.Errorf("tokens = %d, want 12345 plus a ~200-token estimate of the appended turn", tokens)
	}
}

func TestContextTokensAnchorResetsAtCompaction(t *testing.T) {
	// Usage from before a compaction counted a context that no longer
	// projects: it must not anchor the estimate.
	proj := []Message{
		m("system", "s", nil),
		m("assistant", "old", withUsage(500000)),
		SummaryTurn(compactionRow("S", 1)),
		m("assistant", "new", nil),
	}
	tokens, reported := ContextTokens(proj)
	if reported {
		t.Errorf("stale usage anchored the estimate: tokens = %d", tokens)
	}
	if tokens > 1000 {
		t.Errorf("tokens = %d, want a small estimate over the post-anchor turns", tokens)
	}
}

func TestContextTokensNoUsage(t *testing.T) {
	proj := []Message{m("system", strings.Repeat("q", 400), nil), m("user", strings.Repeat("w", 400), nil)}
	tokens, reported := ContextTokens(proj)
	if reported || tokens <= 200 {
		t.Errorf("tokens = %d (reported %v), want a full estimate over 800 chars", tokens, reported)
	}
}

func TestIsContextOverflow(t *testing.T) {
	yes := []*APIError{
		{Status: 400, Message: "the request exceeds the available context size, try increasing it"}, // llama.cpp/sushi
		{Status: 400, Message: "This model's maximum context length is 262144 tokens"},              // OpenAI
		{Status: 400, Type: "context_length_exceeded", Message: "too many tokens"},
		{Status: 413, Message: "payload too large"},
		{Message: "Range of input length should be [1, 262144]"}, // DashScope spelling, upper case
	}
	for _, e := range yes {
		if !IsContextOverflow(e) {
			t.Errorf("missed overflow: %v", e)
		}
	}
	no := []error{
		&APIError{Status: 500, Message: "boom"},
		&APIError{Status: 401, Message: "unauthorized"},
		context.DeadlineExceeded,
	}
	for _, e := range no {
		if IsContextOverflow(e) {
			t.Errorf("false overflow: %v", e)
		}
	}
}

func TestSummarizeSharesTheLivePrefix(t *testing.T) {
	// The cache-reading invariant: the summarize request carries the exact
	// message array the next real turn will send, plus one directive turn -
	// nothing slimmed, reordered, or rewritten.
	srv, requests, _ := newServer(t, "toolcall.sse", "submit.sse")
	s := newSushi(t, srv.URL)
	msgs := []Message{
		m("system", "s", nil),
		m("user", "task", nil),
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": `{"command":"ls"}`},
		}}, "extra": map[string]any{}},
		{"role": "tool", "content": "file list", "tool_call_id": "c1", "extra": map[string]any{}},
		m("exit", "x", map[string]any{"exit_status": "X"}),
		m("user", "steer", map[string]any{"interrupt_type": "UserSteering"}),
	}
	proj := ProjectContext(msgs)
	if _, err := s.Query(context.Background(), proj, nil); err != nil {
		t.Fatal(err)
	}
	text, err := s.Summarize(context.Background(), proj, "SUMMARIZE NOW", 1234)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "task is complete") {
		t.Errorf("summary text = %q", text)
	}
	var main, side Request
	main = (*requests)[0]
	side = (*requests)[1]
	if len(side.Messages) != len(main.Messages)+1 {
		t.Fatalf("side query carried %d messages, want the main %d plus the directive",
			len(side.Messages), len(main.Messages))
	}
	for i := range main.Messages {
		if side.Messages[i]["role"] != main.Messages[i]["role"] ||
			side.Messages[i]["content"] != main.Messages[i]["content"] {
			t.Fatalf("message %d differs: the side query would miss the cached prefix", i)
		}
	}
	last := side.Messages[len(side.Messages)-1]
	if last["role"] != "user" || last["content"] != "SUMMARIZE NOW" {
		t.Errorf("directive turn = %v", last)
	}
	if side.MaxTokens != 1234 {
		t.Errorf("max_tokens = %d, want the summary budget override", side.MaxTokens)
	}
}

func TestSummarizeRejectsToolReplies(t *testing.T) {
	srv, _, _ := newServer(t, "toolcall.sse")
	s := newSushi(t, srv.URL)
	_, err := s.Summarize(context.Background(), []Message{m("system", "s", nil)}, "d", 100)
	if err == nil || !strings.Contains(err.Error(), "tool call") {
		t.Errorf("err = %v, want the tool-call rejection", err)
	}
}

func TestSummarizeRejectsTruncatedSummaries(t *testing.T) {
	srv, _, _ := newServer(t, "length_limit.sse")
	s := newSushi(t, srv.URL)
	_, err := s.Summarize(context.Background(), []Message{m("system", "s", nil)}, "d", 100)
	if err == nil {
		t.Fatal("a summary cut at the output limit must fail, not truncate")
	}
}

// The reported-usage anchor must survive a usage map built in Go, not just one
// decoded from JSON. An int that num() failed to read would silently fall back
// to the chars/4 estimate, which runs ~1.4x low on tool-heavy sessions - and
// the trigger would fire late by exactly that much.
func TestContextTokensReadsIntegerUsage(t *testing.T) {
	for _, usage := range []map[string]any{
		{"total_tokens": 9000},
		{"total_tokens": int64(9000)},
		{"total_tokens": 9000.0},
	} {
		proj := []Message{
			m("system", strings.Repeat("x", 4000), nil),
			m("assistant", "reply", map[string]any{"model_stats": usage}),
		}
		tokens, reported := ContextTokens(proj)
		if !reported || tokens != 9000 {
			t.Errorf("usage %v: got %d reported=%v, want 9000 reported=true", usage, tokens, reported)
		}
	}
}
