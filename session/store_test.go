package session

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"uuid"

	"github.com/VictorLowther/mini/model"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sessions.db"), filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func newSession(t *testing.T, s *Store) *Session {
	t.Helper()
	sess, err := s.Create(Row{ID: uuid.NewV7(), Workdir: "/w", Project: "/p", ModelName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func record(t *testing.T, s *Session, m model.Message) int64 {
	t.Helper()
	seq, err := s.RecordMessage(m)
	if err != nil {
		t.Fatalf("record %v: %v", m["role"], err)
	}
	return seq
}

// A tool observation stores its keys and metadata but never the output text:
// results live only in the result files.
func TestTranscriptHoldsNoToolOutput(t *testing.T) {
	st, dir := openStore(t)
	sess := newSession(t, st)
	record(t, sess, model.Message{"role": "system", "content": "sys"})
	assist := record(t, sess, model.Message{"role": "assistant", "content": "thinking", "extra": map[string]any{"actions": []model.Action{{Command: "echo secret-stdout"}}}})

	tc, err := sess.OpenToolCall(mustMsg(t, sess, assist), 0, model.Action{Command: "echo secret-stdout", ToolCallID: "call_1"})
	if err != nil {
		t.Fatal(err)
	}
	out := "secret-stdout\n"
	if _, err := tc.File.WriteString(out); err != nil {
		t.Fatal(err)
	}
	tc.File.Sync()
	tc.File.Close()
	if err := sess.CompleteToolCall(tc, model.ToolOutcome{Bytes: int64(len(out)), ReturnCode: 0, CWD: dir}); err != nil {
		t.Fatal(err)
	}
	obsSeq := record(t, sess, model.Message{
		"role":         "tool",
		"content":      `{"returncode": 0}`,
		"tool_call_id": "call_1",
		"extra":        map[string]any{"command": "echo secret-stdout", "returncode": 0, "tcno": tc.Number},
	})

	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("transcript has %d turns, want 3", len(msgs))
	}
	obs := msgs[2]
	extra := obs["extra"].(map[string]any)
	for _, k := range []string{"command", "returncode", "tcno", "tcr_path", "tcr_offset", "elided_lines", "turn"} {
		if _, ok := extra[k]; !ok {
			t.Errorf("observation extra missing %q", k)
		}
	}
	if extra["turn"] != int64(obsSeq) {
		t.Errorf("extra[turn] = %v, want %d", extra["turn"], obsSeq)
	}
	if strings.Contains(obs["content"].(string), "secret-stdout") {
		t.Error("observation content carries tool output")
	}
	// The database itself must not hold the output anywhere in the turns
	// table: this is what catches a regression that stages results in a
	// column again.
	var all string
	if err := st.db.QueryRow(`SELECT group_concat(content||reasoning||extra||tool_calls) FROM turns`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, "secret-stdout\n") {
		t.Error("turns table contains the tool output text")
	}
}

func mustMsg(t *testing.T, s *Session, seq int64) model.Message {
	t.Helper()
	return model.Message{"role": "assistant", "content": "", "extra": map[string]any{"turn": seq}}
}

// The result file is the complete record, byte for byte; the call row carries
// the execution facts; numbers continue across Session handles.
func TestToolCallLifecycle(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	aSeq := record(t, sess, model.Message{"role": "assistant", "content": "run two", "extra": map[string]any{"actions": []model.Action{{}}}})
	msg := mustMsg(t, sess, aSeq)

	want := []byte("one\n\x1b[31m")
	tc1, err := sess.OpenToolCall(msg, 0, model.Action{Command: "echo one", ToolCallID: "call_1"})
	if err != nil {
		t.Fatal(err)
	}
	if tc1.Number != 1 {
		t.Fatalf("first tcno = %d, want 1", tc1.Number)
	}
	if _, err := tc1.File.Write(want); err != nil {
		t.Fatal(err)
	}
	tc1.File.Sync()
	tc1.File.Close()
	if err := sess.CompleteToolCall(tc1, model.ToolOutcome{Bytes: 6, ViewOffset: 0, ViewElided: 0, ReturnCode: 0, CWD: "/w"}); err != nil {
		t.Fatal(err)
	}
	tc2, err := sess.OpenToolCall(msg, 1, model.Action{Command: "echo two", ToolCallID: "call_2"})
	if err != nil {
		t.Fatal(err)
	}
	if tc2.Number != 2 {
		t.Errorf("second call Number = %d, want 2", tc2.Number)
	}
	tc2.File.Close()

	// A fresh handle on the same row continues the counter at 3.
	again, err := st.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	tc3, err := again.OpenToolCall(msg, 2, model.Action{Command: "echo three"})
	if err != nil {
		t.Fatal(err)
	}
	if tc3.Number != 3 {
		t.Errorf("third call Number = %d, want 3", tc3.Number)
	}
	tc3.File.Close()

	got, err := os.ReadFile(tc1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("result file = %q, want %q (nothing may append or strip)", got, want)
	}

	var (
		status                               string
		id, cwd                              string
		turn, idx, rc, bytes, offset, elided int64
	)
	err = st.db.QueryRow(`SELECT status, id, turn, idx, returncode, cwd, output_bytes, view_offset, view_elided FROM tool_calls WHERE session_id = ? AND tcno = 1`,
		sess.ID.String()).Scan(&status, &id, &turn, &idx, &rc, &cwd, &bytes, &offset, &elided)
	if err != nil {
		t.Fatal(err)
	}
	if status != "completed" || id != "call_1" || turn != aSeq || idx != 0 || rc != 0 || bytes != 6 || offset != 0 || elided != 0 {
		t.Errorf("call row = %q id=%q turn=%d idx=%d rc=%d bytes=%d off=%d el=%d", status, id, turn, idx, rc, bytes, offset, elided)
	}
	// An open call keeps status 'started' until completed.
	var st3 string
	if err := st.db.QueryRow(`SELECT status FROM tool_calls WHERE tcno = 3`).Scan(&st3); err != nil || st3 != "started" {
		t.Errorf("open call status = %q, %v; want started", st3, err)
	}
	if err := again.CompleteToolCall(model.ToolCall{Number: 99}, model.ToolOutcome{}); err == nil || !strings.Contains(err.Error(), "unknown tool call 99") {
		t.Errorf("CompleteToolCall on unknown tcno err = %v", err)
	}
	if err := again.CompleteToolCall(model.ToolCall{}, model.ToolOutcome{}); err != nil {
		t.Errorf("zero ToolCall must be a no-op, got %v", err)
	}
}

// The stored view offset and elided count reach a resumed transcript intact,
// so no later reader recomputes the tail.
func TestStoredViewRoundTrips(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	aSeq := record(t, sess, model.Message{"role": "assistant", "content": "a"})
	tc, err := sess.OpenToolCall(mustMsg(t, sess, aSeq), 0, model.Action{Command: "long", ToolCallID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	tc.File.Close()
	if err := sess.CompleteToolCall(tc, model.ToolOutcome{Bytes: 100, ViewOffset: 4, ViewElided: 1, ReturnCode: 0, CWD: "/w"}); err != nil {
		t.Fatal(err)
	}
	record(t, sess, model.Message{"role": "tool", "content": "obs", "tool_call_id": "c1",
		"extra": map[string]any{"command": "long", "returncode": 0, "tcno": tc.Number}})

	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	extra := msgs[len(msgs)-1]["extra"].(map[string]any)
	if extra["tcr_offset"] != int64(4) {
		t.Errorf("tcr_offset = %v, want int64(4)", extra["tcr_offset"])
	}
	if extra["elided_lines"] != 1 {
		t.Errorf("elided_lines = %v, want 1", extra["elided_lines"])
	}
	if extra["tcno"] != 1 {
		t.Errorf("tcno = %v, want 1", extra["tcno"])
	}
	if want := sess.tcrPath(1); extra["tcr_path"] != want {
		t.Errorf("tcr_path = %v, want %v", extra["tcr_path"], want)
	}
}

func TestOpenToolCallGuards(t *testing.T) {
	st, dir := openStore(t)
	sess := newSession(t, st)

	// No recorded issuing turn: error, no row, no file.
	_, err := sess.OpenToolCall(model.Message{"role": "assistant", "content": "x"}, 0, model.Action{Command: "y"})
	if err == nil || err.Error() != "session store: issuing turn not recorded" {
		t.Fatalf("unrecorded turn err = %v", err)
	}
	var n int
	st.db.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&n)
	if n != 0 {
		t.Errorf("failed OpenToolCall left %d rows", n)
	}

	// Unwritable result root: the row is deleted again. (Only meaningful for
	// non-root test UIDs.)
	if os.Geteuid() != 0 {
		aSeq := record(t, sess, model.Message{"role": "assistant", "content": "a"})
		tcrRoot := filepath.Join(dir, "tcr")
		if err := os.MkdirAll(tcrRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tcrRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(tcrRoot, 0o700) })
		if _, err := sess.OpenToolCall(mustMsg(t, sess, aSeq), 0, model.Action{Command: "z"}); err == nil {
			t.Fatal("OpenToolCall succeeded into an unwritable directory")
		}
		st.db.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&n)
		if n != 0 {
			t.Errorf("failed file open left %d call rows", n)
		}
	}
}

