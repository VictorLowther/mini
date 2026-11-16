package session

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"
)

// Job kinds. Only KindBash exists today; the column and the nullable pid exist
// so a subagent run can be registered by its own kind without a migration and
// without anything generic having to switch on the difference (the kind is
// display metadata, never a control path).
const (
	KindBash     = "bash"
	KindSubagent = "subagent"
)

// A job moves through four states, and the last of them is not in the state
// column:
//
//	started -> running -> finished -> delivered
//
// started and running are both JobRunning here (the distinction only matters to
// a kind that can fail to spawn, which records the reason in the row); finished
// is one of the four terminal states below, whoever decided it; and delivered is
// carried by the firing in job_events, because the thing that becomes delivered
// is the *result*, not the job. PendingJobEvents / UndeliveredFirings is the
// authoritative question - what does the conversation still owe - and that is
// what lets a run end without waiting for anything: an unfinished job is not a
// debt, an unsaid result is.
//
// The four states are the same for backgrounded and foreground jobs. They differ
// only in where the delivery lands: a foreground call's tool result is the
// delivered transition, while a backgrounded one answers during running and is
// delivered later, as a message the harness authors at a step boundary.
//
// Job states. A job is Running until something ends it, and the four terminal
// states differ in who decided: the command (Completed/Failed), the harness or
// the model (Killed), or the loss of the process that owned it (Orphaned).
// Orphaned is not a synonym for "still running after a restart": it asserts
// only that nobody owns the process any more, which is the honest answer
// (zero's rule, manager.go:475) and the reason it is terminal rather than
// resumed.
const (
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
	JobKilled    = "killed"
	JobOrphaned  = "orphaned"
)

// Terminal reports whether a state can still change. Sweeps, reaping, and the
// monitor engine all ask this instead of comparing against Running.
func Terminal(state string) bool { return state != JobRunning }

// JobIDPattern is why ids are safe as path components. Job ids reach the
// filesystem in jobPath, and they arrive there from model-authored tool calls
// once job_kill exists, so the shape is enforced on the way in rather than
// escaped on the way out (zero validates the same shape for the same reason,
// manager.go:66).
var JobIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ErrUnknownJob is what GetJob answers for an id that is not in this session.
var ErrUnknownJob = errors.New("session store: unknown job")

// JobRow is one jobs record.
//
// Zero-value meaning: nil pointers are SQL NULLs, so PID/PGID nil means "no
// process stands for this job" (a subagent, or an orphan we can no longer
// prove anything about), ExitCode nil means "this kind's ending is not an exit
// status", and EndedAt nil means the row has not ended. Do not read 0 as
// "none" for ExitCode: 0 is success.
type JobRow struct {
	ID          string
	Kind        string
	SessionID   string
	ParentID    string
	TCNO        *int // the tool call that produced it, when there was one
	Label       string
	Command     string
	Description string
	State       string
	PID         *int
	PGID        *int
	OutputPath  string
	ExitCode    *int
	ExitReason  string
	StartedAt   int64
	EndedAt     *int64
	UpdatedAt   int64
}

// jobColumns is in the same order as scanJob, and as the RETURNING lists below.
const jobColumns = `id, kind, session_id, parent_id, tcno, label, command, description,
	state, pid, pgid, output_path, exit_code, exit_reason, started_at, ended_at, updated_at`

func scanJob(sc interface{ Scan(dest ...any) error }) (JobRow, error) {
	var (
		j        JobRow
		tcno     sql.NullInt64
		pid      sql.NullInt64
		pgid     sql.NullInt64
		exitCode sql.NullInt64
		endedAt  sql.NullInt64
	)
	if err := sc.Scan(&j.ID, &j.Kind, &j.SessionID, &j.ParentID, &tcno, &j.Label,
		&j.Command, &j.Description, &j.State, &pid, &pgid, &j.OutputPath,
		&exitCode, &j.ExitReason, &j.StartedAt, &endedAt, &j.UpdatedAt); err != nil {
		return JobRow{}, err
	}
	j.TCNO, j.PID, j.PGID = nullInt(tcno), nullInt(pid), nullInt(pgid)
	j.ExitCode, j.EndedAt = nullInt(exitCode), nullInt64(endedAt)
	return j, nil
}

