package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/VictorLowther/mini/model"
)

// RecordMessage appends one turn to the transcript and returns its seq, the
// single write seam for the history. The seq is stamped into the message's own
// extra map (read-without-creating: model.Extra would add an "extra" key to the
// system and instance messages and make the GUI treat prompt plumbing as agent
// output), which is what lets OpenToolCall name the issuing turn.
//
// Recording an assistant turn that calls tools also pre-allocates the answers:
// one placeholder tool turn per call, in call order, plus its ledger row - in
// one transaction, immediately after the issuing row. The transcript is
// therefore a legal chat-completions history at every instant, including the
// instant the process stops existing: a SIGKILL mid-command leaves the
// placeholder holding an honest "run ended before the result was recorded"
// observation instead of an unanswered call that would brick every future
// resume. A later observation for that call does not append a row, it fills
// its slot (by tcno, or - for padded batches that never opened a file - by
// tool_call_id against the newest unfilled slot): slots fix the order at
// issue time, so completion order can never reorder history.
func (s *Session) RecordMessage(m model.Message) (int64, error) {
	role, _ := m["role"].(string)
	content, _ := m["content"].(string)
	reasoning, _ := m["reasoning_content"].(string)
	callID, _ := m["tool_call_id"].(string)
	extraMap, hasExtra := m["extra"].(map[string]any)

	callsJSON := ""
	if raw, ok := m["tool_calls"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err != nil {
			return 0, fmt.Errorf("session store: encode tool_calls: %w", err)
		}
		callsJSON = string(b)
	}
	// The blob is stored verbatim from the provider wire shape
	// (model/stream.go:172), never re-marshalled from the tool_calls table:
	// it carries fields mini does not model, and calls that never executed
	// have no execution record at all.
	extraJSON := ""
	var marshalErr error
	if hasExtra {
		cp := make(map[string]any, len(extraMap))
		for k, v := range extraMap {
			// "response" is the echoed API body; "turn" is the row's own
			// number, which is never stored twice.
			if k == "response" || k == "turn" {
				continue
			}
			cp[k] = v
		}
		if b, err := json.Marshal(cp); err != nil {
			marshalErr = fmt.Errorf("session store: encode extra: %w", err)
		} else {
			extraJSON = string(b)
		}
	}

	// A tool observation with an allocated slot fills it instead of taking a
	// new seq: the decision must precede the insert below, or the history
	// would carry both the answer's slot and the answer.
	if role == "tool" && marshalErr == nil {
		if seq, ok, err := s.fillSlot(m, content, reasoning, extraJSON); err != nil {
			return 0, err
		} else if ok {
			return seq, nil
		}
	}

	// The calls this assistant turn issued determine everything that follows:
	// the materialized wire array and the slots share one transaction with the
	// issuing row, so no crash window can leave a tool_calls array that
	// outvives its answers.
	var entries []slotEntry
	if role == "assistant" && marshalErr == nil {
		actions, _ := extraMap["actions"].([]model.Action)
		entries = toolCallEntries(m, actions)
		if len(entries) > 0 && callsJSON == "" {
			built := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				args, _ := json.Marshal(map[string]string{"command": e.command})
				built = append(built, map[string]any{
					"id": e.id, "type": "function",
					"function": map[string]any{"name": e.name, "arguments": string(args)},
				})
			}
			if b, err := json.Marshal(built); err == nil {
				callsJSON = string(b)
				m["tool_calls"] = json.RawMessage(b) // the shared map tells the replay the truth
			}
		}
	}

	now := time.Now().UnixMilli()
	if len(entries) > 0 {
		return s.recordCallTurn(extraMap, role, content, reasoning, callsJSON, extraJSON, entries, now)
	}
	var seq int64
	// One statement allocates the next seq (the coalesce(max(seq),0)+1
	// idiom) and returns it; RETURNING also works on UPDATE.
	err := s.store.db.QueryRow(`INSERT INTO turns (session_id, seq, role, content, reasoning, tool_calls, tool_call_id, extra, created_at)
SELECT ?, coalesce(max(seq), 0) + 1, ?, ?, ?, ?, ?, ?, ? FROM turns WHERE session_id = ? RETURNING seq`,
		s.ID.String(), role, content, reasoning, callsJSON, callID, extraJSON, now, s.ID.String()).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("session store: record turn: %w", err)
	}
	if hasExtra {
		extraMap["turn"] = seq
	}

	// The observation answers a call: point its row's result_turn at this
	// turn. Only calls that reached no slot - human-mode rows opened on
	// demand - still take the append path above. A message without tcno
	// performs no update; turns itself never references a call.
	if hasExtra {
		if tcno, ok := callNumber(extraMap["tcno"]); ok {
			res, err := s.store.db.Exec(`UPDATE tool_calls SET result_turn = ? WHERE session_id = ? AND tcno = ?`,
				seq, s.ID.String(), tcno)
			if err != nil {
				return seq, fmt.Errorf("session store: link tool call %d: %w", tcno, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				// The schema cannot represent an observation for a call that
				// was never opened; mini must not pretend otherwise.
				return seq, fmt.Errorf("session store: unknown tool call %d", tcno)
			}
		}
	}

	// One touch per call: updated_at always, exit_status on the exit message,
	// title backfilled only while sessions.title is still empty. The
	// task-carrying user message is exactly the rendered instance_template
	// entry with no extra key — the only history line that holds the task in
	// every flow, including a task the GUI asks for after Create.
	sets := []string{"updated_at = ?"}
	args := []any{now}
	if role == "exit" {
		if status, _ := extraMap["exit_status"].(string); status != "" {
			sets = append(sets, "exit_status = ?")
			args = append(args, status)
		}
	}
	if role == "user" && !hasExtra {
		sets = append(sets, "title = CASE WHEN title = '' THEN ? ELSE title END")
		args = append(args, cappedFirstLine(content))
	}
	args = append(args, s.ID.String())
	if _, err := s.store.db.Exec("UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return seq, fmt.Errorf("session store: touch session: %w", err)
	}
	return seq, marshalErr
}

