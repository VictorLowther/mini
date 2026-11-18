// Command mini is the local bash agent: the Go port of mini-swe-agent's `mini`
// executable, talking to a sushi server.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/host"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
	"github.com/VictorLowther/mini/ui"
)

// helpText and helpEpilog wrap the flag list: the header is printed first, the
// examples after the defaults. Both are written with io.WriteString so their
// backticks and quotes need no escaping.
const helpText = `mini - a local bash agent

Usage:
  mini [flags]

With no -p, mini opens its desktop window; -p (or a stdout that is not a
terminal) runs headless on plain stdout with prompts on stdin.

Flags:
`

const helpEpilog = `
Examples:
  mini -t "add a main() guard to a.py, then submit"
  mini -p -y -t "run 'echo hi' then submit" -o /tmp/traj.json
  mini -c mini.yaml -c model.max_tokens=2048
  mini -cpuprofile cpu.pprof -memprofile heap.pprof   # profile the whole run

By default the config comes from the embedded mini.yaml; adding a -c spec
replaces that default file, so pass "-c mini" first when you only want to
override one key.

`

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// optionalFloat distinguishes "flag not given" from "given as 0".
type optionalFloat struct {
	set bool
	v   float64
}

func (o *optionalFloat) String() string {
	if !o.set {
		return ""
	}
	return fmt.Sprint(o.v)
}
func (o *optionalFloat) Set(v string) error {
	_, err := fmt.Sscanf(v, "%g", &o.v)
	if err != nil {
		return err
	}
	o.set = true
	return nil
}

type options struct {
	task            string
	modelName       string
	url             string
	thinking        string
	maxTokens       int
	yolo            bool
	configSpecs     stringList
	output          string
	exitImmediately bool
	printMode       bool
	logFile         string
	cpuProfile      string
	memProfile      string
	showVersion     bool
	resume          resumeFlag
}

// resumeFlag distinguishes `-r` (resume newest, no value) from `-r <id>`
// (resume a specific session) from the flag being absent. IsBoolFlag makes
// the flag package treat a bare -r as valueless instead of eating "-t".
type resumeFlag struct {
	present bool
	value   string
}

func (r *resumeFlag) String() string { return r.value }
func (r *resumeFlag) Set(v string) error {
	r.present = true
	r.value = v
	return nil
}
func (r *resumeFlag) IsBoolFlag() bool { return true }

