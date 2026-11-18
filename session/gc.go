package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"uuid"
)

// Session garbage collection.
//
// The owner's policy, stated 2026-11-18, and the shape of this file:
//
//   - the unit of collection is a whole session TREE, keyed by its top-level
//     ancestor, and grouped per project directory (the git root when there is
//     one, else the working directory - the same grain as
//     idx_sessions_project_updated);
//   - the keep most recently used trees per project are exempt, always;
//   - everything else is swept after idle days without activity;
//   - a subagent is swept with its parent and never on its own judgement, which
//     is why a child's recency keeps its parent's tree alive and why deletion is
//     issued only against roots (Store.Delete already collects the subtree and
//     removes the result directory);
//   - both numbers are global knobs kept in the settings table, not in the
//     config file: this is interface state that must outlive a run and be
//     settable without a restart of whoever reads it, which is exactly what the
//     settings table is for.
//
// PlanGC mutates nothing. RunGC is PlanGC plus one Store.Delete per candidate,
// so a dry run and a real run see the identical plan rather than two
// implementations of the same rule that could drift.
//
// Nothing here is scheduled: it runs at process start, beside the lineage and
// stray-result sweeps in host.New. A tool that is invoked by opening it has no
// daemon to hold a timer, and startup is already the moment the previous run's
// files are known-stale.

// Setting keys and the defaults they fall back to.
const (
	SettingGCKeepPerProject = "gc.keep_per_project"
	SettingGCIdleDays       = "gc.idle_days"

	DefaultGCKeep    = 3
	DefaultGCIdleDay = 15
)

// GCConfig is the pair of knobs, as resolved for one collection.
type GCConfig struct {
	// Keep is how many of the most recently used trees per project are exempt.
	Keep int
	// IdleDays is how long a tree that is not in Keep must go unused before it
	// is swept.
	IdleDays int
}

// GCConfigFor reads the knobs, substituting the default for anything absent or
// nonsensical. A zero or negative stored value is treated as "no opinion"
// rather than as "keep nothing" or "sweep everything": the destructive reading
// of a typo is the worse one, and there is no configuration that makes
// idle_days=0 mean anything a user would want.
func (s *Store) GCConfigFor() (GCConfig, error) {
	out := GCConfig{Keep: DefaultGCKeep, IdleDays: DefaultGCIdleDay}
	for _, knob := range []struct {
		key  string
		to   *int
		name string
	}{
		{SettingGCKeepPerProject, &out.Keep, "keep"},
		{SettingGCIdleDays, &out.IdleDays, "idle days"},
	} {
		raw, ok, err := s.Setting(knob.key)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			continue
		}
		*knob.to = v
	}
	return out, nil
}

// SetGCConfig persists the knobs. Both are required to be positive, because the
// only sane meanings of zero here are "unlimited" and "never", and neither is a
// thing this collects.
func (s *Store) SetGCConfig(c GCConfig) error {
	if c.Keep <= 0 {
		return fmt.Errorf("session store: gc keep must be at least 1, got %d", c.Keep)
	}
	if c.IdleDays <= 0 {
		return fmt.Errorf("session store: gc idle days must be at least 1, got %d", c.IdleDays)
	}
	if err := s.SetSetting(SettingGCKeepPerProject, strconv.Itoa(c.Keep)); err != nil {
		return err
	}
	return s.SetSetting(SettingGCIdleDays, strconv.Itoa(c.IdleDays))
}

// GCTree is one candidate unit: a root session and everything under it.
type GCTree struct {
	Root    uuid.UUID
	Project string
	// LastUsed is the newest updated_at anywhere in the tree, in unix
	// milliseconds - including the subagents, so a parent whose child is still
	// working is not "idle".
	LastUsed int64
	// Members is how many sessions the tree holds, root included.
	Members int
	// Bytes is the size of the tree's result directory on disk.
	Bytes int64
}

// Idle reports whether the tree is past cfg's inactivity threshold at now.
func (t GCTree) Idle(cfg GCConfig, now int64) bool {
	cutoff := now - int64(cfg.IdleDays)*int64(24*time.Hour/time.Millisecond)
	return t.LastUsed <= cutoff
}

// GCPlan is the outcome of a collection decision, split by WHY each tree was
// spared. Reporting only the swept set would make the exempt one invisible, and
// the exempt set is what tells a user whether the numbers they set are doing
// what they meant.
type GCPlan struct {
	Config GCConfig
	// Sweep is everything to delete, oldest first.
	Sweep []GCTree
	// Kept are the trees spared by the per-project recency rule.
	Kept []GCTree
	// Busy are the trees spared because a live process still owns a session in
	// them - the cross-process guard, which is the difference between collecting
	// an old session and deleting one a window is looking at.
	Busy []GCTree
	// Fresh are past the keep count but inside the idle window.
	Fresh int
}

// Bytes returns the disk the sweep would reclaim, and Freed counts what the run
// actually removed.
func (p GCPlan) Bytes() int64 {
	var n int64
	for _, t := range p.Sweep {
		n += t.Bytes
	}
	return n
}

// Sessions is the total session-row count the sweep covers, children included.
func (p GCPlan) Sessions() int {
	var n int
	for _, t := range p.Sweep {
		n += t.Members
	}
	return n
}