// abortException is what an unfilled slot tells the model: the run ended
// before the command's result was recorded. The call row's status stays the
// honest execution record; this text is the transcript's.
const abortException = "run ended before the result of this command was recorded"

func abortContent() string {
	b, _ := json.Marshal(map[string]any{
		"returncode":     -1,
		"output":         "",
		"exception_info": abortException,
	})
	return string(b)
}

// recordCallTurn writes an assistant turn and every slot its calls will be
// answered by in one transaction: the issuing row, the ledger rows, and the
// placeholder tool turns commit together or not at all, so a transcript that
// shows a tool_calls array always shows its answers - filled or aborted.
func (s *Session) recordCallTurn(extraMap map[string]any, role, content, reasoning, callsJSON, extraJSON string, entries []slotEntry, now int64) (int64, error) {
	tx, err := s.store.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("session store: begin call turn: %w", err)
	}
	defer tx.Rollback()

	var seq int64
	if err := tx.QueryRow(`INSERT INTO turns (session_id, seq, role, content, reasoning, tool_calls, tool_call_id, extra, created_at)
SELECT ?, coalesce(max(seq), 0) + 1, ?, ?, ?, ?, '', ?, ? FROM turns WHERE session_id = ? RETURNING seq`,
		s.ID.String(), role, content, reasoning, callsJSON, extraJSON, now, s.ID.String()).Scan(&seq); err != nil {
		return 0, fmt.Errorf("session store: record turn: %w", err)
	}
	if extraMap != nil {
		extraMap["turn"] = seq
	}

	var last int64
	if err := tx.QueryRow(`UPDATE sessions SET next_tcno = next_tcno + ?, updated_at = ? WHERE id = ? RETURNING next_tcno`,
		len(entries), now, s.ID.String()).Scan(&last); err != nil {
		return 0, fmt.Errorf("session store: allocate call numbers: %w", err)
	}
	for i, e := range entries {
		tcno := last - int64(len(entries)) + 1 + int64(i)
		abortExtra, _ := json.Marshal(map[string]any{
			"returncode":     -1,
			"exception_info": abortException,
			"command":        e.command,
			"cwd":            "",
			"tcr_path":       "",
			"aborted":        true,
		})
		var slotSeq int64
		if err := tx.QueryRow(`INSERT INTO turns (session_id, seq, role, content, reasoning, tool_calls, tool_call_id, extra, created_at)
SELECT ?, coalesce(max(seq), 0) + 1, 'tool', ?, '', '', ?, ?, ? FROM turns WHERE session_id = ? RETURNING seq`,
			s.ID.String(), abortContent(), e.id, string(abortExtra), now, s.ID.String()).Scan(&slotSeq); err != nil {
			return 0, fmt.Errorf("session store: placeholder for call %d: %w", tcno, err)
		}
		// The ledger row is born answered-where: result_turn names its slot
		// from the start, and completion fills that row instead of appending.
		if _, err := tx.Exec(`INSERT INTO tool_calls (session_id, turn, idx, tcno, id, name, command, status, result_turn, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'started', ?, ?)`,
			s.ID.String(), seq, e.idx, tcno, e.id, e.name, e.command, slotSeq, now); err != nil {
			return 0, fmt.Errorf("session store: ledger for call %d: %w", tcno, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("session store: commit call turn: %w", err)
	}
	return seq, nil
}

// slotEntry is one call to pre-allocate: the id is the server's, kept
// verbatim (rewriting it would desync the replayed history from the server's
// KV-cache prefix); command comes from the call's arguments when they parse
// and the issuing action otherwise.
type slotEntry struct {
	id      string
	name    string
	command string
	// idx is the position of this call in the turn as the loop counts it - the
	// wire array index, or the action index when there is no array. It is not
	// the position within this slice, and the difference is A-5: a call without
	// an id is filtered out below, and if the survivors are stamped with their
	// filtered position, every call after the gap is registered one slot early.
	// OpenToolCall looks rows up by (turn, idx) using the *action* index, so
	// that off-by-one silently pairs each command with another call's result
	// file and tcno, and leaves an id-bearing slot permanently "aborted".
	idx int
}

// toolCallEntries reads the calls this assistant turn issued: the wire array
// when present, otherwise actions that carry a tool_call_id. Entries without
// an id (human-mode commands) are the loop's own inventions, replayed as
// user-role text - they need no slot and no array.
func toolCallEntries(m model.Message, actions []model.Action) []slotEntry {
	var raw []any
	switch t := m["tool_calls"].(type) {
	case []any:
		raw = t
	case []map[string]any:
		raw = make([]any, 0, len(t))
		for _, tc := range t {
			raw = append(raw, tc)
		}
	}
	if len(raw) > 0 {
		var out []slotEntry
		for i, item := range raw {
			tc, _ := item.(map[string]any)
			id, _ := tc["id"].(string)
			if id == "" {
				// Skipped, but the index it occupied is not reclaimed: the
				// calls after it keep the positions the loop will look them up
				// by.
				continue
			}
			e := slotEntry{id: id, idx: i}
			if fn, _ := tc["function"].(map[string]any); fn != nil {
				e.name, _ = fn["name"].(string)
				var args map[string]any
				if err := json.Unmarshal([]byte(asText(fn["arguments"])), &args); err == nil {
					e.command, _ = args["command"].(string)
				}
			}
			if e.name == "" {
				e.name = "bash"
			}
			out = append(out, e)
		}
		return out
	}
	var out []slotEntry
	for i, a := range actions {
		if a.ToolCallID != "" {
			out = append(out, slotEntry{id: a.ToolCallID, name: a.ToolName(), command: a.Command, idx: i})
		}
	}
	return out
}

func asText(v any) string {
	s, _ := v.(string)
	if s == "" {
		s = "null"
	}
	return s
}

// fillSlot lands a tool observation in the row its call pre-allocated. The
// tcno route is exact - it is the loop's own handle on the ledger row. The
// id route serves observations that never opened a file (a rejected or
// interrupted batch, whose calls were never executed but whose slots exist):
// it fills the newest unfilled slot for that id, and an unfilled slot is one
// still carrying the creation-time "aborted" marker. Newest-first keeps
// duplicate ids inside one window, and leftovers from a crashed segment,
// from clobbering the current turn's answer.
func (s *Session) fillSlot(m model.Message, content, reasoning, extraJSON string) (int64, bool, error) {
	callID, _ := m["tool_call_id"].(string)
	extraMap, _ := m["extra"].(map[string]any)
	now := time.Now().UnixMilli()

	if tcno, ok := callNumber(extraMap["tcno"]); ok {
		var slot sql.NullInt64
		err := s.store.db.QueryRow(`SELECT result_turn FROM tool_calls WHERE session_id = ? AND tcno = ?`,
			s.ID.String(), tcno).Scan(&slot)
		if errors.Is(err, sql.ErrNoRows) {
			// Same contract as the old link path: an observation for a call
			// the store never opened is a bug, not a fresh row.
			return 0, false, fmt.Errorf("session store: unknown tool call %d", tcno)
		}
		if err != nil {
			return 0, false, fmt.Errorf("session store: find slot for call %d: %w", tcno, err)
		}
		if !slot.Valid {
			return 0, false, nil // on-demand row: the append path owns this one
		}
		return s.fillSlotSeq(slot.Int64, extraMap, content, reasoning, extraJSON, now)
	}
	if callID != "" {
		var slot int64
		err := s.store.db.QueryRow(`SELECT seq FROM turns
WHERE session_id = ? AND role = 'tool' AND tool_call_id = ? AND json_extract(extra, '$.aborted') = 1
ORDER BY seq DESC LIMIT 1`, s.ID.String(), callID).Scan(&slot)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil // no waiting slot: the append path owns it
		}
		if err != nil {
			return 0, false, fmt.Errorf("session store: find unfilled slot: %w", err)
		}
		return s.fillSlotSeq(slot, extraMap, content, reasoning, extraJSON, now)
	}
	return 0, false, nil
}