// resumeArgs rewrites argv so Go's flag package — which has no native
// optional-value flag — always sees the =-form: a bare -r gets the empty
// value (resume the newest session), `-r <id>` binds the id, and neither
// ever swallows the following flag. Session ids never start with "-", so a
// dash after -r means the next flag, not the value.
func resumeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-r" || a == "--r" || a == "-resume" || a == "--resume" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out = append(out, a+"="+args[i+1])
				i++
			} else {
				out = append(out, a+"=")
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func parseFlags(args []string) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("mini", flag.ContinueOnError)
	fs.StringVar(&o.task, "t", "", "task text (omit to be asked)")
	fs.StringVar(&o.task, "task", "", "task text (omit to be asked)")
	fs.StringVar(&o.modelName, "m", "", "model name (falls back to $MINI_MODEL_NAME)")
	fs.StringVar(&o.modelName, "model", "", "model name (falls back to $MINI_MODEL_NAME)")
	fs.StringVar(&o.url, "url", "", "sushi base URL (default "+model.DefaultBaseURL+")")
	fs.StringVar(&o.thinking, "thinking", "", "sushi reasoning effort: off|low|medium|high|xhigh|max")
	fs.IntVar(&o.maxTokens, "max-tokens", 0, "output token budget (0 lets the server decide)")
	fs.BoolVar(&o.yolo, "y", false, "run without confirmation (mode=yolo)")
	fs.BoolVar(&o.yolo, "yolo", false, "run without confirmation (mode=yolo)")
	fs.Var(&o.configSpecs, "c", "config file, name, or key=value (repeatable; replaces the default mini.yaml)")
	fs.StringVar(&o.output, "o", "", "trajectory file")
	fs.StringVar(&o.output, "output", "", "trajectory file")
	fs.BoolVar(&o.exitImmediately, "exit-immediately", false, "don't prompt when the agent is ready; end the run at the submission")
	fs.BoolVar(&o.printMode, "p", false, "headless: plain stdout plus stdin prompts, no GUI window")
	fs.BoolVar(&o.printMode, "print", false, "headless: plain stdout plus stdin prompts, no GUI window")
	fs.StringVar(&o.logFile, "log-file", "", "also write DEBUG logs here")
	fs.StringVar(&o.cpuProfile, "cpuprofile", "", "write a CPU profile of the whole run to FILE")
	fs.StringVar(&o.memProfile, "memprofile", "", "write a heap profile at the end of the run to FILE")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.Var(&o.resume, "r", "resume a session for this repo (optionally: -r <uuid prefix>)")
	fs.Var(&o.resume, "resume", "resume a session for this repo (optionally: -r <uuid prefix>)")
	fs.Usage = func() {
		var b strings.Builder
		fs.SetOutput(&b)
		fs.PrintDefaults()
		fs.SetOutput(os.Stderr)
		_, _ = io.WriteString(os.Stderr, helpText+b.String()+helpEpilog)
	}
	if err := fs.Parse(resumeArgs(args)); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "config" {
		if err := runConfigCommand(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "mini: "+err.Error())
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "sessions" {
		if err := runSessionsCommand(os.Args[2:], os.Stdout); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintln(os.Stderr, "mini: "+err.Error())
			os.Exit(1)
		}
		return
	}
	code := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	o, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "mini:", err)
		return 2
	}
	if o.showVersion {
		fmt.Fprintf(stdout, "mini-go %s\n", agent.Version)
		return 0
	}

	// Profiling brackets the whole run: the CPU profile covers everything
	// from before config resolution to the last deferred act of run, and the
	// heap profile is written as run returns, while everything the run built
	// is still resident. (The subcommands that never reach run - config,
	// sessions - are not profiled: there is no suspect there.) A profile
	// file that cannot be opened aborts the run: a measurement that silently
	// did not happen is worthless.
	if o.cpuProfile != "" {
		f, err := os.Create(o.cpuProfile)
		if err != nil {
			fmt.Fprintln(stderr, "mini: -cpuprofile:", err)
			return 1
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			_ = os.Remove(o.cpuProfile)
			fmt.Fprintln(stderr, "mini: -cpuprofile:", err)
			return 1
		}
		defer func() {
			pprof.StopCPUProfile()
			if err := f.Close(); err != nil {
				fmt.Fprintln(stderr, "mini: closing -cpuprofile:", err)
			}
		}()
	}
	if o.memProfile != "" {
		defer func() {
			f, err := os.Create(o.memProfile)
			if err != nil {
				fmt.Fprintln(stderr, "mini: -memprofile:", err)
				return
			}
			defer f.Close()
			// The heap profile carries both resident and cumulative
			// allocations; debug=0 is the complete protobuf form every pprof
			// reader takes.
			if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
				fmt.Fprintln(stderr, "mini: writing -memprofile:", err)
			}
		}()
	}

	// A stdout that is not a terminal (or an explicit -p) means the headless
	// front end; a terminal means the desktop window is the UI the user chose.
	if !o.printMode && !isTerminal(stdout) {
		o.printMode = true
	}

	// The logger writes through an indirection the GUI can mute: raw writes
	// to the launching terminal are noise nobody reads while the window owns
	// the screen.
	logTarget := ui.NewLogTarget(stderr)
	closeLog := setupLogging(o.logFile, logTarget)
	defer closeLog()

	// The fyne app exists before setup so a startup failure still has a
	// surface: once the window is the chosen front end, errors go there,
	// not to a stderr the user may not be watching.
	var a fyne.App
	if !o.printMode {
		a = app.New()
	}

	r, err := buildRun(o, stdin, stdout, logTarget)
	if err != nil {
		if o.printMode {
			fmt.Fprintln(stderr, "mini: "+err.Error())
			return 1
		}
		// Show the failure in the window instead of quitting one the user
		// never got to see.
		return ui.NewGUI(ui.GUIOptions{App: a, LogWriter: logTarget}).ShowError(err)
	}
	defer r.closeStore()

	printBanner(stdout, o.printMode, r.ConfigName, r.Model)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if o.printMode {
		p := ui.NewPrint(stdout, stdin, r.AgentCfg, r.Store)
		p.Jobs = jobReader(r.Jobs, r.Proc)
		p.Loop = r.Loop
		h, err := ui.New(p, r.AgentCfg, r.Loop)
		if err != nil {
			fmt.Fprintln(stderr, "mini:", err)
			return 1
		}
		r.Loop.Hooks = h
		p.Resume(r.ResumeMsgs, r.ResumeNote)
		if o.task == "" {
			if !p.Interactive() {
				fmt.Fprintln(stderr, "mini: no task given and no terminal to ask for one")
				return 1
			}
			ans, err := p.Ask(ctx, ui.Question{Kind: ui.AskTask, Title: ui.TaskPrompt})
			if err != nil {
				fmt.Fprintln(stderr, "mini:", err)
				return 1
			}
			o.task = ans.Text
		}
		info, err := r.Loop.Run(ctx, o.task)
		return reportEnd(stdout, stderr, r, info, err)
	}

	// `o` is the parsed CLI options here, so the front end's own gets a word.
	guiOpts := sessionGUI(r.HS, r.ConfigName)
	guiOpts.LogWriter, guiOpts.App, guiOpts.Settings = logTarget, a, r.Store
	// What the strip's "+" does: opens another top-level session beside this one.
	guiOpts.Opener = r.openSibling
	// And what /export does: rebuilds one session's trajectory from the store. The
	// project the session belongs to is fixed here rather than passed from the
	// window, so a tab can only ever export its own conversation.
	guiOpts.Exporter = func(id, path string) (string, error) {
		return exportSession(r.Store, r.Sess.Project, id, path)
	}
	g := ui.NewGUI(guiOpts)
	// A stalled window with no explanation is indistinguishable from a crash,
	// so the retry schedule goes into the transcript.
	r.Model.OnRetry = func(attempt, max int, err error, wait time.Duration) {
		g.RetryNotice(attempt, max, err, wait)
	}
	// The window is the UI, not the confirmation logic: without the same
	// Interactive adapter --print gets, the loop never calls OnMessage and
	// the transcript never commits a turn.
	if err := g.InstallHooks(); err != nil {
		return g.ShowError(err)
	}
	// A subagent gets a tab of its own, and its prompts go to that tab: a child the
	// human can see is a child they can answer, which is what decision 15's
	// "bubbling becomes routing" means, and what retires the old reasoning that a
	// child of an unattended parent must be unattended (decision 13, re-ruled). The
	// callback is installed here rather than in host because host still does not
	// know a UI exists; an error refuses the spawn rather than running a child
	// nobody can answer.
	r.Proc.OnChild = func(cs *host.Session) error {
		_, err := g.OpenSession(sessionGUI(cs, r.ConfigName))
		return err
	}
	g.SetSessionID(r.Sess.ID.String())
	g.Resume(r.ResumeMsgs, r.ResumeNote)

	info, err := g.Run(ctx, o.task)
	return reportEnd(stdout, stderr, r, info, err)
}

