// Package session is the transcript's source of truth: one global SQLite
// database holding sessions, turns, tool-call records, and job records (plus a
// global settings row-per-key table for interface state that outlives a
// session), the per-call result files under <tcrBase>/<uuid>/<tcno>.log that
// hold every command's complete merged stdout+stderr, and the per-job files
// under <tcrBase>/<uuid>/job-<id>.log that hold a background run's output while
// it keeps growing. Tool output never enters the database; the call row only
// names the byte range the model was shown. Jobs are durable for a different
// reason: a job must be findable after the turn that started it has been
// summarized away, and after the process that started it has died.
package session

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT    NOT NULL,
	kind         TEXT    NOT NULL DEFAULT 'bash',
	session_id   TEXT    NOT NULL,
	parent_id    TEXT    NOT NULL DEFAULT '',
	tcno         INTEGER,
	label        TEXT    NOT NULL DEFAULT '',
	command      TEXT    NOT NULL DEFAULT '',
	description  TEXT    NOT NULL DEFAULT '',
	state        TEXT    NOT NULL DEFAULT 'running'
		CHECK (state IN ('running','completed','failed','killed','orphaned')),
	pid          INTEGER,
	pgid         INTEGER,
	output_path  TEXT    NOT NULL,
	exit_code    INTEGER,
	exit_reason  TEXT    NOT NULL DEFAULT '',
	started_at   INTEGER NOT NULL,
	ended_at     INTEGER,
	updated_at   INTEGER NOT NULL,
	-- (session_id, id), not id alone: a job id is a per-session handle the model
	-- quotes back ("j12"), and every read, kill and sweep in this store is
	-- scoped to a session. An id-only key makes two sessions' first jobs collide
	-- across the whole database, which is exactly what the live run found -
	-- UNIQUE constraint failed on the second session to background anything.
	PRIMARY KEY (session_id, id),
	FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_jobs_session_started ON jobs (session_id, started_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_jobs_parent ON jobs (parent_id);

CREATE TABLE IF NOT EXISTS job_events (
	id           INTEGER NOT NULL PRIMARY KEY,
	session_id   TEXT    NOT NULL,
	job_id       TEXT    NOT NULL,
	monitor_id   TEXT    NOT NULL DEFAULT 'exit',
	reason       TEXT    NOT NULL,
	line_no      INTEGER,
	byte_offset  INTEGER,
	matched      TEXT    NOT NULL DEFAULT '',
	context      TEXT    NOT NULL DEFAULT '',
	elapsed      INTEGER NOT NULL DEFAULT 0,
	exit_code    INTEGER,
	fired_at     INTEGER NOT NULL,
	delivered_at INTEGER,
	FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
	FOREIGN KEY (session_id, job_id) REFERENCES jobs (session_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_job_events_pending ON job_events (session_id, delivered_at, id);
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
	if err := migrateJobKeys(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("session store: migrate: %w", err)
	}
	return &Store{db: db, tcrBase: tcrBase}, nil
}

// migrateJobKeys rebuilds the job tables when they were created with an
// id-only primary key, which is the shape the first version of this table had
// and which cannot hold two sessions' first jobs.
//
// Rebuilding rather than rewriting rows is deliberate: the schema has no version
// tracking (see the note on schema above), the tables are new, and what they can
// contain from the affected build is a handful of finished rows and their
// firings - history, not state. Preserved: nothing worth the extra failure mode
// of a column swap under foreign keys, and the note is worth more than the rows.
func migrateJobKeys(db *sql.DB) error {
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'jobs'`).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // fresh database: the schema const creates the right shape
	}
	if err != nil {
		return fmt.Errorf("session store: inspect jobs table: %w", err)
	}
	// SQLite stores the CREATE text verbatim, and the shipped version aligned
	// its columns with runs of spaces - so comparing against any single-spacing
	// spelling silently never matches, which is exactly how this migration
	// passed its own test and then failed against a real database. Collapse the
	// whitespace and match on that.
	if !strings.Contains(strings.Join(strings.Fields(ddl), " "),
		"id TEXT NOT NULL PRIMARY KEY") {
		return nil // already per-session keyed, or hand-edited and none of our business
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS job_events; DROP TABLE IF EXISTS jobs;`); err != nil {
		return fmt.Errorf("session store: rebuild job tables: %w", err)
	}
	return nil
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