// fillSlotSeq overwrites the placeholder in place: the row keeps its seq, so
// the transcript's order was decided at issue time and no completion can
// shuffle it. Filling removes the "aborted" marker - the stored extra is now
// the observation's own.
func (s *Session) fillSlotSeq(slot int64, extraMap map[string]any, content, reasoning, extraJSON string, now int64) (int64, bool, error) {
	var seq int64
	err := s.store.db.QueryRow(`UPDATE turns SET content = ?, reasoning = ?, extra = ?, created_at = ?
WHERE session_id = ? AND seq = ? RETURNING seq`,
		content, reasoning, extraJSON, now, s.ID.String(), slot).Scan(&seq)
	if err != nil {
		return 0, false, fmt.Errorf("session store: fill slot %d: %w", slot, err)
	}
	if extraMap != nil {
		extraMap["turn"] = seq
	}
	if _, err := s.store.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		now, s.ID.String()); err != nil {
		return seq, true, fmt.Errorf("session store: touch session: %w", err)
	}
	return seq, true, nil
}

// RecordProgress persists the run's spend and model-call counters so a killed
// run still has them to resume from.
func (s *Session) RecordProgress(cost float64, nCalls int) error {
	res, err := s.store.db.Exec(`UPDATE sessions SET cost = ?, n_calls = ?, updated_at = ? WHERE id = ?`,
		cost, nCalls, time.Now().UnixMilli(), s.ID.String())
	if err != nil {
		return fmt.Errorf("session store: record progress: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session store: unknown session %q", s.ID)
	}
	return nil
}

