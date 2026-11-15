package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// settings is the database's global half. sessions/turns/tool_calls are
// per-session history, and all of it is deletable; the settings table holds
// the small amount of interface state a human expects to still be there the
// next time they open mini - whether the model's thinking is shown, how big
// the window was - which belongs to nobody's session and must survive
// `mini sessions` cleanup and a deleted history.
//
// One row per key rather than one JSON document: `sqlite3 ~/.mini/sessions.db
// 'select * from settings'` reads like the preference list it is, and a value
// a future version cannot parse costs one key instead of the whole document.
// The keys and their meanings live with the front end that owns them (see
// ui/prefs.go); this layer is a type-free text store, so the interface never
// has to import this package.
//
// Every settings call is context-bounded, which the transcript writes are
// not: a sticky preference is a nicety, and a busy database (the agent
// goroutine is mid-turn, or another mini holds the write lock for the whole
// 30s busy_timeout) must not be able to hang a window closing.

// settingTimeout bounds one settings read or write. Reads normally return in
// microseconds; the cap exists for the locked-database case.
const settingTimeout = 2 * time.Second

// SetSetting writes one global key, creating or replacing it.
func (s *Store) SetSetting(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), settingTimeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("session store: setting %q: %w", key, err)
	}
	return nil
}

// Setting reads one global key. ok is false when the key was never written,
// which is how a front end tells "no stored opinion" from a stored default -
// the two differ, because a preference someone set once should win over what
// this version of mini would have defaulted to.
func (s *Store) Setting(key string) (value string, ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), settingTimeout)
	defer cancel()
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("session store: setting %q: %w", key, err)
	}
	return value, true, nil
}

// DeleteSetting forgets one key, so the next read reports "no stored
// opinion". Unknown keys are not an error.
func (s *Store) DeleteSetting(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), settingTimeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("session store: unset %q: %w", key, err)
	}
	return nil
}