// result_turn lands on the call the observation answers, and only that one.
func TestResultTurnLinksTheRightRow(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	aSeq := record(t, sess, model.Message{"role": "assistant", "content": "two calls"})
	m := mustMsg(t, sess, aSeq)
	tc1, _ := sess.OpenToolCall(m, 0, model.Action{Command: "one", ToolCallID: "call_1"})
	tc2, _ := sess.OpenToolCall(m, 1, model.Action{Command: "two", ToolCallID: "call_2"})
	tc1.File.Close()
	tc2.File.Close()
	obs := record(t, sess, model.Message{"role": "tool", "content": "o1", "tool_call_id": "call_1",
		"extra": map[string]any{"command": "one", "returncode": 0, "tcno": tc1.Number}})

	var rt1, rt2 sql.NullInt64
	st.db.QueryRow(`SELECT result_turn FROM tool_calls WHERE session_id = ? AND turn = ? AND idx = 0`, sess.ID.String(), aSeq).Scan(&rt1)
	st.db.QueryRow(`SELECT result_turn FROM tool_calls WHERE session_id = ? AND turn = ? AND idx = 1`, sess.ID.String(), aSeq).Scan(&rt2)
	if !rt1.Valid || rt1.Int64 != obs {
		t.Errorf("(turn %d, idx 0).result_turn = %v, want %d", aSeq, rt1, obs)
	}
	if rt2.Valid {
		t.Errorf("(turn %d, idx 1).result_turn = %v, want NULL", aSeq, rt2)
	}

	// An observation naming a call that was never opened is an error.
	_, err := sess.RecordMessage(model.Message{"role": "tool", "content": "ghost",
		"extra": map[string]any{"command": "x", "returncode": 0, "tcno": 42}})
	if err == nil || !strings.Contains(err.Error(), "unknown tool call 42") {
		t.Errorf("ghost observation err = %v", err)
	}
}