// Transcript rebuilds the []model.Message history from the turns table. It is
// one query and reads no result files; the joined call columns give every
// observation turn the same extra keys the live loop put on it, so a resumed
// message carries exactly what a live one did and readers just seek and read.
// Unfilled slots - a call whose file was never closed - join but are not
// enriched: their stored aborted extra deliberately names no result file,
// because none exists.
func (s *Session) Transcript() ([]model.Message, error) {
	rows, err := s.store.db.Query(`SELECT t.seq, t.role, t.content, t.reasoning, t.tool_calls, t.tool_call_id,
       t.extra, c.tcno, c.status, c.view_offset, c.view_elided, c.output_bytes
FROM turns t
LEFT JOIN tool_calls c ON c.session_id = t.session_id AND c.result_turn = t.seq
WHERE t.session_id = ?
ORDER BY t.seq`, s.ID.String())
	if err != nil {
		return nil, fmt.Errorf("session store: transcript: %w", err)
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		var (
			seq                  int64
			role, content        string
			reasoning, callsJSON string
			callID, extraJSON    string
			tcno                 sql.NullInt64
			callStatus           sql.NullString
			viewOffset           sql.NullInt64
			viewElided           sql.NullInt64
			outBytes             sql.NullInt64
		)
		if err := rows.Scan(&seq, &role, &content, &reasoning, &callsJSON, &callID, &extraJSON,
			&tcno, &callStatus, &viewOffset, &viewElided, &outBytes); err != nil {
			return nil, err
		}
		m := model.Message{"role": role, "content": content}
		// Keys are omitted, never set to "": Sushi.requestBody
		// (model/sushi.go:175-190) copies every non-extra key straight onto
		// the wire.
		if reasoning != "" {
			m["reasoning_content"] = reasoning
		}
		if callID != "" {
			m["tool_call_id"] = callID
		}
		if callsJSON != "" {
			var calls []any
			if err := json.Unmarshal([]byte(callsJSON), &calls); err != nil {
				return nil, fmt.Errorf("session store: decode tool_calls of turn %d: %w", seq, err)
			}
			m["tool_calls"] = calls
		}
		var extra map[string]any
		if extraJSON != "" {
			if err := json.Unmarshal([]byte(extraJSON), &extra); err != nil {
				return nil, fmt.Errorf("session store: decode extra of turn %d: %w", seq, err)
			}
		}
		if tcno.Valid || extra != nil {
			if extra == nil {
				extra = map[string]any{}
			}
			extra["turn"] = seq
			if tcno.Valid && callStatus.String == "completed" {
				extra["tcno"] = int(tcno.Int64)
				extra["tcr_path"] = filepath.Join(s.store.tcrBase, s.ID.String(),
					fmt.Sprintf("%d.log", tcno.Int64))
				extra["tcr_offset"] = viewOffset.Int64
				extra["tcr_bytes"] = outBytes.Int64
				extra["elided_lines"] = int(viewElided.Int64)
			}
			m["extra"] = extra
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// cappedFirstLine summarises a task: its first line, at most 200 runes.
func cappedFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}

// callNumber reads an extra["tcno"] the way the loop writes it (int) or the
// way a decoded JSON number arrives (float64).
func callNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), n > 0
	case int64:
		return n, n > 0
	case float64:
		return int64(n), n > 0
	}
	return 0, false
}