// reportEnd closes both front ends the same way: how the run ended, where its
// trajectory went if one was asked for, and how to resume it. The trajectory line
// is conditional because nothing writes that file unless the human named a path
// now - "Saved trajectory to ”" for a run that saved nothing is exactly the
// misunderstanding this change removes.
func reportEnd(stdout, stderr io.Writer, r *miniRun, info map[string]any, err error) int {
	reportRun(r.Loop, info, err)
	if r.AgentCfg.OutputPath != "" {
		fmt.Fprintf(stdout, "Saved trajectory to '%s'\n", r.AgentCfg.OutputPath)
	}
	fmt.Fprintf(stderr, "Session %s — resume with mini -r %s\n", r.Sess.ID, r.Sess.ID)
	return exitCode(info, err)
}

// sessionGUI is how any session in this process reaches the window: the top-level
// one at startup, a spawned subagent, and a second repository the human picked
// from the strip all come through here, so no path can wire a session differently
// by accident. Window-level options - app, log target, settings, the opener - are
// the shell's and are set once, where the window is built.
func sessionGUI(hs *host.Session, configName string) ui.GUIOptions {
	m, _ := hs.Model.(*model.Sushi)
	return ui.GUIOptions{
		Cfg: hs.Cfg, Loop: hs.Loop, Model: m, ConfigName: configName,
		Jobs:     jobReader(hs.Jobs, hs.Process()),
		JobEnd:   jobEnder(hs.Jobs),
		Dir:      hs.Workdir,
		Closer:   hs.Quit,
		TabLabel: hs.Name,
	}
}

