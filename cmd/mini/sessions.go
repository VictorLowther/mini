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

Lists the stored sessions, newest update first, for the repository (or
working directory) the command runs in. --all covers every project.
`

// runSessionsCommand lists and deletes sessions. STEPS counts model calls
// (Loop.NCalls), not tool executions.
func runSessionsCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mini sessions", flag.ContinueOnError)
	all := fs.Bool("all", false, "list every project, not just the current one")
	limit := fs.Int("limit", 20, "maximum number of sessions to show")
	fs.Usage = func() { fmt.Fprint(fs.Output(), sessionsHelp) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := session.Open(config.SessionDB(), config.ToolCallResultsDir())
	if err != nil {
		return err
	}
	defer store.Close()

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	_, project := projectOf(wd)
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
