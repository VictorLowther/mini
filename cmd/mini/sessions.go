package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/host"
	"github.com/VictorLowther/mini/session"
)

const sessionsHelp = `Usage:
  mini sessions [--all] [--limit N]         list sessions for this repo
  mini sessions rm <uuid-prefix>             delete one session, its transcript and its result files
  mini sessions export [-o FILE] [<uuid-prefix>]   write one session's trajectory out
  mini sessions gc [-n] [-keep N] [-idle-days D]  collect idle sessions, or preview it

Lists the stored sessions, newest update first, for the repository (or
working directory) the command runs in. --all covers every project.

export is the only way a trajectory file gets written: running a session
saves nothing but the store. With no prefix it takes this project's most
recent session, and with no -o it writes mini-<short id>.traj.json in the
working directory. A subagent's prefix works too - a child's transcript is in
the store like anyone else's, and exporting is neither resuming nor deleting.

The document is rebuilt from the store, so it carries what the store kept: the
session's own facts (id, kind, lineage, project, model, cost, steps, exit
status) and its full message list. It has no config block, because the
configuration a session ran under was never recorded.

gc applies the retention policy: the N most recently used session trees per
project are always exempt, and anything else is collected after D days without
activity. A tree is a session and its subagents, which are collected with their
parent and never judged on their own - and a tree whose child was used today
counts as used today. Collection removes the rows and the result files together.
-n previews the plan and deletes nothing. -keep and -idle-days override the
numbers for that run only; add -save to persist them as the global settings,
which live in the database rather than the config file; the same pass also runs by itself whenever mini starts. A tree
holding a session another live process has open is skipped, so collecting from
the command line cannot delete what a window is looking at.
`

// runSessionsCommand lists, deletes and exports sessions. STEPS counts model calls
// (Loop.NCalls), not tool executions.
func runSessionsCommand(args []string, stdout io.Writer) error {
	// Subcommands are recognized before any parsing, because Go's flag package
	// stops at the first positional: with one FlagSet for the whole verb, the
	// "-o" in `mini sessions export -o f.json` would never be parsed as a flag -
	// and would then be handed to the session lookup as a uuid prefix.
	if len(args) > 0 && args[0] == "export" {
		return runSessionsExport(args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "gc" {
		return runSessionsGC(args[1:], stdout)
	}

	fs := flag.NewFlagSet("mini sessions", flag.ContinueOnError)
	all := fs.Bool("all", false, "list every project, not just the current one")
	limit := fs.Int("limit", 20, "maximum number of sessions to show")
	fs.Usage = func() { fmt.Fprint(fs.Output(), sessionsHelp) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, project, err := sessionsScope()
	if err != nil {
		return err
	}
	defer store.Close()
	if *all {
		project = ""
	}

	if fs.NArg() > 0 && fs.Arg(0) == "rm" {
		if fs.NArg() != 2 {
			fs.Usage()
			return errors.New("sessions rm needs exactly one session uuid prefix")
		}
		sess, err := store.Find(project, fs.Arg(1))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no session matching %q in this project", fs.Arg(1))
			}
			return err
		}
		// The cascade removes the turns and tool_calls rows; Delete removes
		// the result directory. A run still writing this session logs its
		// next failed RecordMessage and carries on in memory; no lock.
		if err := store.Delete(sess.ID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted session %s\n", sess.ID)
		return nil
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	rows, err := store.List(project, *all, *limit)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	// COST is the session's own spend and TREE is the whole run's, kept as two
	// columns because they answer two different questions and the design
	// separates them deliberately: cost_limit bounds a session, while the number
	// a human wants for "what did this cost me" includes every subagent under it
	// (docs/parallel-sessions.md §5.8). Collapsing them would make the footer
	// disagree with the limit that stopped the run.
	fmt.Fprintln(w, "SESSION UUID\tUPDATED\tSTEPS\tCOST\tTREE\tTASK")
	for _, r := range rows {
		tree := "-"
		if kids, err := store.Children(r.ID.String()); err == nil && len(kids) > 0 {
			if cost, calls, err := store.TreeTotals(r.ID.String()); err == nil {
				tree = fmt.Sprintf("$%.2f/%d (%d sub)", cost, calls, len(kids))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t$%.2f\t%s\t%s\n",
			r.ID, time.UnixMilli(r.UpdatedAt).Format("2006-01-02 15:04"), r.NCalls, r.Cost, tree, r.Title)
	}
	return w.Flush()
}

// sessionsScope opens the store the subcommands read and works out which project
// they mean: the repository (or working directory) the command runs in, the same
// identity `mini -r` resolves against.
func sessionsScope() (*session.Store, string, error) {
	store, err := session.Open(config.SessionDB(), config.ToolCallResultsDir())
	if err != nil {
		return nil, "", err
	}
	wd, err := os.Getwd()
	if err != nil {
		store.Close()
		return nil, "", err
	}
	_, project := projectOf(wd)
	return store, project, nil
}

// runSessionsExport is `mini sessions export [-o FILE] [uuid-prefix]`.
func runSessionsExport(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mini sessions export", flag.ContinueOnError)
	out := fs.String("o", "", "file to write the trajectory to")
	fs.Usage = func() { fmt.Fprint(fs.Output(), sessionsHelp) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(1))
	}
	store, project, err := sessionsScope()
	if err != nil {
		return err
	}
	defer store.Close()

	prefix := ""
	if fs.NArg() == 1 {
		prefix = fs.Arg(0)
	}
	written, err := exportSession(store, project, prefix, *out)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Exported trajectory to %s\n", written)
	return nil
}

// exportSession writes one stored session's trajectory out. An empty prefix means
// this project's most recent session - the thing a human who just finished a run
// almost always wants - and an empty path names the file after the session in the
// working directory, so the default destination is where they were standing rather
// than a state directory nobody looks in.
//
// Children resolve here through FindAny, which is deliberate: a subagent's
// transcript is as worth exporting as its parent's, and export is neither resume
// nor delete, the two operations whose kind = 'top' filter must not be widened.
func exportSession(store *session.Store, project, prefix, path string) (string, error) {
	if prefix == "" {
		rows, err := store.List(project, project == "", 1)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "", errors.New("no stored session for this project")
		}
		sess, err := store.Get(rows[0].ID)
		if err != nil {
			return "", err
		}
		return sess.ExportTrajectory(path)
	}
	sess, err := store.FindAny(project, prefix)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no session matching %q in this project", prefix)
		}
		return "", err
	}
	return sess.ExportTrajectory(path)
}

