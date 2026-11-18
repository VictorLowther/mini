package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"uuid"
)

// tcrUnder is where the store keeps result directories: <state dir>/tcr, named
// per session. openStore hands back the state dir, so every test that writes a
// result file has to add the segment - getting this wrong makes a collection
// look like it left files behind when the files were never where the store
// would have put them.
func tcrUnder(dir string) string { return filepath.Join(dir, "tcr") }

func daysAgo(d float64) int64 {
	return time.Now().Add(-time.Duration(d * 24 * float64(time.Hour))).UnixMilli()
}

// mkTree creates one root session in project, last touched ageDays ago, plus
// kids children whose own last-touch is kidDaysAgo (or the root's age when
// negative). Each member gets a result directory holding one file, because
// that is what collection has to reclaim.
func mkTree(t *testing.T, st *Store, tcr, project string, ageDays float64, kids int, kidDaysAgo float64) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	rootID := uuid.NewV7()
	if _, err := st.Create(Row{
		ID: rootID, Kind: KindTop, Workdir: project, Project: project,
		ModelName: "m", CreatedAt: daysAgo(ageDays + 1), UpdatedAt: daysAgo(ageDays),
	}); err != nil {
		t.Fatal(err)
	}
	writeResult(t, tcr, rootID, 1024)

	all := []uuid.UUID{rootID}
	for i := 0; i < kids; i++ {
		child := uuid.NewV7()
		age := ageDays
		if kidDaysAgo >= 0 {
			age = kidDaysAgo
		}
		if _, err := st.Create(Row{
			ID: child, ParentID: rootID.String(), Kind: KindSubagent,
			Workdir: project, Project: project, ModelName: "m",
			CreatedAt: daysAgo(age + 1), UpdatedAt: daysAgo(age),
		}); err != nil {
			t.Fatal(err)
		}
		writeResult(t, tcr, child, 512)
		all = append(all, child)
	}
	return rootID, all
}

