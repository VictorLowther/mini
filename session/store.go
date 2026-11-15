// Package session is the transcript's source of truth: one global SQLite
// database holding sessions, turns, and tool-call records (plus a global
// settings row-per-key table for interface state that outlives a session), the
// per-call result files under <tcrBase>/<uuid>/<tcno>.log that hold every
// command's complete merged stdout+stderr. Tool output never enters the
// database; the call row only names the byte range the model was shown.
package session

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"uuid"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" dialect
)

// Store is one open session database.
type Store struct {
	db      *sql.DB
	tcrBase string
}

// schema has no version tracking: mini is 0.1.0 and no legacy databases
// exist; the first breaking change adds PRAGMA user_version. db.Exec without
// arguments runs every ;-separated statement (ncruces driver.go:414-436), so
// this may be one const; parameterized queries must stay single statements.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id          TEXT    NOT NULL PRIMARY KEY,
	title       TEXT    NOT NULL DEFAULT '',
	workdir     TEXT    NOT NULL DEFAULT '',
	project     TEXT    NOT NULL DEFAULT '',
	model       TEXT    NOT NULL DEFAULT '',
	cost        REAL    NOT NULL DEFAULT 0,
	n_calls     INTEGER NOT NULL DEFAULT 0,
	exit_status TEXT    NOT NULL DEFAULT '',
	next_tcno   INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_project_updated ON sessions (project, updated_at DESC);

CREATE TABLE IF NOT EXISTS settings (
	key        TEXT    NOT NULL PRIMARY KEY,
	value      TEXT    NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS turns (
	session_id   TEXT    NOT NULL,
	seq          INTEGER NOT NULL,
	role         TEXT    NOT NULL,
	content      TEXT    NOT NULL DEFAULT '',
	reasoning    TEXT    NOT NULL DEFAULT '',
	tool_calls   TEXT    NOT NULL DEFAULT '',
	tool_call_id TEXT    NOT NULL DEFAULT '',
	extra        TEXT    NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (session_id, seq),
	FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS tool_calls (
	session_id     TEXT    NOT NULL,
	turn           INTEGER NOT NULL,
	idx            INTEGER NOT NULL,
	tcno           INTEGER NOT NULL,
	id             TEXT    NOT NULL DEFAULT '',
	name           TEXT    NOT NULL DEFAULT 'bash',
	command        TEXT    NOT NULL,
	status         TEXT    NOT NULL DEFAULT 'started'
	CHECK (status IN ('started','completed')),
	returncode     INTEGER NOT NULL DEFAULT 0,
	cwd            TEXT    NOT NULL DEFAULT '',
	output_bytes   INTEGER NOT NULL DEFAULT 0,
	view_offset    INTEGER NOT NULL DEFAULT 0,
	view_elided    INTEGER NOT NULL DEFAULT 0,
	result_turn    INTEGER,
	created_at     INTEGER NOT NULL,
	PRIMARY KEY (session_id, turn, idx),
	UNIQUE (session_id, tcno),
	FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
	FOREIGN KEY (session_id, turn) REFERENCES turns (session_id, seq) ON DELETE CASCADE,
	FOREIGN KEY (session_id, result_turn) REFERENCES turns (session_id, seq) ON DELETE CASCADE
) WITHOUT ROWID;
`

// Open prepares the database at path. Result files live under
// <tcrBase>/<session uuid>/<tcno>.log; tcrBase is never stored in a row.
func Open(path, tcrBase string) (*Store, error) {
	// The `file:` prefix is load-bearing: the driver parses _pragma/_txlock
	// only for URL-form DSNs (driver/driver.go:176-195). Setting _pragma also
	// suppresses the driver's own one-minute busy timeout, so busy_timeout is
	// ours to set. foreign_keys(1) is what makes the schema's constraints and
	// cascades real; SQLite defaults enforcement OFF.
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("session store: open: %w", err)
	}
	// Multiple pool connections interleaving writes/checkpoints desync the
	// WAL header (crush's scar, internal/db/connect.go:137-141). Concurrency
	// stance: N processes may write this database — WAL, busy_timeout and
	// immediate transactions handle that — but two processes driving the
	// *same session* at once is unsupported and only produces interleaved
	// turns. No flock/lease machinery.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("session store: migrate: %w", err)
	}
	return &Store{db: db, tcrBase: tcrBase}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Row is one sessions record.
type Row struct {
	ID         uuid.UUID
	Title      string
	Workdir    string
	Project    string
	ModelName  string
	Cost       float64
	NCalls     int
	ExitStatus string
	CreatedAt  int64
	UpdatedAt  int64
}

// Session is a Row bound to the store that owns it.
type Session struct {
	Row
	store *Store
}

const sessionColumns = "id, title, workdir, project, model, cost, n_calls, exit_status, created_at, updated_at"

func scanSession(sc interface{ Scan(dest ...any) error }) (Row, error) {
	var (
		r  Row
		id string
	)
	if err := sc.Scan(&id, &r.Title, &r.Workdir, &r.Project, &r.ModelName,
		&r.Cost, &r.NCalls, &r.ExitStatus, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Row{}, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return Row{}, fmt.Errorf("session store: bad id %q: %w", id, err)
	}
	r.ID = parsed
	return r, nil
}

// Create inserts a new session row; zero timestamps are filled with now.
func (s *Store) Create(r Row) (*Session, error) {
	now := time.Now().UnixMilli()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = now
	}
	_, err := s.db.Exec(`INSERT INTO sessions (id, title, workdir, project, model, cost, n_calls, exit_status, next_tcno, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		r.ID.String(), r.Title, r.Workdir, r.Project, r.ModelName, r.Cost, r.NCalls, r.ExitStatus, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("session store: create: %w", err)
	}
	return &Session{Row: r, store: s}, nil
}

// Get fetches a session by full id; unknown ids give sql.ErrNoRows.
func (s *Store) Get(id uuid.UUID) (*Session, error) {
	row, err := scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id.String()))
	if err != nil {
		return nil, err
	}
	return &Session{Row: row, store: s}, nil
}