// All three foreign keys are live: without _pragma=foreign_keys(1) SQLite
// defaults enforcement OFF and every one of these would succeed silently.
func TestForeignKeysAreLive(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	aSeq := record(t, sess, model.Message{"role": "assistant", "content": "a"})
	m := mustMsg(t, sess, aSeq)
	tc, _ := sess.OpenToolCall(m, 0, model.Action{Command: "c", ToolCallID: "call_1"})
	tc.File.Close()

	fk := func(query string, args ...any) {
		t.Helper()
		_, err := st.db.Exec(query, args...)
		if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			t.Errorf("%q err = %v, want FOREIGN KEY constraint failed", query, err)
		}
	}
	fk(`INSERT INTO tool_calls (session_id, turn, idx, tcno, command, created_at) VALUES (?, 999, 0, 77, 'x', 0)`, sess.ID.String())
	fk(`UPDATE tool_calls SET result_turn = 999 WHERE tcno = 1`)
	fk(`INSERT INTO turns (session_id, seq, role, created_at) VALUES ('not-a-session', 1, 'user', 0)`)

	// A NULL result_turn is skipped by the constraint (MATCH SIMPLE): that is
	// what keeps a call without an answer yet out of trouble.
	var n int
	st.db.QueryRow(`SELECT count(*) FROM tool_calls WHERE tcno = 1 AND result_turn IS NULL`).Scan(&n)
	if n != 1 {
		t.Error("NULL result_turn was rejected or the row vanished")
	}
}