func writeResult(t *testing.T, tcr string, id uuid.UUID, size int) {
	t.Helper()
	dir := filepath.Join(tcr, id.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.log"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The two rules together: keep N per project, sweep the rest past the idle
// window. A tree that is old but inside Keep survives, and one outside Keep but
// still fresh does too.
func TestPlanGCApplyKeepThenIdle(t *testing.T) {
	st, dir := openStore(t)
	tcr := tcrUnder(dir)
	ages := []float64{0, 1, 5, 10, 20, 40}
	for _, a := range ages {
		mkTree(t, st, tcr, "/proj", a, 0, -1)
	}
	cfg := GCConfig{Keep: 3, IdleDays: 15}
	plan, err := st.PlanGC(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 3 {
		t.Errorf("kept = %d, want 3 (the three most recent)", len(plan.Kept))
	}
	if len(plan.Sweep) != 2 {
		t.Errorf("sweep = %d, want 2 (the 20d and 40d trees)", len(plan.Sweep))
	}
	if plan.Fresh != 1 {
		t.Errorf("fresh = %d, want 1 (the 10d tree is past keep but not idle)", plan.Fresh)
	}
	// Oldest first.
	if len(plan.Sweep) == 2 && plan.Sweep[0].LastUsed > plan.Sweep[1].LastUsed {
		t.Error("sweep must be ordered oldest first")
	}
}

// The exemption is per project. A busy repo must not shelter a dead one, and a
// dead one must not put a busy repo's 4th-oldest at risk.
func TestPlanGCCountsKeepPerProject(t *testing.T) {
	st, dir := openStore(t)
	tcr := tcrUnder(dir)
	for _, proj := range []string{"/a", "/b"} {
		for _, a := range []float64{0, 1, 2, 30} {
			mkTree(t, st, tcr, proj, a, 0, -1)
		}
	}
	plan, err := st.PlanGC(GCConfig{Keep: 3, IdleDays: 15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sweep) != 2 {
		t.Fatalf("sweep = %d, want one stale tree per project", len(plan.Sweep))
	}
	got := map[string]int{}
	for _, tr := range plan.Sweep {
		got[tr.Project]++
	}
	if got["/a"] != 1 || got["/b"] != 1 {
		t.Errorf("swept per project = %v, want one each", got)
	}
	if len(plan.Kept) != 6 {
		t.Errorf("kept = %d, want 3 per project", len(plan.Kept))
	}
}

// Subagents are swept with the parent and never judged alone. The sharp case is
// the first: a long-idle parent whose CHILD is still being used must survive,
// because the tree's last activity is the max over its members. Collecting on
// the root's own timestamp would delete a live conversation's history from
// under it.
func TestPlanGCUsesWholeTreeRecency(t *testing.T) {
	st, dir := openStore(t)
	tcr := tcrUnder(dir)
	// Four idle parents, each with a child touched today, so every tree is fresh.
	for i := 0; i < 4; i++ {
		mkTree(t, st, tcr, "/p", 60, 1, 0)
	}
	plan, err := st.PlanGC(GCConfig{Keep: 3, IdleDays: 15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sweep) != 0 {
		t.Fatalf("swept %d trees whose subagents were used today", len(plan.Sweep))
	}
	if plan.Fresh != 1 {
		t.Errorf("fresh = %d, want 1 (the 4th tree is past keep but its child is active)", plan.Fresh)
	}

	// And the reverse: a genuinely idle tree is collected as a unit - one
	// candidate, not one per member - carrying both members' bytes.
	st2, dir2 := openStore(t)
	tcr2 := tcrUnder(dir2)
	// A fresh neighbour, so the idle tree falls OUTSIDE keep and is judged on
	// its age: with keep=1 and no sibling the single tree is exempt by rank and
	// this would test nothing.
	mkTree(t, st2, tcr2, "/q", 0, 0, -1)
	root, members := mkTree(t, st2, tcr2, "/q", 40, 2, 40)
	plan2, err := st2.PlanGC(GCConfig{Keep: 1, IdleDays: 15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.Sweep) != 1 {
		t.Fatalf("sweep = %d trees, want the one idle tree as a single unit", len(plan2.Sweep))
	}
	got := plan2.Sweep[0]
	if got.Root != root || got.Members != 3 {
		t.Errorf("swept root=%v members=%d, want %v/3", got.Root, got.Members, root)
	}
	if want := int64(1024 + 2*512); got.Bytes != want {
		t.Errorf("tree bytes = %d, want %d (a child's result dir is named after the CHILD)", want, want)
	}
	_ = members
}

// Collection removes rows and files for the whole tree, and a second run is a
// no-op.
func TestRunGCCollectsRowsAndFiles(t *testing.T) {
	st, dir := openStore(t)
	tcr := tcrUnder(dir)
	root, members := mkTree(t, st, tcr, "/p", 40, 2, 40)
	mkTree(t, st, tcr, "/p", 0, 0, -1) // keeps the project non-empty

	plan, err := st.RunGC(GCConfig{Keep: 1, IdleDays: 15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sweep) != 1 {
		t.Fatalf("swept %d, want 1", len(plan.Sweep))
	}
	for _, m := range members {
		if exists(filepath.Join(tcr, m.String())) {
			t.Errorf("result directory for %s survived", m)
		}
		var n int
		if err := st.db.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, m.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("session row %s survived the tree sweep", m)
		}
	}
	// The fresh tree is untouched.
	if _, err := st.Get(root); err == nil {
		t.Error("the swept root still resolves")
	}
	again, err := st.RunGC(GCConfig{Keep: 1, IdleDays: 15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Sweep) != 0 {
		t.Errorf("second collection swept %d trees, want 0 (idempotent)", len(again.Sweep))
	}
}

// A live process holding any session in the tree protects the whole tree. This
// is the difference between collecting stale history and deleting a session an
// open window is looking at - and the GUI keeps windows open for days.
func TestPlanGCSkipsTreesWithLiveOwners(t *testing.T) {
	st, dir := openStore(t)
	tcr := tcrUnder(dir)
	// Three fresh trees occupy the keep slots, so the fourth is eligible.
	for i := 0; i < 3; i++ {
		mkTree(t, st, tcr, "/p", 0, 0, -1)
	}
	dead, all := mkTree(t, st, tcr, "/p", 40, 1, 40)
	live, _ := mkTree(t, st, tcr, "/p", 41, 0, -1)
	if _, err := st.db.Exec(`UPDATE sessions SET opened_by = ? WHERE id = ?`, 4242, dead.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE sessions SET opened_by = ? WHERE id = ?`, 9999, live.String()); err != nil {
		t.Fatal(err)
	}
	alive := func(pid int) bool { return pid == 4242 } // 9999 is a crashed holder

	plan, err := st.PlanGC(GCConfig{Keep: 3, IdleDays: 15}, alive)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sweep) != 1 || plan.Sweep[0].Root != live {
		t.Errorf("sweep = %v, want the crashed holder's tree (pid 9999 is not alive)", plan.Sweep)
	}
	if len(plan.Busy) != 1 || plan.Busy[0].Root != dead {
		t.Errorf("busy = %v, want the live-owned tree protected", plan.Busy)
	}
	// A crashed holder's stale stamp must not preserve the tree forever.
	plan2, err := st.PlanGC(GCConfig{Keep: 3, IdleDays: 15}, func(int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.Sweep) != 2 {
		t.Errorf("with no live owners, sweep = %d, want 2", len(plan2.Sweep))
	}
	// PlanGC never deletes; the files must still be there to prove it.
	if !exists(filepath.Join(tcr, all[0].String())) {
		t.Error("PlanGC mutated the disk")
	}
}

// The knobs live in sqlite, default sanely, and refuse the destructive reading
// of a typo.
func TestGCConfigKnobs(t *testing.T) {
	st, _ := openStore(t)
	got, err := st.GCConfigFor()
	if err != nil {
		t.Fatal(err)
	}
	if got != (GCConfig{Keep: DefaultGCKeep, IdleDays: DefaultGCIdleDay}) {
		t.Fatalf("defaults = %+v, want keep %d idle %d", got, DefaultGCKeep, DefaultGCIdleDay)
	}
	if err := st.SetGCConfig(GCConfig{Keep: 5, IdleDays: 30}); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GCConfigFor(); got != (GCConfig{Keep: 5, IdleDays: 30}) {
		t.Errorf("round trip = %+v", got)
	}
	// Garbage and zero must not mean "keep nothing" or "sweep everything".
	for _, bad := range []string{"", "0", "-3", "banana", "  "} {
		if err := st.SetSetting(SettingGCKeepPerProject, bad); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.GCConfigFor(); got.Keep != DefaultGCKeep {
			t.Errorf("keep=%q resolved to %d, want the default", bad, got.Keep)
		}
	}
	if err := st.SetGCConfig(GCConfig{Keep: 0, IdleDays: 15}); err == nil {
		t.Error("keep=0 must be refused, not stored")
	}
	if err := st.SetGCConfig(GCConfig{Keep: 1, IdleDays: 0}); err == nil {
		t.Error("idle=0 must be refused, not stored")
	}
}