// Find resolves a uuid prefix ("" = the whole id) inside project; it returns
// sql.ErrNoRows when nothing matches and an error naming the ambiguous prefix
// when more than one session matches. project "" searches every project.
func (s *Store) Find(project, prefix string) (*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions
WHERE (? = '' OR project = ?) AND substr(id, 1, length(?)) = ?
ORDER BY updated_at DESC, id DESC LIMIT 2`, project, project, prefix, prefix)
	if err != nil {
		return nil, fmt.Errorf("session store: find: %w", err)
	}
	defer rows.Close()
	var found []Row
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, sql.ErrNoRows
	case 2:
		return nil, fmt.Errorf("session store: ambiguous session prefix %q (at least %s and %s)",
			prefix, found[0].ID, found[1].ID)
	}
	return &Session{Row: found[0], store: s}, nil
}

// List returns sessions newest-update first. project "" lists every project
// when all is true; all with an empty project is how `mini sessions --all`
// reads the whole database.
func (s *Store) List(project string, all bool, limit int) ([]Row, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions`
	var args []any
	if !all && project != "" {
		q += ` WHERE project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("session store: list: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes the session row; every foreign key cascades, so turns and
// tool_calls go with it in this single statement (a turn's deletion takes its
// calls too). The result files live outside the database, so Delete also
// removes the whole <tcrBase>/<uuid> directory (os.RemoveAll, a no-op when it
// is absent).
func (s *Store) Delete(id uuid.UUID) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id.String())
	if err != nil {
		return fmt.Errorf("session store: delete: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("session store: unknown session %q", id)
	}
	return os.RemoveAll(filepath.Join(s.tcrBase, id.String()))
}