// runSessionsGC is `mini sessions gc [-n] [-keep N] [-idle-days D]`.
//
// It talks to the store directly rather than going through host.New, which
// matters twice over: New runs a collection pass of its own, so a preview
// invoked as `gc -n` would otherwise delete on the way in, and New would also
// bring up a job registry and a process context for a command that only reads
// and prunes rows.
func runSessionsGC(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mini sessions gc", flag.ContinueOnError)
	dry := fs.Bool("n", false, "preview the plan; delete nothing")
	dryLong := fs.Bool("dry-run", false, "preview the plan; delete nothing")
	keep := fs.Int("keep", 0, "override the exempt tree count for this run")
	idle := fs.Int("idle-days", 0, "override the idle window for this run")
	save := fs.Bool("save", false, "persist the -keep / -idle-days values as the global settings")
	fs.Usage = func() { fmt.Fprint(fs.Output(), sessionsHelp) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, _, err := sessionsScope()
	if err != nil {
		return err
	}
	defer store.Close()

	cfg, err := store.GCConfigFor()
	if err != nil {
		return err
	}
	if *keep > 0 {
		cfg.Keep = *keep
	}
	if *idle > 0 {
		cfg.IdleDays = *idle
	}
	// Overriding for a preview must not rewrite the global numbers, or "what
	// would this collect at 1 day?" quietly changes what every later run does -
	// and the destructive direction is the one a curious user tries first. The
	// values apply to this run unless -save says otherwise.
	if *save {
		if *keep <= 0 && *idle <= 0 {
			return errors.New("gc: -save needs -keep and/or -idle-days to save")
		}
		if err := store.SetGCConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Saved: keep %d trees per project, collect after %d idle days\n",
			cfg.Keep, cfg.IdleDays)
	} else if *keep > 0 || *idle > 0 {
		fmt.Fprintf(stdout, "(overriding the stored policy for this run only; add -save to keep it)\n")
	}

	plan, err := store.PlanGC(cfg, host.ProcessAlive)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Policy: keep the %d most recent trees per project; collect the rest after %s idle\n",
		cfg.Keep, pluralDays(cfg.IdleDays))
	fmt.Fprintf(stdout, "Exempt: %d by recency, %d held by a live process, %d still within the window\n",
		len(plan.Kept), len(plan.Busy), plan.Fresh)

	if len(plan.Sweep) == 0 {
		fmt.Fprintln(stdout, "Nothing to collect.")
		return nil
	}
	for _, t := range plan.Sweep {
		fmt.Fprintf(stdout, "  %s  %-28s  %d session(s), %8s  idle since %s\n",
			t.Root.String()[:8], filepath.Base(t.Project), t.Members,
			humanSize(t.Bytes), time.UnixMilli(t.LastUsed).Format("2006-01-02"))
	}
	fmt.Fprintf(stdout, "%s, %s, %s of result files\n",
		plural(len(plan.Sweep), "tree"), plural(plan.Sessions(), "session"), humanSize(plan.Bytes()))

	if *dry || *dryLong {
		fmt.Fprintln(stdout, "Dry run: nothing was deleted.")
		return nil
	}
	// Re-run through the store so the deletion path is the one New uses, and
	// report what it actually removed rather than what the preview measured.
	done, err := store.RunGC(cfg, host.ProcessAlive)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Collected %s (%s, %s).\n",
		plural(len(done.Sweep), "tree"), plural(done.Sessions(), "session"), humanSize(done.Bytes()))
	return nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func pluralDays(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// humanSize keeps the report scannable; the numbers are disk, not tokens.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