// One DELETE on sessions empties both children through the cascades, and the
// result directory goes with it.
func TestDeleteCascades(t *testing.T) {
	st, dir := openStore(t)
	sess := newSession(t, st)
	aSeq := record(t, sess, model.Message{"role": "assistant", "content": "a"})
	tc, _ := sess.OpenToolCall(mustMsg(t, sess, aSeq), 0, model.Action{Command: "c"})
	tc.File.WriteString("data\n")
	tc.File.Close()
	record(t, sess, model.Message{"role": "tool", "content": "o",
		"extra": map[string]any{"command": "c", "returncode": 0, "tcno": tc.Number}})

	if err := st.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := st.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM turns WHERE session_id = ?`, sess.ID.String()); n != 0 {
		t.Errorf("turns after delete = %d", n)
	}
	if n := count(`SELECT count(*) FROM tool_calls WHERE session_id = ?`, sess.ID.String()); n != 0 {
		t.Errorf("tool_calls after delete = %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "tcr", sess.ID.String())); !os.IsNotExist(err) {
		t.Errorf("result dir still present: %v", err)
	}
	if err := st.Delete(uuid.MustParse("00000000-0000-7000-8000-000000000000")); err == nil || !strings.Contains(err.Error(), "unknown session") {
		t.Errorf("Delete unknown err = %v", err)
	}
}

// Deleting an issuing turn takes its call rows with it and leaves the answer
// turn standing: the dependency points calls -> turns, never back.
func TestDeleteIssuingTurnTakesCalls(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	aSeq := record(t, sess, model.Message{"role": "assistant", "content": "a"})
	tc, _ := sess.OpenToolCall(mustMsg(t, sess, aSeq), 0, model.Action{Command: "c"})
	tc.File.Close()
	obs := record(t, sess, model.Message{"role": "tool", "content": "o",
		"extra": map[string]any{"command": "c", "returncode": 0, "tcno": tc.Number}})

	if _, err := st.db.Exec(`DELETE FROM turns WHERE session_id = ? AND seq = ?`, sess.ID.String(), aSeq); err != nil {
		t.Fatal(err)
	}
	var calls int
	st.db.QueryRow(`SELECT count(*) FROM tool_calls WHERE session_id = ?`, sess.ID.String()).Scan(&calls)
	if calls != 0 {
		t.Errorf("call rows after issuing-turn delete = %d, want 0", calls)
	}
	var seqs []int64
	rows, _ := st.db.Query(`SELECT seq FROM turns WHERE session_id = ?`, sess.ID.String())
	defer rows.Close()
	for rows.Next() {
		var v int64
		rows.Scan(&v)
		seqs = append(seqs, v)
	}
	if len(seqs) != 1 || seqs[0] != obs {
		t.Errorf("surviving turns = %v, want just the observation %d", seqs, obs)
	}
}

func TestTitleBackfillAndExitStatus(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	record(t, sess, model.Message{"role": "system", "content": "sys prompt"})
	// The rendered instance message: user, no extra key at all.
	record(t, sess, model.Message{"role": "user", "content": "fix the bug\nthen run the tests"})
	if err := sess.RecordProgress(1.25, 3); err != nil {
		t.Fatal(err)
	}
	record(t, sess, model.Message{"role": "exit", "content": "done",
		"extra": map[string]any{"exit_status": "Submitted", "submission": "done"}})

	got, err := st.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "fix the bug" {
		t.Errorf("title = %q", got.Title)
	}
	if got.ExitStatus != "Submitted" {
		t.Errorf("exit_status = %q", got.ExitStatus)
	}
	if got.Cost != 1.25 || got.NCalls != 3 {
		t.Errorf("progress = %v/%v", got.Cost, got.NCalls)
	}
	// A later task message must not overwrite the title.
	record(t, sess, model.Message{"role": "user", "content": "second task"})
	got, _ = st.Get(sess.ID)
	if got.Title != "fix the bug" {
		t.Errorf("title overwritten: %q", got.Title)
	}
}

func TestFindAndList(t *testing.T) {
	st, _ := openStore(t)
	a := newSession(t, st)
	b, _ := st.Create(Row{ID: uuid.NewV7(), Project: "/other"})
	for _, s := range []*Session{a, b} {
		record(t, s, model.Message{"role": "user", "content": "task for " + s.Project})
	}
	got, err := st.Find("/p", "")
	if err != nil || got.ID != a.ID {
		t.Fatalf("Find exact = %v, %v", got, err)
	}
	prefix := a.ID.String()[:8]
	got, err = st.Find("/p", prefix)
	if err != nil || got.ID != a.ID {
		t.Fatalf("Find prefix = %v, %v", got, err)
	}
	if _, err := st.Find("/p", "ffffffff"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Find missing = %v, want sql.ErrNoRows", err)
	}
	rows, err := st.List("", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != b.ID { // b was touched later: updated_at DESC
		t.Errorf("List all = %+v", rows)
	}
	rows, err = st.List("/p", false, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != a.ID {
		t.Errorf("List project = %+v, %v", rows, err)
	}
}

// assertAdjacency enforces the rule a real OpenAI-compatible server enforces
// with a 400: every tool call in an assistant turn is answered - exactly
// once, immediately, and by nothing else - in the following block of tool
// rows.
func assertAdjacency(t *testing.T, msgs []model.Message) {
	t.Helper()
	for i, m := range msgs {
		raw, _ := m["tool_calls"].([]any)
		if m["role"] != "assistant" || len(raw) == 0 {
			continue
		}
		want := map[string]int{}
		for _, tc := range raw {
			if id, _ := tc.(map[string]any)["id"].(string); id != "" {
				want[id]++
			}
		}
		j := i + 1
		seen := map[string]int{}
		for ; j < len(msgs) && msgs[j]["role"] == "tool"; j++ {
			id, _ := msgs[j]["tool_call_id"].(string)
			seen[id]++
		}
		if len(seen) != len(raw) {
			for _, tc := range raw {
				id, _ := tc.(map[string]any)["id"].(string)
				if seen[id] != 1 {
					t.Errorf("assistant %d: call %s answered %d times (block of %d tool rows)", i, id, seen[id], j-i-1)
				}
			}
		}
	}
}

// An assistant turn is recorded together with its answers: before the first
// command has even started, the transcript is a legal chat-completions
// history, because every call owns a slot that holds an honest aborted
// observation until the real one arrives - and then holds the real one at the
// same seq, so completion cannot reorder history.
func TestSlotsMakeHistoryLegalAtEveryInstant(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	record(t, sess, model.Message{"role": "system", "content": "sys"})
	asst := model.Message{
		"role":    "assistant",
		"content": "",
		"extra": map[string]any{"actions": []model.Action{
			{Command: "one", ToolCallID: "call_1"},
			{Command: "two", ToolCallID: "call_2"},
		}},
		"tool_calls": []map[string]any{
			{"id": "call_1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"one"}`}},
			{"id": "call_2", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"two"}`}},
		},
	}
	record(t, sess, asst)

	// This is what a SIGKILL leaves behind, and it is already legal.
	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("transcript = %d rows, want the 2 turns plus 2 slots", len(msgs))
	}
	assertAdjacency(t, msgs)
	for _, slot := range msgs[2:] {
		ex := slot["extra"].(map[string]any)
		if ex["aborted"] != true {
			t.Errorf("unfilled slot lacks its marker: %v", ex)
		}
		if p, _ := ex["tcr_path"].(string); p != "" {
			t.Errorf("unfilled slot names a result file that was never written: %q", p)
		}
	}

	// Call 1 executes and completes; its observation fills its slot in place.
	tc1, err := sess.OpenToolCall(asst, 0, model.Action{Command: "one", ToolCallID: "call_1"})
	if err != nil {
		t.Fatal(err)
	}
	tc1.File.WriteString("1\n")
	tc1.File.Sync()
	tc1.File.Close()
	if err := sess.CompleteToolCall(tc1, model.ToolOutcome{Bytes: 2, ReturnCode: 0, CWD: "/w"}); err != nil {
		t.Fatal(err)
	}
	slotSeq := msgs[2]["extra"].(map[string]any)["turn"].(int64)
	obsSeq := record(t, sess, model.Message{"role": "tool", "content": `{"returncode": 0}`,
		"tool_call_id": "call_1",
		"extra":        map[string]any{"command": "one", "returncode": 0, "tcno": tc1.Number}})
	if obsSeq != slotSeq {
		t.Errorf("observation took seq %d, want its slot %d: fills must not append", obsSeq, slotSeq)
	}

	// Call 2's batch was interrupted before it opened a file: its padded
	// observation carries no tcno and fills by tool_call_id instead.
	record(t, sess, model.Message{"role": "tool", "content": `{"returncode": -1}`,
		"tool_call_id": "call_2",
		"extra":        map[string]any{"command": "two", "returncode": -1, "exception_info": "action was not executed"}})

	msgs, err = sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("transcript grew to %d rows under fills, want 4", len(msgs))
	}
	assertAdjacency(t, msgs)
	for _, row := range msgs[2:] {
		ex := row["extra"].(map[string]any)
		if ex["aborted"] == true {
			t.Errorf("filled slot kept its marker: %v", ex)
		}
	}
	// The completed call's row still carries its execution facts and the
	// joined view; the interrupted one stays 'started' with result_turn on
	// its (now filled) slot.
	var st1, st2 string
	var rt2 int64
	st.db.QueryRow(`SELECT status FROM tool_calls WHERE id = 'call_1'`).Scan(&st1)
	st.db.QueryRow(`SELECT status, result_turn FROM tool_calls WHERE id = 'call_2'`).Scan(&st2, &rt2)
	if st1 != "completed" {
		t.Errorf("call_1 status = %q", st1)
	}
	if st2 != "started" || rt2 != slotSeq+1 {
		t.Errorf("call_2 = (%q, result_turn %d), want (started, %d)", st2, rt2, slotSeq+1)
	}
}

// A model that issues actions without a provider tool_calls array (the test
// models' shape) gets the array materialized at record time, so replaying a
// resumed transcript sends the same calls the slots answer.
func TestActionsMaterializeTheirCallArray(t *testing.T) {
	st, _ := openStore(t)
	sess := newSession(t, st)
	asst := model.Message{"role": "assistant", "content": "",
		"extra": map[string]any{"actions": []model.Action{{Command: "echo hi", ToolCallID: "c1"}}}}
	if _, err := sess.RecordMessage(asst); err != nil {
		t.Fatal(err)
	}
	if _, ok := asst["tool_calls"]; !ok {
		t.Fatal("the shared message map should have learned its wire array")
	}
	var blob string
	st.db.QueryRow(`SELECT tool_calls FROM turns WHERE role = 'assistant'`).Scan(&blob)
	if !strings.Contains(blob, `"id":"c1"`) || !strings.Contains(blob, "echo hi") {
		t.Errorf("stored array = %s", blob)
	}
	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	assertAdjacency(t, msgs)
}
