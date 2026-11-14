//go:build gui

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fyne.io/fyne/v2/app"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/ui"
)

// runGUI opens the desktop window. The setup is run()'s: same flags, same
// buildRun, same post-run report - the window replaces the terminal, not the
// contract.
func runGUI(args []string) int {
	o, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "mini:", err)
		return 2
	}
	if o.showVersion {
		fmt.Fprintf(os.Stdout, "mini-go %s\n", agent.Version)
		return 0
	}

	// The logger keeps writing to the terminal that launched the window; the
	// GUI mutes it for the duration of the run, like the pane.
	logTarget := ui.NewLogTarget(os.Stderr)
	closeLog := setupLogging(o.logFile, logTarget)
	defer closeLog()

	a := app.New()
	r, err := buildRun(o, os.Stdin, os.Stdout, logTarget)
	if err != nil {
		// The window is the surface the user chose: show the failure there
		// instead of quitting one they never got to see.
		g := ui.NewGUI(ui.GUIOptions{App: a, LogWriter: logTarget})
		return g.ShowError(err)
	}
	defer r.closeStore()

	g := ui.NewGUI(ui.GUIOptions{
		Cfg: r.AgentCfg, Loop: r.Loop, Model: r.Model,
		ConfigName: r.ConfigName, LogWriter: logTarget, App: a,
	})
	// A stalled window with no explanation is indistinguishable from a crash,
	// so the retry schedule goes into the transcript.
	r.Model.OnRetry = func(attempt, max int, err error, wait time.Duration) {
		g.RetryNotice(attempt, max, err, wait)
	}
	// The window is the UI, not the confirmation logic: without the same
	// Interactive adapter the pane and --print get, the loop never calls
	// OnMessage and the transcript never commits a turn.
	h, err := ui.New(g, r.AgentCfg, r.Loop)
	if err != nil {
		return g.ShowError(err)
	}
	r.Loop.Hooks = h
	g.SetSessionID(r.Sess.ID.String())
	g.Resume(r.ResumeMsgs, r.ResumeNote)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	info, err := g.Run(ctx, o.task)
	reportRun(r.Loop, info, err)
	fmt.Fprintf(os.Stdout, "Saved trajectory to '%s'\n", r.AgentCfg.OutputPath)
	fmt.Fprintf(os.Stderr, "Session %s — resume with mini -r %s\n", r.Sess.ID, r.Sess.ID)
	return exitCode(info, err)
}