// jobEnder is the human's half of job_kill: look the run up and stop it through
// the same (*job.Run).Kill the model's tool already uses. Ending waits for the
// unwind, so the window calls it from a goroutine.
func jobEnder(m *job.Manager) func(string) error {
	if m == nil {
		return nil
	}
	return func(id string) error {
		run, err := m.Get(id)
		if err != nil {
			return err
		}
		return run.Kill()
	}
}

// openSibling starts another top-level session in the directory the human chose,
// from the configuration this one was assembled with, and blocks doing it: the
// session row is written and the context window discovered before anything can be
// drawn. The window calls it from a goroutine for exactly that reason.
func (r *miniRun) openSibling(dir string) (ui.GUIOptions, error) {
	workdir, project := projectOf(dir)
	hs, err := r.HS.OpenSibling(workdir, project)
	if err != nil {
		return ui.GUIOptions{}, err
	}
	return sessionGUI(hs, r.ConfigName), nil
}

// The store's settings table is the front ends' sticky state (ui/prefs.go):
// one row per key of interface state that is nobody's session.
var _ ui.Settings = (*session.Store)(nil)

// miniRun is a fully configured run, ready for whichever frontend owns it:
// everything `mini` does before it opens a UI (config, model, environment,
// loop, session, resume) and no terminal interaction beyond the first-time
// setup the shared call sequence already performs.
type miniRun struct {
	O        *options
	AgentCfg *agent.Config
	Model    *model.Sushi
	Env      *env.Local
	Loop     *agent.Loop
	Sess     *session.Session
	// Store is the same handle Sess writes through, handed to the front ends
	// for the store's global settings table - the interface state that is not
	// part of any session (ui/prefs.go).
	Store *session.Store
	// Jobs is the registry of runs that outlive their tool call: the sweep at
	// open, the handoff from a promoted foreground command, the jobs and
	// job_kill tools, and the reap on exit.
	Jobs *job.Manager
	// Proc is the process-level owner this session was opened by, and the GUI's
	// handle on it: `Proc.OnChild` is what opens a tab when the model spawns a
	// subagent (§7 step 7.4). Holding it here is what lets a second session be
	// opened without one CLI invocation per conversation.
	Proc *host.Process
	// HS is this run's session inside Proc: what a second session is cloned from
	// and how the tab's × stops this one.
	HS         *host.Session
	ResumeMsgs []model.Message
	ResumeNote string
	ConfigName string
	LogTarget  *ui.LogTarget
	closeStore func()
}

