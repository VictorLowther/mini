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
	fmt.Fprintln(w, "SESSION UUID\tUPDATED\tSTEPS\tCOST\tTASK")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t$%.2f\t%s\n",
			r.ID, time.UnixMilli(r.UpdatedAt).Format("2006-01-02 15:04"), r.NCalls, r.Cost, r.Title)
	}
	return w.Flush()
}
