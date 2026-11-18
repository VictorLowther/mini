package session

import (
	"testing"

	"github.com/VictorLowther/mini/model"
)

// A-5. A stream that emits a call with no id (partial or fragmenting servers do)
// used to shift every later slot: entries were filtered, then stamped with their
// position in the filtered slice, while OpenToolCall looks rows up by the *action*
// index. So call 0 inherited call 1's id, result file and tcno, call 1 got a
// second row, and the pre-allocated slot stayed "aborted" forever - a transcript
// that lies, written at record time so nothing can repair it later.
func TestCallSlotsKeepTheirWireIndexes(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)

	msg := model.Message{
		"role":    "assistant",
		"content": "two calls, the first with no id",
		"tool_calls": []any{
			map[string]any{"function": map[string]any{
				"name": "bash", "arguments": `{"command":"echo AAA"}`}},
			map[string]any{"id": "X", "function": map[string]any{
				"name": "bash", "arguments": `{"command":"echo BBB"}`}},
		},
		"extra": map[string]any{"actions": []model.Action{
			{Command: "echo AAA"},
			{Command: "echo BBB", ToolCallID: "X"},
		}},
	}
	seq := record(t, sess, msg)

	// The ledger must hold exactly one row for this turn, at the index the
	// server put the call at.
	var (
		count int
		idx   int
		id    string
		tcno  int
	)
	if err := st.db.QueryRow(`SELECT count(*) FROM tool_calls WHERE session_id = ? AND turn = ?`,
		sess.ID.String(), seq).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pre-allocated %d slots, want 1 (the id-less call needs none)", count)
	}
	if err := st.db.QueryRow(`SELECT idx, id, tcno FROM tool_calls WHERE session_id = ? AND turn = ?`,
		sess.ID.String(), seq).Scan(&idx, &id, &tcno); err != nil {
		t.Fatal(err)
	}
	if id != "X" || idx != 1 {
		t.Fatalf("slot = (idx %d, id %q), want (idx 1, id X)", idx, id)
	}

	// Now the loop's own view, action by action.
	aaa, err := sess.OpenToolCall(msg, 0, model.Action{Command: "echo AAA"})
	if err != nil {
		t.Fatal(err)
	}
	bbb, err := sess.OpenToolCall(msg, 1, model.Action{Command: "echo BBB", ToolCallID: "X"})
	if err != nil {
		t.Fatal(err)
	}

	if bbb.Number != tcno {
		t.Errorf("BBB resolved to tcno %d, want the slot %d stamped for its id", bbb.Number, tcno)
	}
	if aaa.Number == bbb.Number {
		t.Fatalf("both actions share tcno %d: AAA claimed X's slot", aaa.Number)
	}
	if aaa.Path == bbb.Path {
		t.Fatalf("both actions write the same result file %q", aaa.Path)
	}
}

// The same re-indexing without a wire array: human-mode actions carry no id, so
// an id-bearing action that is not first must still find its own slot.
func TestCallSlotsFromActionsKeepTheirIndexes(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)

	actions := []model.Action{
		{Command: "echo no-id"},                   // index 0, no slot
		{Command: "echo first", ToolCallID: "A"},  // index 1
		{Command: "echo second", ToolCallID: "B"}, // index 2
	}
	msg := model.Message{
		"role":    "assistant",
		"content": "actions, no tool_calls array",
		"extra":   map[string]any{"actions": actions},
	}
	seq := record(t, sess, msg)

	rows, err := st.db.Query(`SELECT idx, id FROM tool_calls WHERE session_id = ? AND turn = ? ORDER BY idx`,
		sess.ID.String(), seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var i int
		var id string
		if err := rows.Scan(&i, &id); err != nil {
			t.Fatal(err)
		}
		got[i] = id
	}
	if len(got) != 2 || got[1] != "A" || got[2] != "B" {
		t.Fatalf("slots = %v, want {1:A 2:B}", got)
	}
}