// buildRun prepares a run exactly as mini always has, in the same order,
// printing nothing: every failure comes back unprefixed so the frontend that
// owns the terminal prints "mini: "+err like before.
// buildRun prepares a run exactly as mini always has, in the same order,
// printing nothing: every failure comes back unprefixed so the frontend that
// owns the terminal prints "mini: "+err like before.
//
// What changed is that the assembly itself belongs to host now. This function is
// the CLI half - global environment, first-run setup, flag-derived config, the
// store, project identity, resume resolution - and one call to
// `host.Process.Open` for the six objects that used to be built here. That split
// is the prerequisite (§5): a subagent has no flags and no terminal, so anything
// it needs to become a session must live below this function.
func buildRun(o *options, stdin *os.File, stdout io.Writer, logTarget *ui.LogTarget) (r *miniRun, err error) {
	loadGlobalEnv()

	merged, configName, err := buildConfig(o)
	if err != nil {
		return nil, err
	}

	// First-run setup is gated on the configuration mini is about to run with, so
	// it has to come after the merge and not before it. It used to be a test on two
	// environment variables and the -m flag evaluated before any config was read,
	// which meant a saved config that specifies model.model_name and model.base_url
	// satisfied nothing, and the wizard asked for a model and provider on every
	// start - the very config it was supposed to use.
	if needsFirstTimeSetup(merged) {
		if err := runFirstTimeSetup(stdin, stdout); err != nil {
			return nil, err
		}
		// The wizard exports what it wrote into this process, so re-merge: the
		// answer typed a moment ago has to reach this run, not only the next one.
		if merged, configName, err = buildConfig(o); err != nil {
			return nil, err
		}
	}
	modelCfg := config.Sub(merged, "model")
	envCfg := config.Sub(merged, "environment")

	// A store that cannot be opened is fatal: a transcript source of truth
	// that silently stopped recording is worse than a run that refuses to
	// start.
	if _, err := config.EnsureStateDir(); err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}
	store, err := session.Open(config.SessionDB(), config.ToolCallResultsDir())
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}

	// The bound on in-flight chat completions is resolved before any session can
	// ask a question, from the stored preference or the config or the default
	// (§5.7). It needs the store to read the stored preference, so it goes
	// immediately after opening it and before host.New, whose sweeps are the
	// other process-level startup step.
	applyConcurrency(store, modelCfg)

	proc := host.New(store, context.Background())

	// Project identity: the git toplevel, else the canonical working
	// directory, so `mini sessions` and `mini -r` find the runs started in
	// this checkout from anywhere inside it. This stays with the CLI on
	// purpose - it is a rule about checkouts and symlinks, not a property of
	// running a conversation - so the host is handed the answer rather than
	// the means of computing it.
	startDir := config.String(envCfg, "cwd", "")
	if startDir == "" {
		startDir, _ = os.Getwd()
	}
	workdir, project := projectOf(startDir)

	spec := host.Spec{Config: merged, Workdir: workdir, Project: project}
	if o.resume.present {
		found, err := findResumable(store, project, o.resume.value)
		if err != nil {
			_ = store.Close()
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errors.New("no session for this directory")
			}
			return nil, fmt.Errorf("session store: %w", err)
		}
		spec.Resume = found
	}

	hs, err := proc.Open(spec)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	sushi, ok := hs.Model.(*model.Sushi)
	if !ok {
		_ = store.Close()
		return nil, errors.New("mini always opens a sushi client; the model was replaced under it")
	}
	// A request that had to queue says so, for the same reason a retry does: a
	// stalled session with no explanation is indistinguishable from a crashed
	// one. The GUI turns this into a line in the transcript in step 7; for now
	// it is a log record, which is at least a true one.
	sushi.OnGateWait = func(w model.GateWait) {
		slog.Info("waiting for a model slot",
			"waited", w.Waited.Round(time.Millisecond).String(),
			"busy", w.Busy, "waiting", w.Waiting, "limit", w.Limit)
	}
	for _, wm := range hs.SweepWarn {
		slog.Warn("job left running by a previous run", "warning", wm)
	}

	return &miniRun{
		O: o, AgentCfg: hs.Cfg, Model: sushi, Env: hs.Env, Loop: hs.Loop,
		Sess: hs.Sess, Store: store, Jobs: hs.Jobs, Proc: proc, HS: hs,
		ResumeMsgs: hs.ResumeMsgs, ResumeNote: hs.ResumeNote,
		ConfigName: configName, LogTarget: logTarget,
		// Reaping rides the one cleanup path both front ends already share, and
		// runs before the database closes so the ended states are recorded: a
		// job outlives a turn, never a session. Process.Close ends children
		// before parents (§9.2), which is the order that cannot leave a running
		// process whose session is already closed.
		closeStore: func() {
			_ = proc.Close()
			_ = store.Close()
		},
	}, nil
}