func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func nullInt64(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// timeArg writes an *int64 timestamp as the driver's NULL-or-int64 argument.
func timeArg(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// int64p writes a *int as the driver's NULL-or-int64 argument.
func int64p(p *int) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

// JobPath names a job's output file. It lives in the session's result directory
// so the session's own cleanup owns it, and it is named by job id rather than
// by tcno because a job is not a tool call: the file is still being written
// after the row that started it was finalized, and a subagent's file has no
// tcno to be named by.
func (s *Session) JobPath(id string) string {
	return filepath.Join(s.tcrDir(), "job-"+id+".log")
}

// CreateJob inserts a running job. The caller supplies the output path; the
// writer opens it in append mode, because append-at-EOF is the one rule that
// keeps every existing tcr_offset valid (result files are never rewritten).
func (s *Session) CreateJob(j JobRow) (*JobRow, error) {
	if !JobIDPattern.MatchString(j.ID) {
		return nil, fmt.Errorf("session store: job id %q is not path-safe", j.ID)
	}
	if j.Kind == "" {
		j.Kind = KindBash
	}
	if j.State == "" {
		j.State = JobRunning
	}
	if j.OutputPath == "" {
		j.OutputPath = s.JobPath(j.ID)
	}
	now := time.Now().UnixMilli()
	if j.StartedAt == 0 {
		j.StartedAt = now
	}
	j.SessionID, j.UpdatedAt = s.ID.String(), now
	_, err := s.store.db.Exec(`INSERT INTO jobs (`+jobColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Kind, j.SessionID, j.ParentID, int64p(j.TCNO), j.Label, j.Command,
		j.Description, j.State, int64p(j.PID), int64p(j.PGID), j.OutputPath,
		int64p(j.ExitCode), j.ExitReason, j.StartedAt, timeArg(j.EndedAt), j.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("session store: create job %s: %w", j.ID, err)
	}
	return &j, nil
}

// GetJob fetches one job by id within this session. A row belonging to another
// session is deliberately unknown here: the session id is part of every key
// the model can quote back to us.
func (s *Session) GetJob(id string) (*JobRow, error) {
	row, err := scanJob(s.store.db.QueryRow(
		`SELECT `+jobColumns+` FROM jobs WHERE session_id = ? AND id = ?`, s.ID.String(), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, id)
	}
	if err != nil {
		return nil, fmt.Errorf("session store: get job %s: %w", id, err)
	}
	return &row, nil
}

// ListJobs returns this session's jobs newest first, running ones included.
// The listing is the model's only source of truth for what is live - the
// transcript is a hint, since compaction keeps recency and not state - so it
// must not need a filter to be useful and must never be built from history.
func (s *Session) ListJobs() ([]JobRow, error) {
	rows, err := s.store.db.Query(`SELECT `+jobColumns+` FROM jobs
WHERE session_id = ? ORDER BY started_at DESC, id DESC`, s.ID.String())
	if err != nil {
		return nil, fmt.Errorf("session store: list jobs: %w", err)
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("session store: list jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CountRunningJobs backs the job cap and the GUI footer count, both of which
// have to answer "how many now" without materialising rows.
func (s *Session) CountRunningJobs() (int, error) {
	var n int
	if err := s.store.db.QueryRow(
		`SELECT count(*) FROM jobs WHERE session_id = ? AND state = 'running'`,
		s.ID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("session store: count jobs: %w", err)
	}
	return n, nil
}

// FinishJob moves a running job to a terminal state. It refuses to overwrite a
// state that is already terminal: the exit watcher, a kill, and the startup
// sweep can all reach the same job, and first-writer-wins is the only rule that
// keeps "who ended this" answerable.
func (s *Session) FinishJob(id, state string, exitCode *int, reason string) (*JobRow, error) {
	if !Terminal(state) {
		return nil, fmt.Errorf("session store: %q is not a terminal job state", state)
	}
	now := time.Now().UnixMilli()
	row, err := scanJob(s.store.db.QueryRow(`UPDATE jobs
SET state = ?, exit_code = ?, exit_reason = ?, ended_at = ?, updated_at = ?
WHERE session_id = ? AND id = ? AND state = 'running'
RETURNING `+jobColumns,
		state, int64p(exitCode), reason, now, now, s.ID.String(), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, id)
	}
	if err != nil {
		return nil, fmt.Errorf("session store: finish job %s: %w", id, err)
	}
	return &row, nil
}

// RunningJobIDs is the set a wait has to watch: the ids this session believes
// are still burning. It comes from the store rather than the live handles
// because the store is the source of truth for liveness, and a wait that asked
// memory could hang on a job whose row already says otherwise.
func (s *Session) RunningJobIDs() ([]string, error) {
	rows, err := s.store.db.Query(`SELECT id FROM jobs WHERE session_id = ? AND state = 'running'`,
		s.ID.String())
	if err != nil {
		return nil, fmt.Errorf("session store: running jobs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("session store: running jobs: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetJobProcess records the pid after the spawn succeeds, which is why the row
// is created first: a job that never started must still be reportable.
func (s *Session) SetJobProcess(id string, pid, pgid int) error {
	if _, err := s.store.db.Exec(`UPDATE jobs SET pid = ?, pgid = ?, updated_at = ?
WHERE session_id = ? AND id = ?`, pid, pgid, time.Now().UnixMilli(), s.ID.String(), id); err != nil {
		return fmt.Errorf("session store: record job %s process: %w", id, err)
	}
	return nil
}

// SweepOrphanedJobs ends every row this session still claims is running, and
// hands back the warnings a restart produced. It must be Go code run at open,
// not SQL in the schema const: the schema has no version tracking
// (store.go:26-29), and the sweep is a statement about processes we lost, not a
// data fix. The warning strings are returned rather than logged here so the
// front end can show the human what mini cannot prove - that nothing is still
// holding the GPU - instead of printing "still running" from a stale column.
func (s *Session) SweepOrphanedJobs() ([]string, error) {
	rows, err := s.store.db.Query(`SELECT id, label, command, started_at FROM jobs
WHERE session_id = ? AND state = 'running'`, s.ID.String())
	if err != nil {
		return nil, fmt.Errorf("session store: find orphaned jobs: %w", err)
	}
	defer rows.Close()
	type stale struct {
		id, label, command string
		started            int64
	}
	var staleRows []stale
	for rows.Next() {
		var v stale
		if err := rows.Scan(&v.id, &v.label, &v.command, &v.started); err != nil {
			return nil, fmt.Errorf("session store: find orphaned jobs: %w", err)
		}
		staleRows = append(staleRows, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(staleRows) == 0 {
		return nil, nil
	}
	now := time.Now().UnixMilli()
	if _, err := s.store.db.Exec(`UPDATE jobs SET state = 'orphaned', ended_at = ?, updated_at = ?,
	exit_reason = 'process ownership was lost when mini stopped'
WHERE session_id = ? AND state = 'running'`, now, now, s.ID.String()); err != nil {
		return nil, fmt.Errorf("session store: sweep orphaned jobs: %w", err)
	}
	warnings := make([]string, 0, len(staleRows))
	for _, v := range staleRows {
		name := v.label
		if name == "" {
			name = v.command
		}
		warnings = append(warnings, fmt.Sprintf(
			"marked reloaded running job %s (%s) as orphaned; original process ownership was lost",
			v.id, trunc(name, 60)))
		// Orphaning *is* a finish, so it queues a firing like any other, and the
		// result becomes debt the resumed conversation pays at its first step
		// boundary. Without this the model learns nothing about a soak its own
		// previous process was killed holding - the ending happened, nobody was
		// told, and the log just stopped growing.
		//
		// This is also where an unfired monitor on the job would be cancelled
		// when monitors exist: a job that finished cannot produce another
		// result, and the exit firing is its last word. See Manager.recordExit.
		if _, err := s.RecordJobEvent(JobEventRow{
			JobID: v.id, MonitorID: "exit", Reason: JobOrphaned,
			FiredAt: now, ExitReason: trunc(name, 60),
		}); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// trunc keeps a warning line to one line of terminal output.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// JobEventRow is one monitor firing: a row and not a callback, so the wait path
// and the batch injector consume the same record and a firing is delivered at
// most once. Only the table exists today; the monitor engine writes it.
type JobEventRow struct {
	ID         int64
	JobID      string
	MonitorID  string
	Reason     string
	LineNo     *int
	ByteOffset *int64
	Matched    string
	Context    string
	Elapsed    int64
	ExitCode   *int
	// ExitReason carries what the harness knew about the ending when the ending
	// was not the job's own - the orphan case, where there is no exit code and
	// the most useful fact is which run it was.
	ExitReason  string
	FiredAt     int64
	DeliveredAt *int64
}

// RecordJobEvent appends one firing.
func (s *Session) RecordJobEvent(e JobEventRow) (int64, error) {
	if e.FiredAt == 0 {
		e.FiredAt = time.Now().UnixMilli()
	}
	var id int64
	err := s.store.db.QueryRow(`INSERT INTO job_events
(session_id, job_id, monitor_id, reason, line_no, byte_offset, matched, context, elapsed, exit_code, fired_at, delivered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL) RETURNING id`,
		s.ID.String(), e.JobID, or(e.MonitorID, "exit"), e.Reason, int64p(e.LineNo),
		e.ByteOffset, e.Matched, or(e.Context, e.ExitReason), e.Elapsed, int64p(e.ExitCode), e.FiredAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("session store: record job event: %w", err)
	}
	return id, nil
}

// UndeliveredFirings counts results that happened but have not been reported.
// The loop asks this at every exit boundary: a nonzero answer is a step waiting
// to be taken, not a thing to wait for.
func (s *Session) UndeliveredFirings() (int, error) {
	var n int
	if err := s.store.db.QueryRow(`SELECT count(*) FROM job_events
WHERE session_id = ? AND delivered_at IS NULL`, s.ID.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("session store: count owed firings: %w", err)
	}
	return n, nil
}

// PendingJobEvents returns firings nobody has delivered, oldest first.
func (s *Session) PendingJobEvents(limit int) ([]JobEventRow, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.store.db.Query(`SELECT id, job_id, monitor_id, reason, line_no, byte_offset,
	matched, context, elapsed, exit_code, fired_at, delivered_at FROM job_events
WHERE session_id = ? AND delivered_at IS NULL ORDER BY id LIMIT ?`, s.ID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("session store: pending job events: %w", err)
	}
	defer rows.Close()
	var out []JobEventRow
	for rows.Next() {
		var (
			e            JobEventRow
			lineNo       sql.NullInt64
			byteOffset   sql.NullInt64
			exitCode     sql.NullInt64
			deliveredAt  sql.NullInt64
			monitor, ctx sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.JobID, &monitor, &e.Reason, &lineNo, &byteOffset,
			&e.Matched, &ctx, &e.Elapsed, &exitCode, &e.FiredAt, &deliveredAt); err != nil {
			return nil, fmt.Errorf("session store: pending job events: %w", err)
		}
		e.MonitorID, e.Context = monitor.String, ctx.String
		e.LineNo, e.ExitCode = nullInt(lineNo), nullInt(exitCode)
		if byteOffset.Valid {
			v := byteOffset.Int64
			e.ByteOffset = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkJobEventsDelivered stamps delivery time so a firing is never injected
// twice, which matters because the wake drain and the hold both read the same
// table.
func (s *Session) MarkJobEventsDelivered(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	var args []any
	ph := ""
	for i, id := range ids {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args = append(args, id)
	}
	_, err := s.store.db.Exec(`UPDATE job_events SET delivered_at = ? WHERE session_id = ? AND id IN (`+ph+`)`,
		append([]any{time.Now().UnixMilli(), s.ID.String()}, args...)...)
	if err != nil {
		return fmt.Errorf("session store: mark job events delivered: %w", err)
	}
	return nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
