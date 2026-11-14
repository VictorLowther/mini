package session

import (
	"strings"
	"testing"

	"github.com/VictorLowther/mini/model"
)

// A compaction row is a first-class transcript record: it stores and replays
// like any other turn, and ProjectContext turns the replayed transcript into
// exactly the same wire context the live run sent after the same anchor.
func TestCompactionRowRoundTrips(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	record(t, sess, model.Message{"role": "system", "content": "sys"})
	record(t, sess, model.Message{"role": "user", "content": "task"})
	assist := record(t, sess, model.Message{
		"role": "assistant", "content": "a1",
		"tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": `{"command":"echo hi"}`}}},
		"extra": map[string]any{"actions": []model.Action{{Command: "echo hi", ToolCallID: "call_1"}}},
	})
	tc, err := sess.OpenToolCall(mustMsg(t, sess, assist), 0, model.Action{Command: "echo hi", ToolCallID: "call_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.File.WriteString("hi\n"); err != nil {
		t.Fatal(err)
	}
	_, offset, elided, err := model.ViewOf(tc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.File.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sess.CompleteToolCall(tc, model.ToolOutcome{Bytes: 3, ViewOffset: offset, ViewElided: elided, ReturnCode: 0}); err != nil {
		t.Fatal(err)
	}
	obs := record(t, sess, model.Message{
		"role": "tool", "content": `{"output":"hi"}`, "tool_call_id": "call_1",
		"extra": map[string]any{"command": "echo hi", "returncode": 0, "tcno": tc.Number},
	})
	comp := record(t, sess, model.Message{
		"role": model.RoleCompaction, "content": "## Goal\nthe summary",
		"extra": map[string]any{
			model.ExtraKeptN:         2,
			model.ExtraTokensBefore:  4684,
			model.ExtraTokensAfter:   1818,
			model.ExtraFirstKeptTurn: assist,
		},
	})

	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("transcript has %d turns, want 5: %v", len(msgs), msgs)
	}
	row := msgs[4]
	if !model.IsCompaction(row) {
		t.Fatalf("role = %v, want compaction", row["role"])
	}
	extra := row["extra"].(map[string]any)
	if extra["turn"].(int64) != comp {
		t.Errorf("compaction row did not stamp its turn: %v", extra["turn"])
	}
	if obs == comp {
		t.Error("seq collision")
	}
	// The projection of the replayed transcript is the live wire shape:
	// plumbing, synthesized summary, kept tail; the anchor row itself and
	// any exit rows never appear.
	proj := model.ProjectContext(msgs)
	for _, m := range proj {
		if r, _ := m["role"].(string); r == model.RoleCompaction || r == "exit" {
			t.Fatalf("projection leaked role %q", r)
		}
	}
	if len(proj) != 5 || !strings.Contains(proj[2]["content"].(string), "the summary") {
		t.Errorf("projection = %d turns, want [system user summary assistant tool]", len(proj))
	}
	if role, _ := proj[2]["role"].(string); role != "user" {
		t.Errorf("summary turn role = %q, want user", role)
	}
}