// treeRow is one grouped row of the candidate query.
type treeRow struct {
	root     string
	project  string
	lastUsed int64
	members  int
	// ids is every member's own id, comma-joined. Result directories are named
	// after the SESSION that wrote them - a subagent's output lives under the
	// subagent's id, not the root's - so a tree's footprint is the sum over its
	// members. Store.Delete walks the same set for the same reason.
	ids string
}

// PlanGC decides what to collect without collecting it.
//
// liveOwner reports whether a pid belongs to a running process; it is injected
// because liveness is an OS question and this package must not grow one. A nil
// predicate means "assume nobody is holding anything", which is what a
// single-process CLI or a test wants.
func (s *Store) PlanGC(cfg GCConfig, liveOwner func(pid int) bool) (GCPlan, error) {
	plan := GCPlan{Config: cfg}
	rows, err := s.db.Query(`
		SELECT COALESCE(NULLIF(s.root_id, ''), s.id) AS unit,
		       COALESCE(NULLIF(r.project, ''), NULLIF(s.project, ''), '') AS project,
		       MAX(s.updated_at) AS last_used,
		       COUNT(*) AS members,
		       GROUP_CONCAT(s.id, ',') AS ids
		FROM sessions s
		LEFT JOIN sessions r ON r.id = COALESCE(NULLIF(s.root_id, ''), s.id)
		GROUP BY unit
		ORDER BY last_used DESC, unit ASC`)
	if err != nil {
		return plan, fmt.Errorf("session store: gc candidates: %w", err)
	}
	var trees []treeRow
	for rows.Next() {
		var t treeRow
		if err := rows.Scan(&t.root, &t.project, &t.lastUsed, &t.members, &t.ids); err != nil {
			rows.Close()
			return plan, fmt.Errorf("session store: scan gc candidates: %w", err)
		}
		trees = append(trees, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return plan, fmt.Errorf("session store: gc candidates: %w", err)
	}
	rows.Close() // the single pool connection must be back before we write or walk

	// Which trees a live process is holding, by any member's owner stamp.
	busy := map[string][]int{}
	holders, err := s.db.Query(`SELECT COALESCE(NULLIF(root_id, ''), id) AS unit, opened_by
		FROM sessions WHERE opened_by IS NOT NULL AND opened_by != 0`)
	if err != nil {
		return plan, fmt.Errorf("session store: gc holders: %w", err)
	}
	for holders.Next() {
		var unit string
		var pid int
		if err := holders.Scan(&unit, &pid); err != nil {
			holders.Close()
			return plan, fmt.Errorf("session store: scan gc holders: %w", err)
		}
		busy[unit] = append(busy[unit], pid)
	}
	if err := holders.Err(); err != nil {
		holders.Close()
		return plan, fmt.Errorf("session store: gc holders: %w", err)
	}
	holders.Close()

	now := time.Now().UnixMilli()
	// The candidate query already ordered by recency, so counting up per project
	// while walking it IS the "most recently used" ranking. Sorting again here
	// would be a second definition of the same rule.
	seen := map[string]int{}
	for _, tr := range trees {
		id, err := uuid.Parse(tr.root)
		if err != nil {
			continue // a unit key that is not a session id is not ours to collect
		}
		t := GCTree{Root: id, Project: tr.project, LastUsed: tr.lastUsed, Members: tr.members}
		seen[tr.project]++
		rank := seen[tr.project]

		if rank <= cfg.Keep {
			plan.Kept = append(plan.Kept, t)
			continue
		}
		if liveOwner != nil {
			live := false
			for _, pid := range busy[tr.root] {
				if liveOwner(pid) {
					live = true
					break
				}
			}
			if live {
				plan.Busy = append(plan.Busy, t)
				continue
			}
		}
		if !t.Idle(cfg, now) {
			plan.Fresh++
			continue
		}
		for _, member := range strings.Split(tr.ids, ",") {
			if member != "" {
				t.Bytes += dirBytes(filepath.Join(s.tcrBase, member))
			}
		}
		plan.Sweep = append(plan.Sweep, t)
	}
	// Oldest first: a long sweep then reports the stale end of history up front,
	// and a crash partway through leaves the newest survivors rather than the
	// oldest.
	sort.SliceStable(plan.Sweep, func(i, j int) bool {
		return plan.Sweep[i].LastUsed < plan.Sweep[j].LastUsed
	})
	return plan, nil
}

// RunGC executes a plan, returning the plan narrowed to what was really removed.
// Each unit is deleted by Store.Delete against its ROOT,
// which is what makes "subagents are swept with their parent, not independently"
// true by construction: a child id is never handed to Delete here, and Delete
// collects the subtree and the result directory itself.
func (s *Store) RunGC(cfg GCConfig, liveOwner func(pid int) bool) (GCPlan, error) {
	plan, err := s.PlanGC(cfg, liveOwner)
	if err != nil {
		return plan, err
	}
	// A tree that could not be deleted is put back in Kept rather than dropped
	// silently or left in Sweep: the caller's report must describe what actually
	// happened, and one unreadable row must not abandon the rest of the sweep.
	// plan.Bytes() then means freed bytes, because Sweep has been narrowed to
	// what was removed.
	done := plan.Sweep[:0]
	for _, t := range plan.Sweep {
		if err := s.Delete(t.Root); err != nil {
			plan.Kept = append(plan.Kept, t)
			continue
		}
		done = append(done, t)
	}
	plan.Sweep = done
	return plan, nil
}

// dirBytes totals a directory tree, tolerating anything that vanishes mid-walk.
// Only ever called on sweep candidates, so it never walks the whole store.
func dirBytes(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