func reportRun(l *agent.Loop, info map[string]any, err error) {
	if err != nil {
		var in *model.Interrupt
		if errors.As(err, &in) {
			slog.Error("run interrupted", "kind", in.Kind)
			return
		}
		slog.Error("run failed", "err", err)
		return
	}
	status, _ := info["exit_status"].(string)
	slog.Debug("run finished", "exit_status", status, "messages", len(l.Messages))
}

func exitCode(info map[string]any, err error) int {
	if err != nil {
		return 1
	}
	if s, _ := info["exit_status"].(string); s == model.KindSubmitted {
		return 0
	}
	return 1
}

// buildConfig resolves the -c specs left to right and merges the CLI flags on
// top as UNSET-padded overrides, like run/mini.py.
func buildConfig(o *options) (map[string]any, string, error) {
	specs := o.configSpecs
	if len(specs) == 0 {
		specs = stringList{defaultConfigSpec()}
	}
	var maps []map[string]any
	name := ""
	for _, spec := range specs {
		m, err := config.FromSpec(spec)
		if err != nil {
			return nil, "", err
		}
		if name == "" && !strings.Contains(spec, "=") {
			name = spec
		}
		maps = append(maps, m)
	}
	if name == "" {
		name = specs[0]
	}
	overrides := map[string]any{
		"run": map[string]any{"task": orUnset(o.task)},
		"agent": map[string]any{
			"mode":         strFn("yolo", o.yolo),
			"confirm_exit": boolFn(false, o.exitImmediately),
			"output_path":  orUnset(o.output),
		},
		"model": map[string]any{
			"model_name":       orUnset(o.modelName),
			"base_url":         orUnset(o.url),
			"reasoning_effort": orUnset(o.thinking),
			"max_tokens":       intOrUnset(o.maxTokens),
		},
	}
	return config.Merge(append(maps, overrides)...), name, nil
}

func orUnset(s string) any {
	if s == "" {
		return config.Unset
	}
	return s
}

func floatOrUnset(o optionalFloat) any {
	if !o.set {
		return config.Unset
	}
	return o.v
}

func intOrUnset(i int) any {
	if i == 0 {
		return config.Unset
	}
	return i
}

func strFn(v string, on bool) any {
	if !on {
		return config.Unset
	}
	return v
}

func boolFn(v bool, on bool) any {
	if !on {
		return config.Unset
	}
	return v
}

// defaultConfigSpec honours MINI_MINI_CONFIG_PATH (the file or name to load)
// like MSWEA_MINI_CONFIG_PATH.
func defaultConfigSpec() string {
	if v := os.Getenv("MINI_CONFIG_PATH"); v != "" {
		return strings.TrimSuffix(v, ".yaml")
	}
	return "mini"
}

// settingMaxConcurrentRequests is the stored bound on in-flight chat
// completions - machine state rather than run configuration, in exactly the
// sense that makes the thinking toggle live here instead of in the config file
// (ui/prefs.go:12-20): it is a preference about this box, it should still be
// there next launch, and `mini -p` must not rewrite anybody's config to keep it.
//
// The key deliberately has NO entry in the embedded mini.yaml. Precedence below
// treats a config value as an explicit statement for this run, so a shipped
// default sitting in the file would outrank the stored preference forever and
// the setting would be a write-only column.
const settingMaxConcurrentRequests = "max_concurrent_requests"

