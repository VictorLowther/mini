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
	-- Lineage (docs/parallel-sessions.md §5.3, §5.11). parent_id is nullable
	-- rather than '' DEFAULT because it carries a foreign key, and SQLite
	-- exempts NULL from FK enforcement while '' would be an unresolvable id -
	-- the same reason jobs.pid is nullable. ON DELETE CASCADE is the
	-- *constraint*, not the cleanup: Store.Delete collects the subtree first,
	-- because the result files are named after rows that CASCADE has already
	-- erased by the time it runs, so a cascade-only design leaks directories
	-- permanently (see the note there). root_id is the top-level ancestor's own
	-- id, and a top-level session is its own root, which is what makes
	-- "everything in this run" one indexed predicate with no OR clause to
	-- forget; Create backfills it so '' never persists. kind reuses the two
	-- words the job kinds already use (jobs.go:16-19) and carries no CHECK, for
	-- the same reason jobs.kind does not: a constraint added by ALTER TABLE
	-- cannot carry one, so a CHECK here would exist on databases created fresh
	-- and be absent on every database that predates this column, and a rule
	-- whose force depends on when the file was first created is worse than the
	-- Go-side default in Create.
	parent_id   TEXT    REFERENCES sessions (id) ON DELETE CASCADE,
	root_id     TEXT    NOT NULL DEFAULT '',
	kind        TEXT    NOT NULL DEFAULT 'top',
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
-- idx_sessions_root and idx_sessions_parent are deliberately NOT created here.
-- The schema const is exec'd before migrateSessionLineage, and an index over a
-- column a legacy database does not have yet fails the whole exec. Both are
-- created by that migration instead, so a fresh and a migrated database still
-- end up with the same shape.

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
	// After the schema exec, not before: a database created fresh gets its
	// columns from the const above and this returns immediately, so there is one
	// spelling of the table. Only a database that predates lineage reaches the
	// ALTERs, and doing it in this order is what keeps the two from disagreeing.
	if err := migrateSessionLineage(db); err != nil {
		db.Close()
		return nil, err
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

// migrateSessionLineage adds the lineage columns to a sessions table created
// before they existed, and backfills root_id so it is a real invariant rather
// than a column that is sometimes empty.
//
// ALTER TABLE rather than a rebuild, which is what migrateJobKeys above does -
// and the difference is the cost of each. Rebuilding jobs was cheap because
// nothing else references it; rebuilding sessions would have to move every turn,
// tool_call, job and job_event with their cascades intact and foreign_keys ON,
// for a change that adds three nullable-or-defaulted columns. ADD COLUMN can do
// that here, with two constraints that shape the result: an added column cannot
// carry a CHECK (why kind has no constraint in schema), and an added column with
// a REFERENCES clause must default to NULL (why parent_id is nullable rather
// than the ” jobs.parent_id uses). Both are honored in the const, so fresh and
// migrated databases end up with the same shape.
func migrateSessionLineage(db *sql.DB) error {
	have, err := sessionColumnsPresent(db)
	if err != nil {
		return err
	}
	for _, col := range []struct{ name, ddl string }{
		{"parent_id", `ALTER TABLE sessions ADD COLUMN parent_id TEXT REFERENCES sessions (id) ON DELETE CASCADE`},
		{"root_id", `ALTER TABLE sessions ADD COLUMN root_id TEXT NOT NULL DEFAULT ''`},
		{"kind", `ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'top'`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			return fmt.Errorf("session store: add sessions.%s: %w", col.name, err)
		}
	}
	// Every row that exists is the root of its own tree: no database this code
	// ever wrote has a child in it, so backfilling to id is the whole truth and
	// not a guess. It also has to happen before Create can inherit a parent's
	// root, or a new child would resolve its root to '' and share a tree with
	// every other row that missed the backfill.
	if _, err := db.Exec(`UPDATE sessions SET root_id = id WHERE root_id = ''`); err != nil {
		return fmt.Errorf("session store: backfill root_id: %w", err)
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_root ON sessions (root_id, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions (parent_id)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("session store: lineage index: %w", err)
		}
	}
	return nil
}

// sessionColumnsPresent reads the live table shape rather than pattern-matching
// the stored CREATE text the way migrateJobKeys must. Column presence is a
// fact; a DDL string match is a guess about spacing, and migrateJobKeys' own
// comment records how that guess once passed its test and failed against a real
// database.
func sessionColumnsPresent(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		return nil, fmt.Errorf("session store: read sessions columns: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("session store: read sessions columns: %w", err)
		}
		have[n] = true
	}
	return have, rows.Err()
}

