package session

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/VictorLowther/mini/model"
)

// turnOf reads the issuing turn that RecordMessage stamped into the message's
// extra. A missing or non-numeric value means the turn was never persisted
// (exactly what a failed RecordMessage leaves behind), so there is nothing to
// attach a call to.
func turnOf(msg model.Message) (int64, error) {
	extra, _ := msg["extra"].(map[string]any)
	switch n := extra["turn"].(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	}
	return 0, errors.New("session store: issuing turn not recorded")
}

// OpenToolCall hands back the call's row and its result file, open for
// writing. Normally the row already exists: recording the issuing assistant
// turn allocated the ledger row and its transcript slot together, so the
// file's whole life - open, partial writes, death - happens inside a
// transcript that was legal before the command started. A row that is missing
// is a call no assistant turn issued (a human-mode command): those take the
// old on-demand path, inserting now and answering in a fresh appended turn,
// because no slot was pre-allocated for them.
func (s *Session) OpenToolCall(msg model.Message, idx int, action model.Action) (model.ToolCall, error) {
	turn, err := turnOf(msg)
	if err != nil {
		return model.ToolCall{}, err
	}
	now := time.Now().UnixMilli()
	var tcno int
	onDemand := false
	err = s.store.db.QueryRow(`SELECT tcno FROM tool_calls WHERE session_id = ? AND turn = ? AND idx = ?`,
		s.ID.String(), turn, idx).Scan(&tcno)
	if errors.Is(err, sql.ErrNoRows) {
		onDemand = true
		// One statement allocates the per-session number, so concurrent
		// processes cannot collide; next_tcno counts issued calls, hence the
		// first call is 1 and the files are 1.log, 2.log, …
		if err := s.store.db.QueryRow(`UPDATE sessions SET next_tcno = next_tcno + 1, updated_at = ? WHERE id = ? RETURNING next_tcno`,
			now, s.ID.String()).Scan(&tcno); err != nil {
			return model.ToolCall{}, fmt.Errorf("session store: allocate tool call: %w", err)
		}
		if _, err := s.store.db.Exec(`INSERT INTO tool_calls (session_id, turn, idx, tcno, id, name, command, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			// name is "bash" today, the only tool parseToolCallActions accepts
			// (model/actions.go:31-35); the column exists so a second tool needs
			// no migration. id is "" for a human-mode command, which has no
			// provider tool_call_id.
			s.ID.String(), turn, idx, tcno, action.ToolCallID, "bash", action.Command, now); err != nil {
			return model.ToolCall{}, fmt.Errorf("session store: open tool call %d: %w", tcno, err)
		}
	} else if err != nil {
		return model.ToolCall{}, fmt.Errorf("session store: find tool call: %w", err)
	}
	// The file comes after the row, in this order so a failure never leaves
	// a row pointing at nothing. An on-demand row is deleted again; a slot's
	// row is not: its transcript placeholder is already committed, and the
	// ledger is the slot's story whether or not the file ever opens.
	// next_tcno is NOT rolled back: a skipped number is harmless.
	path := s.tcrPath(tcno)
	if err := os.MkdirAll(s.tcrDir(), 0o700); err != nil {
		if onDemand {
			s.deleteCallRow(tcno)
		}
		return model.ToolCall{}, fmt.Errorf("session store: create %s: %w", s.tcrDir(), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		if onDemand {
			s.deleteCallRow(tcno)
		}
		return model.ToolCall{}, fmt.Errorf("session store: open %s: %w", path, err)
	}
	return model.ToolCall{Number: tcno, Path: path, File: f, Action: action}, nil
}

func (s *Session) deleteCallRow(tcno int) {
	if _, err := s.store.db.Exec(`DELETE FROM tool_calls WHERE session_id = ? AND tcno = ?`,
		s.ID.String(), tcno); err != nil {
		// Nothing useful remains to do; the row is unreachable anyway once
		// the session goes, and the cascade owns its cleanup.
		_ = err
	}
}

// CompleteToolCall closes the row with the outcome the loop measured on the
// closed file; it does NOT touch the bytes, which the shell already wrote, and
// it does not close c.File — the caller must stop the shell's writes before
// anything reads the file.
func (s *Session) CompleteToolCall(c model.ToolCall, out model.ToolOutcome) error {
	if c.Number == 0 {
		// Scratch or unrecorded call: no row, no stored offset.
		return nil
	}
	res, err := s.store.db.Exec(`UPDATE tool_calls SET status = 'completed', returncode = ?, cwd = ?, output_bytes = ?, view_offset = ?, view_elided = ? WHERE session_id = ? AND tcno = ?`,
		out.ReturnCode, out.CWD, out.Bytes, out.ViewOffset, out.ViewElided,
		s.ID.String(), c.Number)
	if err != nil {
		return fmt.Errorf("session store: complete tool call %d: %w", c.Number, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session store: unknown tool call %d", c.Number)
	}
	return nil
}