// applyConcurrency resolves the process-wide bound on in-flight chat completion
// requests and installs it. It is process-wide rather than per-session because
// the resource it protects - one GPU behind one sushi - is shared by every
// session in the process, and a bound a subagent can step around by building
// its own client is not a bound (docs/parallel-sessions.md §5.7).
//
// The chain is ordered by specificity, which is how the context window resolves
// in model.DiscoverContextWindow, and it logs its answer with the same
// "source" field so a reader can tell which of the three moved.
func applyConcurrency(store *session.Store, modelCfg map[string]any) {
	limit, source := model.DefaultMaxConcurrentRequests, "default"
	if stored, ok, err := store.Setting(settingMaxConcurrentRequests); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(stored)); err == nil && n > 0 {
			limit, source = n, "setting"
		}
	}
	// A config value - from the file or a -c override - outranks the stored
	// preference: a script that asks for one request at a time means it for this
	// run, and that is what the config file is for.
	if n := config.Int(modelCfg, "max_concurrent_requests", 0); n > 0 {
		limit, source = n, "config"
	}
	model.DefaultGate.SetLimit(limit)
	slog.Info("model concurrency", "limit", limit, "source", source)
}

// projectOf canonicalises dir and walks up to the nearest directory containing
// a .git entry (a worktree's .git file counts). It returns (workdir, workdir)
// when nothing does, "" workdir only if it cannot be made absolute. No git
// exec, no hashing: harness hashed a raw cwd, so symlinked checkouts and
// /var vs /private/var forked separate histories.
func projectOf(dir string) (workdir, project string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", ""
	}
	workdir = abs
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		workdir = real
	}
	for p := workdir; ; {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return workdir, p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return workdir, workdir
		}
		p = parent
	}
}

// findResumable resolves -r: a value is a uuid prefix inside project; a bare
// -r takes the project's most recently updated session.
func findResumable(store *session.Store, project, prefix string) (*session.Session, error) {
	if prefix != "" {
		return store.Find(project, prefix)
	}
	rows, err := store.List(project, false, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return store.Get(rows[0].ID)
}

// globalEnvOnce guards loadGlobalEnv because writing the process environment is
// a thing that happens once per process, not once per session. It used to be
// called from buildRun, which is only ever run once, so the distinction was
// academic; with assembly callable N times it becomes a race between one
// session's startup and another session's shells snapshotting os.Environ()
// (env/local.go:119), which is exactly the kind of cross-talk a parallel session
// must not have.
var globalEnvOnce sync.Once

// loadGlobalEnv applies the global .env without overriding real environment
// variables, matching dotenv's default (override=False).
func loadGlobalEnv() {
	globalEnvOnce.Do(func() { loadGlobalEnvNow() })
}

func loadGlobalEnvNow() {
	values, err := config.LoadEnv(config.GlobalConfigFile())
	if err != nil {
		slog.Warn("could not read the global config", "path", config.GlobalConfigFile(), "err", err)
		return
	}
	for k, v := range values {
		if _, ok := os.LookupEnv(k); !ok {
			_ = os.Setenv(k, v)
		}
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func printBanner(w io.Writer, printMode bool, configName string, m *model.Sushi) {
	if os.Getenv("MINI_SILENT_STARTUP") != "" {
		return
	}
	out := io.Discard
	// The banner goes to stderr so --print output stays pipeable; the window
	// owns the screen entirely and gets its header in the transcript.
	if !printMode {
		out = w
	}
	fmt.Fprintf(out, "mini-go %s . config %s . model %s @ %s . ctx %s\n",
		agent.Version, configName, m.Model, m.BaseURL, m.ContextWindowNote())
}

func setupLogging(logFile string, stderr io.Writer) func() {
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	handler := slog.Handler(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	var file *os.File
	if logFile != "" {
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err == nil {
			f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				file = f
				handler = &splitHandler{stderr: handler, file: slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})}
				level.Set(slog.LevelDebug)
			}
		}
	}
	slog.SetDefault(slog.New(handler))
	return func() {
		if file != nil {
			_ = file.Close()
		}
	}
}

// splitHandler fans records out to a console handler (its own level) and a file
// handler (DEBUG), mirroring utils/log.py's console plus file handlers.
type splitHandler struct {
	stderr slog.Handler
	file   slog.Handler
}

func (h *splitHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.stderr.Enabled(ctx, l) || h.file.Enabled(ctx, l)
}

func (h *splitHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.stderr.Enabled(ctx, r.Level) {
		if err := h.stderr.Handle(ctx, r); err != nil {
			return err
		}
	}
	if h.file.Enabled(ctx, r.Level) {
		return h.file.Handle(ctx, r)
	}
	return nil
}

func (h *splitHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &splitHandler{h.stderr.WithAttrs(a), h.file.WithAttrs(a)}
}

func (h *splitHandler) WithGroup(g string) slog.Handler {
	return &splitHandler{h.stderr.WithGroup(g), h.file.WithGroup(g)}
}

// needsFirstTimeSetup is the whole gate: mini asks for a model and a server only
// when the configuration it is about to run cannot answer that question itself.
// It asks it the way the model client will - the merged section first,
// $MINI_MODEL_NAME as the fallback - so a saved config, an environment variable and
// -m each count as configured, and none of them can drift from what actually gets
// used, because this is the same constructor that produces the client.
//
// MINI_CONFIGURED stays as an answer of its own: the wizard lets every field be left
// blank on purpose, so somebody who went through it and typed nothing has still said
// "stop asking", which a missing model name must not overwrite.
func needsFirstTimeSetup(merged map[string]any) bool {
	if os.Getenv("MINI_CONFIGURED") != "" {
		return false
	}
	return model.NewSushi(config.Sub(merged, "model")).Model == ""
}

// runFirstTimeSetup asks the first-run questions, and only where a human can answer
// them: with no terminal, mini names the command that configures it instead of
// blocking on a prompt nobody will ever see. Whether anything needs asking is
// needsFirstTimeSetup's question - and that one can only be asked once the config
// has been merged.
func runFirstTimeSetup(stdin *os.File, stdout io.Writer) error {
	fi, err := stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintln(os.Stderr, "mini: no model configured. Run `mini config setup`, or pass -m/--model.")
		return nil
	}
	return setupWizard(stdin, stdout, false)
}