// SweepLineage deletes child sessions whose parent no longer exists, along with
// their files, and returns one warning per session it removed.
//
// A child outlives its parent only by accident: rows removed by a build that
// predated the recursive Delete, a hand-edited database, or a crash between the
// parent's DELETE and the child's directory removal. Such a row is not history
// anyone can read - it was one step inside a conversation that no longer exists
// - and its files are the garbage §5.11's ruling accepts on purpose, so the
// sweep is what makes that acceptance safe rather than a slow leak.
//
// It runs at open, before any session can start, for the same reason the job
// sweep does: it is a statement about what this process did not create, and it
// must be true before anything reads the registry.
func (s *Store) SweepLineage() ([]string, error) {
	rows, err := s.db.Query(`SELECT id, title FROM sessions
WHERE parent_id IS NOT NULL AND parent_id <> ''
  AND NOT EXISTS (SELECT 1 FROM sessions p WHERE p.id = sessions.parent_id)`)
	if err != nil {
		return nil, fmt.Errorf("session store: find orphaned children: %w", err)
	}
	type orphan struct{ id, title string }
	var found []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.title); err != nil {
			rows.Close()
			return nil, fmt.Errorf("session store: find orphaned children: %w", err)
		}
		found = append(found, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // release the single connection before the deletes

	warnings := make([]string, 0, len(found))
	for _, o := range found {
		name := o.title
		if name == "" {
			name = o.id
		}
		id, err := uuid.Parse(o.id)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("subagent session %s has an unparseable id and was left alone", o.id))
			continue
		}
		if err := s.Delete(id); err != nil {
			warnings = append(warnings, fmt.Sprintf("subagent session %s could not be removed: %v", name, err))
			continue
		}
		warnings = append(warnings, fmt.Sprintf("removed subagent session %s: its parent session is gone", name))
	}
	return warnings, nil
}

