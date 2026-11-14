package session

import (
	"database/sql"
	"encoding/json"
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

	now := time.Now().UnixMilli()
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
	// turn. A message without tcno performs no update; turns itself never
	// references a call.
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
func (s *Session) Transcript() ([]model.Message, error) {
	rows, err := s.store.db.Query(`SELECT t.seq, t.role, t.content, t.reasoning, t.tool_calls, t.tool_call_id,
       t.extra, c.tcno, c.view_offset, c.view_elided, c.output_bytes
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
			viewOffset           sql.NullInt64
			viewElided           sql.NullInt64
			outBytes             sql.NullInt64
		)
		if err := rows.Scan(&seq, &role, &content, &reasoning, &callsJSON, &callID, &extraJSON,
			&tcno, &viewOffset, &viewElided, &outBytes); err != nil {
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
			if tcno.Valid {
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
