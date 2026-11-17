package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/session"
)

const sessionsHelp = `Usage:
  mini sessions [--all] [--limit N]         list sessions for this repo
  mini sessions rm <uuid-prefix>             delete one session, its transcript and its result files
  mini sessions export [-o FILE] [<uuid-prefix>]   write one session's trajectory out

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