// SweepStrayResults removes per-session result directories that no session row
// names, and returns how many.
//
// This is the other half of the leak. Delete removes rows first and files
// second, deliberately - a directory going missing while its row still lives
// would leave a transcript citing bytes that are not there - so a crash in
// between strands directories that nothing will ever name again. tcrBase
// outlives the database's knowledge of them, and these are the biggest files
// mini writes.
//
// It is conservative in three ways, because a sweep that deletes the wrong
// directory destroys a live run's evidence: only entries that are directories
// are considered, only those whose name parses as a session uuid, and only
// those with no row. A row is never removed here, so a session that exists in
// another process keeps its files whatever else is on the disk.
func (s *Store) SweepStrayResults() (int, error) {
	entries, err := os.ReadDir(s.tcrBase)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil // nothing has ever written a result file
	}
	if err != nil {
		return 0, fmt.Errorf("session store: read %s: %w", s.tcrBase, err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := uuid.Parse(e.Name())
		if err != nil {
			continue // not ours
		}
		var exists bool
		err = s.db.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, id.String()).Scan(&exists)
		if err == nil {
			continue // a live row owns this directory
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return removed, fmt.Errorf("session store: check session %s: %w", id, err)
		}
		if err := os.RemoveAll(filepath.Join(s.tcrBase, e.Name())); err != nil {
			return removed, fmt.Errorf("session store: remove stray %s: %w", e.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// KindTop names a session a human started. Its counterpart, KindSubagent, is
// declared with the job kinds in jobs.go:16-19, and the two tables share the
// vocabulary deliberately: one set of words answers "who made this" whatever
// the row is. The kind is checked in Go rather than by a CHECK constraint for
// the reason spelled out on the column in schema.
const KindTop = "top"

// Row is one sessions record.
type Row struct {
	ID       uuid.UUID
	ParentID string
	RootID   string
	// Kind is KindTop or KindSubagent. The distinction is load-bearing in two
	// places that look unrelated: a subagent session is neither resumable nor
	// removable on its own (docs/parallel-sessions.md §5.4, §5.11), and both
	// commands reach a session through Find, so one filter enforces both.
	Kind       string
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

const sessionColumns = "id, parent_id, root_id, kind, title, workdir, project, model, cost, n_calls, exit_status, created_at, updated_at"

func scanSession(sc interface{ Scan(dest ...any) error }) (Row, error) {
	var (
		r        Row
		id       string
		parentID sql.NullString
	)
	if err := sc.Scan(&id, &parentID, &r.RootID, &r.Kind, &r.Title,
		&r.Workdir, &r.Project, &r.ModelName,
		&r.Cost, &r.NCalls, &r.ExitStatus, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Row{}, err
	}
	r.ParentID = parentID.String // a NULL parent - a top-level session - reads ""
	parsed, err := uuid.Parse(id)
	if err != nil {
		return Row{}, fmt.Errorf("session store: bad id %q: %w", id, err)
	}
	r.ID = parsed
	return r, nil
}

// Create inserts a new session row, filling in the lineage defaults so the
// columns cannot be left half-true by a caller that did not think about them.
// Zero timestamps are filled with now.
func (s *Store) Create(r Row) (*Session, error) {
	now := time.Now().UnixMilli()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = now
	}
	if r.Kind == "" {
		r.Kind = KindTop
	}
	switch r.Kind {
	case KindTop, KindSubagent:
	default:
		return nil, fmt.Errorf("session store: unknown session kind %q", r.Kind)
	}

	// A child resolves its own root and its parent's authority here, so no
	// launcher can decide "inherit" differently later - zero's discipline of
	// resolving before the seam (swarm/member.go:23), and the reason the row
	// carries a resolved root rather than a trail of pointers.
	var parentArg any
	if r.ParentID != "" {
		var proot, pkind string
		err := s.db.QueryRow(`SELECT root_id, kind FROM sessions WHERE id = ?`, r.ParentID).Scan(&proot, &pkind)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session store: parent session %q does not exist", r.ParentID)
		}
		if err != nil {
			return nil, fmt.Errorf("session store: read parent session: %w", err)
		}
		// The two-level tree gets a backstop here as well as in prompt and tool
		// visibility (docs/parallel-sessions.md §5.6). Enforcing it only by
		// hiding the spawn tool leaves the rule true because nothing asked
		// otherwise; this makes a grandchild impossible for any caller, including
		// a test, a future kind, or a hand-written row - and two layers that
		// agree are two layers that cannot drift.
		if pkind != KindTop {
			return nil, fmt.Errorf("session store: session %q is a %s and cannot have children", r.ParentID, pkind)
		}
		parentArg = r.ParentID
		if r.RootID == "" {
			// firstNonEmpty(parent.Root, parent.ID): a parent predating this
			// column has root_id '' and is its own root.
			r.RootID = proot
			if r.RootID == "" {
				r.RootID = r.ParentID
			}
		}
	}
	if r.RootID == "" {
		r.RootID = r.ID.String() // a top-level session is its own tree
	}

	_, err := s.db.Exec(`INSERT INTO sessions (id, parent_id, root_id, kind, title, workdir, project, model, cost, n_calls, exit_status, next_tcno, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		r.ID.String(), parentArg, r.RootID, r.Kind, r.Title, r.Workdir, r.Project, r.ModelName,
		r.Cost, r.NCalls, r.ExitStatus, r.CreatedAt, r.UpdatedAt)
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
	// kind = 'top' is the whole enforcement of two rules that look unrelated:
	// a subagent session is not resumable (§5.4) and not removable except with
	// its parent (§5.11), and both -r and `mini sessions rm` resolve their
	// argument here. A prefix that names a child is simply not found, which is
	// the correct answer to both commands at once.
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions
WHERE kind = 'top' AND (? = '' OR project = ?) AND substr(id, 1, length(?)) = ?
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
	// Top-level only, for the same reason Find is: this is what `mini sessions`
	// shows and what findResumable picks the newest of, and either would be a
	// lie about the human's own conversation if a subagent's row could appear in
	// it (docs/parallel-sessions.md §3.5.7). Children have Children below.
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE kind = 'top'`
	var args []any
	if !all && project != "" {
		q += ` AND project = ?`
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

// Children lists the subagent sessions spawned by one parent, oldest first.
// List and Find never return these rows; this is the only way to them, because
// the whole point of the kind column is that a child is not an endpoint (see
// Find). It exists for the report and the roster of docs/parallel-sessions.md
// §7 step 7, and for nothing that resumes or removes.
func (s *Store) Children(parentID string) ([]Row, error) {
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions
WHERE parent_id = ? ORDER BY created_at ASC, id ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("session store: children: %w", err)
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

// TreeTotals is the inclusive cost and call count of a whole run - the
// top-level session plus every subagent under it - as one derived query rather
// than a number stored on the parent.
//
// That choice is the point. crush accumulates a child's spend into its parent
// and therefore needs rolledUpCost to remember how much it has already folded
// in, because the child's own total is cumulative across every run
// (docs/parallel-sessions.md §5.8, agent_dispatch.go:173-176). Nothing here is
// accumulated, so nothing can be double-counted, no ordering between a child
// finishing and the parent's ledger being written can be wrong, and a crash
// between the two still answers correctly. It also keeps Loop.Cost and
// Loop.NCalls off the table: those are agent-goroutine-only by contract
// (agent.go:696-698), and any rollup that added into them from a child's
// watcher would be a data race this package could not prevent.
//
// root_id is indexed for this; the coalesce guards a row whose root_id is still
// ” from before the backfill, which would otherwise answer for itself alone.
func (s *Store) TreeTotals(rootID string) (cost float64, calls int, err error) {
	err = s.db.QueryRow(`SELECT coalesce(sum(cost), 0), coalesce(sum(n_calls), 0)
FROM sessions WHERE root_id = ? OR id = ?`, rootID, rootID).Scan(&cost, &calls)
	if err != nil {
		return 0, 0, fmt.Errorf("session store: tree totals: %w", err)
	}
	return cost, calls, nil
}

// Delete removes a session, every subagent session under it, and all of their
// files. A child is removable only with its parent, and only with its parent
// (docs/parallel-sessions.md §5.11): removing one before its parent destroys
// data the parent may still use, and removing one after is garbage the sweep
// below collects.
//
// The subtree is collected BEFORE anything is deleted, which is the entire
// reason this function is not one DELETE and one RemoveAll. Result files live
// outside the database at <tcrBase>/<uuid>, a path *derived from the row*
// (tcrDir, tcr.go:12), so once ON DELETE CASCADE has erased a child row nothing
// can name its directory again - the rows are gone and the path is computed
// from them. A cascade-only design therefore leaks every subagent's command
// output and job logs permanently, and the leak never stops costing, because
// those are the largest files mini writes. Background job logs are covered by
// the same single RemoveAll per session, since JobPath puts them inside the
// session directory rather than beside it (jobs.go:156-158).
//
// WHERE parent_id = ? rather than root_id = ?: a child shares its root with its
// siblings, so a root-scoped delete would take out brothers and sisters that
// were not asked about. Collecting by parent_id is complete at any depth we can
// reach, because Create refuses a grandchild (see the note there) - the two-
// level tree is what makes one hop the whole subtree.
func (s *Store) Delete(id uuid.UUID) error {
	kids, err := s.db.Query(`SELECT id FROM sessions WHERE parent_id = ?`, id.String())
	if err != nil {
		return fmt.Errorf("session store: collect subtree: %w", err)
	}
	ids := []string{id.String()}
	for kids.Next() {
		var k string
		if err := kids.Scan(&k); err != nil {
			kids.Close()
			return fmt.Errorf("session store: collect subtree: %w", err)
		}
		ids = append(ids, k)
	}
	if err := kids.Err(); err != nil {
		kids.Close()
		return fmt.Errorf("session store: collect subtree: %w", err)
	}
	kids.Close() // the single pool connection must be back before we write

	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id.String())
	if err != nil {
		return fmt.Errorf("session store: delete: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("session store: unknown session %q", id)
	}

	// Files after rows: a directory removed while its row still lives leaves a
	// transcript pointing at bytes that are gone, which is worse than the
	// reverse - the reverse is what SweepFilelessSessions collects, and the row
	// still describes what happened.
	var failures []string
	for _, one := range ids {
		if err := os.RemoveAll(filepath.Join(s.tcrBase, one)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", one, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("session store: %d of %d result directories survived deletion (%s)",
			len(failures), len(ids), strings.Join(failures, "; "))
	}
	return nil
}
