package session

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uuid"
)

// These are the tests for docs/parallel-sessions.md §7 step 1, and the two that
// matter most are the ones the document says to test first because they can
// cost a user something today: a resume that picks the wrong conversation, and a
// delete that quietly strands the largest files mini writes.

// oldSessionsDDL is the sessions table exactly as it was before lineage existed
// (git HEAD~ at the time of writing), used to build a database that the
// migration has to cope with rather than one it created.
const oldSessionsDDL = `
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
);`

func newChild(t *testing.T, s *Store, parent *Session, title string) *Session {
	t.Helper()
	kid, err := s.Create(Row{
		ID: uuid.NewV7(), ParentID: parent.ID.String(), Kind: KindSubagent,
		Title: title, Workdir: parent.Workdir, Project: parent.Project, ModelName: "test",
	})
	if err != nil {
		t.Fatalf("create child %q: %v", title, err)
	}
	return kid
}

// writeResultDir stands in for the per-call files OpenToolCall and JobPath
// create, so the tests can assert on the directory rather than on a row.
func writeResultDir(t *testing.T, base string, id uuid.UUID, name string) string {
	t.Helper()
	dir := filepath.Join(base, "tcr", id.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestChildrenInvisibleToResumeAndRm is the §3.5.7 bug, expressed as a test.
// findResumable picks the newest updated_at row in the project when handed a
// bare -r, and `mini sessions rm` resolves its prefix through Find. Both are
// List/Find, so if a subagent row could be returned by either, a finished
// subagent - which always updates more recently than the parent that spawned it,
// in the same directory - would shadow the human's own conversation.
func TestChildrenInvisibleToResumeAndRm(t *testing.T) {
	st, _ := openStore(t)
	parent := newSession(t, st)
	kid := newChild(t, st, parent, "quantise 70b")

	// The child is the more recently touched session, the exact condition that
	// makes the ordering alone a hazard.
	if _, err := st.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		time.Now().Add(time.Hour).UnixMilli(), kid.ID.String()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.List(parent.Project, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != parent.ID {
		t.Fatalf("List = %v, want only the parent", rows)
	}

	// What findResumable does with a bare -r.
	newest, err := st.List(parent.Project, false, 1)
	if err != nil || len(newest) != 1 || newest[0].ID != parent.ID {
		t.Fatalf("resume pick = %+v, %v; want the parent", newest, err)
	}

	// And what `mini sessions rm <child prefix>` does: not found. The child is
	// removable only with its parent (§5.11), so the same filter that protects
	// resume protects rm without a second rule anywhere.
	// The child's *whole* id, not a prefix: a uuid v7 begins with a millisecond
	// timestamp, so a parent and a child created in the same tick share their
	// first eight characters, and an eight-char prefix here is a query that
	// matches both and would have failed for the wrong reason.
	if _, err := st.Find(parent.Project, kid.ID.String()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Find(child id) err = %v, want sql.ErrNoRows", err)
	}
	if got, err := st.Find(parent.Project, parent.ID.String()); err != nil || got.ID != parent.ID {
		t.Fatalf("Find(parent id) = %v, %v; want the parent", got, err)
	}

	// The child is still reachable by the one query that means to reach it.
	kids, err := st.Children(parent.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].ID != kid.ID || kids[0].Kind != KindSubagent {
		t.Fatalf("Children = %+v, want the one subagent", kids)
	}
}

// TestDeleteParentRemovesChildrenAndTheirFiles is the directory leak. The rows
// would go on their own through ON DELETE CASCADE; the files would not, because
// their path is derived from a row the cascade has already erased. A Delete that
// passes a row-count test and leaks three directories is the bug this exists to
// keep from being reintroduced.
func TestDeleteParentRemovesChildrenAndTheirFiles(t *testing.T) {
	st, base := openStore(t)
	parent := newSession(t, st)
	k1 := newChild(t, st, parent, "first")
	k2 := newChild(t, st, parent, "second")

	dirs := []string{
		writeResultDir(t, base, parent.ID, "1.log"),
		writeResultDir(t, base, k1.ID, "job-j1.log"), // JobPath lives in here
		writeResultDir(t, base, k2.ID, "3.log"),
	}
	for _, d := range dirs {
		if !dirExists(d) {
			t.Fatalf("fixture missing %s", d)
		}
	}

	if err := st.Delete(parent.ID); err != nil {
		t.Fatal(err)
	}

	for _, id := range []uuid.UUID{parent.ID, k1.ID, k2.ID} {
		if _, err := st.Get(id); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("session %s still present after parent delete: %v", id, err)
		}
	}
	for _, d := range dirs {
		if dirExists(d) {
			t.Errorf("result directory %s survived the deletion of its session", d)
		}
	}
}

// TestDeleteChildLeavesSiblings is why the subtree is collected by parent_id and
// not by root_id: siblings share a root, so a root-scoped delete would take out
// brothers and sisters nobody asked about.
func TestDeleteChildLeavesSiblings(t *testing.T) {
	st, base := openStore(t)
	parent := newSession(t, st)
	k1 := newChild(t, st, parent, "keep")
	k2 := newChild(t, st, parent, "remove")
	d1 := writeResultDir(t, base, k1.ID, "1.log")
	d2 := writeResultDir(t, base, k2.ID, "1.log")

	if err := st.Delete(k2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(k1.ID); err != nil {
		t.Errorf("sibling was taken with it: %v", err)
	}
	if !dirExists(d1) {
		t.Error("sibling's files were taken with it")
	}
	if dirExists(d2) {
		t.Error("the deleted child's directory survived")
	}
	if _, err := st.Get(parent.ID); err != nil {
		t.Errorf("removing a child must not touch the parent: %v", err)
	}
}

func TestCreateRefusesAGrandchildAndAnUnknownParent(t *testing.T) {
	st, _ := openStore(t)
	parent := newSession(t, st)
	kid := newChild(t, st, parent, "leaf")

	// The two-level tree is enforced here as well as by hiding the spawn tool,
	// so the rule does not depend on every caller having read §5.6.
	if _, err := st.Create(Row{ID: uuid.NewV7(), ParentID: kid.ID.String(), Kind: KindSubagent}); err == nil {
		t.Fatal("a grandchild was created; §5.6 says the tree is two levels")
	} else if !strings.Contains(err.Error(), "cannot have children") {
		t.Errorf("refusal = %v, want the grandchild rule", err)
	}

	if _, err := st.Create(Row{ID: uuid.NewV7(), ParentID: uuid.NewV7().String()}); err == nil {
		t.Fatal("a child of a missing parent was created")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("refusal = %v, want the missing-parent error", err)
	}

	if _, err := st.Create(Row{ID: uuid.NewV7(), Kind: "banana"}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func TestRootIsTheAncestorsOwnID(t *testing.T) {
	st, _ := openStore(t)
	parent := newSession(t, st)
	if parent.RootID != parent.ID.String() {
		t.Errorf("top-level root = %q, want its own id %q", parent.RootID, parent.ID.String())
	}
	kid := newChild(t, st, parent, "k")
	if kid.RootID != parent.ID.String() {
		t.Errorf("child root = %q, want the parent's id", kid.RootID)
	}
	// A caller that names a root gets it honored; one that names nothing gets the
	// inherited value rather than ''.
	if _, err := st.Get(kid.ID); err != nil {
		t.Fatal(err)
	}
}

// TestTreeTotalsIsInclusive covers §5.8's cost model: the human sees one number
// for the run, and it is computed rather than accumulated, so a child that
// finishes while the parent is mid-turn cannot be missed or counted twice.
func TestTreeTotalsIsInclusive(t *testing.T) {
	st, _ := openStore(t)
	parent := newSession(t, st)
	if _, err := st.db.Exec(`UPDATE sessions SET cost = 1.5, n_calls = 3 WHERE id = ?`, parent.ID.String()); err != nil {
		t.Fatal(err)
	}
	for i, c := range []float64{0.25, 0.5} {
		kid := newChild(t, st, parent, "k")
		if _, err := st.db.Exec(`UPDATE sessions SET cost = ?, n_calls = ? WHERE id = ?`,
			c, i+1, kid.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	cost, calls, err := st.TreeTotals(parent.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if cost != 2.25 || calls != 6 {
		t.Errorf("tree totals = %v/%v, want 2.25/6", cost, calls)
	}
	// And the parent's own row is untouched: the rollup is a query, never a
	// write into somebody else's ledger.
	own, err := st.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if own.Cost != 1.5 {
		t.Errorf("parent's own cost = %v, want 1.5 unchanged", own.Cost)
	}
}

func TestSweepLineageRemovesOrphanedChildren(t *testing.T) {
	st, base := openStore(t)
	parent := newSession(t, st)
	kid := newChild(t, st, parent, "orphan-to-be")

	// Build the state the sweep repairs: a child row whose parent is gone. The
	// only way to make one is to bypass the foreign key, which is exactly the
	// point - ON DELETE CASCADE means no path through this package can produce
	// an orphan, so an orphan can only arrive from outside: a database written
	// by a build that predated the cascade, or a hand edit.
	if err := st.Delete(parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Exec(`PRAGMA foreign_keys=ON`) })
	if _, err := st.db.Exec(`INSERT INTO sessions (id, parent_id, root_id, kind, title, workdir, project, model, cost, n_calls, exit_status, next_tcno, created_at, updated_at)
VALUES (?, ?, ?, 'subagent', 'orphaned child', '', '', 'test', 0, 0, '', 0, ?, ?)`,
		kid.ID.String(), parent.ID.String(), kid.ID.String(),
		time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed the orphan: %v", err)
	}
	if _, err := st.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	// The directory is written after the parent delete on purpose: Delete
	// collects the subtree, so a directory created before it would be - correctly
	// - removed with the child's row, and the sweep's whole job is the residue
	// that outlives a row.
	dir := writeResultDir(t, base, kid.ID, "1.log")

	warnings, err := st.SweepLineage()
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "orphan") {
		t.Fatalf("warnings = %v, want one naming the orphaned child", warnings)
	}
	if _, err := st.Get(kid.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("orphaned child survived the sweep: %v", err)
	}
	if dirExists(dir) {
		t.Error("the orphaned child's files survived the sweep")
	}
}

func TestSweepStrayResultsKeepsLiveAndForeign(t *testing.T) {
	st, base := openStore(t)
	live := newSession(t, st)
	liveDir := writeResultDir(t, base, live.ID, "1.log")

	// A directory whose row is gone: the crash-between-the-two case.
	gone := uuid.NewV7()
	goneDir := writeResultDir(t, base, gone, "1.log")

	// Something that is not ours at all - a file, and a directory with a name
	// that cannot be a session id.
	if err := os.WriteFile(filepath.Join(base, "tcr", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(base, "tcr", "not-a-uuid")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	n, err := st.SweepStrayResults()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if dirExists(goneDir) {
		t.Error("the rowless directory survived")
	}
	if !dirExists(liveDir) {
		t.Error("a live session's files were removed - the sweep is not allowed to do this")
	}
	if !dirExists(foreign) {
		t.Error("an entry that is not a session uuid was removed")
	}
	if _, err := os.Stat(filepath.Join(base, "tcr", "notes.txt")); err != nil {
		t.Errorf("a loose file was removed: %v", err)
	}
}

// TestLineageMigration opens a database that was created before the lineage
// columns existed and requires that it come up usable, backfilled, and able to
// hold a child whose root resolves correctly.
func TestLineageMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")

	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(oldSessionsDDL); err != nil {
		t.Fatal(err)
	}
	legacy := uuid.NewV7()
	if _, err := raw.Exec(`INSERT INTO sessions (id, title, workdir, project, model, cost, n_calls, exit_status, next_tcno, created_at, updated_at)
VALUES (?, 'legacy', '/w', '/p', 'test', 0, 0, '', 0, ?, ?)`, legacy.String(), time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer st.Close()

	got, err := st.Get(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindTop {
		t.Errorf("migrated kind = %q, want %q", got.Kind, KindTop)
	}
	if got.RootID != legacy.String() {
		t.Errorf("migrated root_id = %q, want its own id", got.RootID)
	}

	// A child created after migration must inherit the backfilled root, not the
	// empty string the column defaulted to.
	kid := newChild(t, st, got, "post-migration")
	if kid.RootID != legacy.String() {
		t.Errorf("child root = %q, want the legacy id", kid.RootID)
	}
	cost, _, err := st.TreeTotals(legacy.String())
	if err != nil || cost != 0 {
		t.Errorf("tree totals on migrated rows = %v, %v", cost, err)
	}
	// Reopening an already-migrated database is a no-op, not an error from
	// adding a column twice.
	st2, err := Open(path, filepath.Join(dir, "tcr"))
	if err != nil {
		t.Fatalf("second open of a migrated database: %v", err)
	}
	st2.Close()
}