func setupWizard(stdin *os.File, stdout io.Writer, force bool) error {
	path := config.GlobalConfigFile()
	if _, err := config.EnsureConfigDir(); err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	fmt.Fprintf(stdout, `To get started, mini needs a default model and the URL of your sushi server.

mini talks to an OpenAI-compatible /v1/chat/completions endpoint with native
bash tool calls. Start one with:  sushi serve --port 1234 --model <pack>

The config is written to %s
(You can leave any setting blank to skip it.)

`, path)
	if !force {
		defer os.Setenv("MINI_CONFIGURED", "true")
	}
	modelName, err := prompt(in, stdout, "Enter your default model", os.Getenv("MINI_MODEL_NAME"))
	if err != nil {
		return err
	}
	if modelName != "" {
		if err := config.SetEnvValue(path, "MINI_MODEL_NAME", modelName); err != nil {
			return err
		}
		_ = os.Setenv("MINI_MODEL_NAME", modelName)
	}
	url, err := prompt(in, stdout, "Enter the sushi base URL", model.DefaultBaseURL)
	if err != nil {
		return err
	}
	if url != "" && url != model.DefaultBaseURL {
		if err := config.SetEnvValue(path, "MINI_BASE_URL", url); err != nil {
			return err
		}
		_ = os.Setenv("MINI_BASE_URL", url)
	}
	if err := config.SetEnvValue(path, "MINI_CONFIGURED", "true"); err != nil {
		return err
	}
	_ = os.Setenv("MINI_CONFIGURED", "true")
	fmt.Fprintf(stdout, "\nConfig saved. Revisit it any time with `mini config setup`, or edit %s\n", path)
	return nil
}

func prompt(in *bufio.Reader, out io.Writer, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}
