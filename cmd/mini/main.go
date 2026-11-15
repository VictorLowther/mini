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
	"strings"
	"syscall"
	"time"
	"uuid"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
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
	costLimit       optionalFloat
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
	fs.Var(&o.costLimit, "l", "cost limit; 0 disables (needs model.pricing)")
	fs.Var(&o.costLimit, "cost-limit", "cost limit; 0 disables (needs model.pricing)")
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
		reportRun(r.Loop, info, err)
		fmt.Fprintf(stdout, "Saved trajectory to '%s'\n", r.AgentCfg.OutputPath)
		fmt.Fprintf(stderr, "Session %s — resume with mini -r %s\n", r.Sess.ID, r.Sess.ID)
		return exitCode(info, err)
	}

	g := ui.NewGUI(ui.GUIOptions{
		Cfg: r.AgentCfg, Loop: r.Loop, Model: r.Model,
		ConfigName: r.ConfigName, LogWriter: logTarget, App: a,
		Settings: r.Store,
	})
	// A stalled window with no explanation is indistinguishable from a crash,
	// so the retry schedule goes into the transcript.
	r.Model.OnRetry = func(attempt, max int, err error, wait time.Duration) {
		g.RetryNotice(attempt, max, err, wait)
	}
	// The window is the UI, not the confirmation logic: without the same
	// Interactive adapter --print gets, the loop never calls OnMessage and
	// the transcript never commits a turn.
	h, err := ui.New(g, r.AgentCfg, r.Loop)
	if err != nil {
		return g.ShowError(err)
	}
	r.Loop.Hooks = h
	g.SetSessionID(r.Sess.ID.String())
	g.Resume(r.ResumeMsgs, r.ResumeNote)

	info, err := g.Run(ctx, o.task)
	reportRun(r.Loop, info, err)
	fmt.Fprintf(stdout, "Saved trajectory to '%s'\n", r.AgentCfg.OutputPath)
	fmt.Fprintf(stderr, "Session %s — resume with mini -r %s\n", r.Sess.ID, r.Sess.ID)
	return exitCode(info, err)
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
	// Jobs is the registry of runs that outlive their tool call. Nothing starts
	// one yet - the bash parameters and the job_* tools are the next steps of
	// docs/jobs-research.md §4's build order - but the sweep at open and the
	// reap on exit are already wired, which is what keeps the feature safe to
	// ship half-finished.
	Jobs       *job.Manager
	ResumeMsgs []model.Message
	ResumeNote string
	ConfigName string
	LogTarget  *ui.LogTarget
	closeStore func()
}

// buildRun prepares a run exactly as mini always has, in the same order,
// printing nothing: every failure comes back unprefixed so the frontend that
// owns the terminal prints "mini: "+err like before.
func buildRun(o *options, stdin *os.File, stdout io.Writer, logTarget *ui.LogTarget) (r *miniRun, err error) {
	loadGlobalEnv()
	if err := maybeRunFirstTimeSetup(stdin, stdout, o); err != nil {
		return nil, err
	}

	merged, configName, err := buildConfig(o)
	if err != nil {
		return nil, err
	}

	agentCfg := agent.ConfigFromMap(config.Sub(merged, "agent"))
	modelCfg := config.Sub(merged, "model")
	envCfg := config.Sub(merged, "environment")

	if agentCfg.OutputPath == "" {
		agentCfg.OutputPath = defaultOutputPath()
	}
	if agentCfg.SystemTemplate == "" || agentCfg.InstanceTemplate == "" {
		return nil, errors.New("the agent config needs system_template and instance_template")
	}

	m := model.NewSushi(modelCfg)
	if m.Model == "" {
		return nil, errors.New("no model set. Pass -m NAME or run `mini config setup`.")
	}

	// The window is the server's to know: ask it (GET /v1/models) before the
	// loop and the front ends are built, so the compaction trigger, the
	// footer's ctx%% and the trajectory all run against the extent this session
	// actually has. agent.context_window stays the fallback for a server that
	// will not say, then model.DefaultContextWindow. It is a startup question
	// with a known-good default answer, so a silent or unreachable server
	// costs one short request and never a run.
	agentCfg.ContextWindow, _ = m.DiscoverContextWindow(context.Background(), agentCfg.ContextWindow)

	e := newEnvironment(envCfg)

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
	// A failed setup must not leave the database locked for the next attempt
	// in this process; a successful one hands the handle to the caller.
	defer func() {
		if err != nil {
			_ = store.Close()
		}
	}()

	// Project identity: the git toplevel, else the canonical working
	// directory, so `mini sessions` and `mini -r` find the runs started in
	// this checkout from anywhere inside it.
	startDir := e.CWD
	if startDir == "" {
		startDir, _ = os.Getwd()
	}
	workdir, project := projectOf(startDir)

	l := &agent.Loop{Cfg: agentCfg, Model: m, Env: e, Logger: slog.Default()}

	// Fresh runs get a row (Title backfilled by the store when the task
	// message lands, which is what titles the GUI's ask-for-a-task flow
	// too); -r runs load the stored transcript and continue it.
	var (
		sess       *session.Session
		resumeMsgs []model.Message
		resumeNote string
	)
	if o.resume.present {
		found, err := findResumable(store, project, o.resume.value)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errors.New("no session for this directory")
			}
			return nil, fmt.Errorf("session store: %w", err)
		}
		msgs, err := found.Transcript()
		if err != nil {
			return nil, fmt.Errorf("session store: %w", err)
		}
		sess = found
		resumeMsgs = msgs
		resumeNote = fmt.Sprintf("resumed session %s (%d turns)", sess.ID, len(msgs))
		l.Cost, l.NCalls = sess.Cost, sess.NCalls
		l.Resume(msgs)
	} else {
		created, err := store.Create(session.Row{ID: uuid.NewV7(), Workdir: workdir, Project: project, ModelName: m.Model})
		if err != nil {
			return nil, fmt.Errorf("session store: %w", err)
		}
		sess = created
	}
	l.Recorder = sess

	// Job registry, one per run. It interprets backgrounded commands through the
	// same env.Local that answers the foreground bash tool, so a job is the same
	// shell with a longer leash rather than a second dialect the model cannot
	// see coming. The sweep runs before anything can start: a row still marked
	// 'running' is a claim about a process this mini does not own, so it becomes
	// orphaned and the human is told, which is the honest version of "still
	// running" (docs/jobs-research.md §4).
	jobs := &job.Manager{Session: sess, Local: e, Logger: slog.Default()}
	l.Jobs = jobs
	// One number for the foreground wait, from the same setting that governs
	// the environment's own deadline.
	l.CommandBudget = e.Timeout
	for _, w := range jobs.Sweep() {
		slog.Warn("job left running by a previous run", "warning", w)
	}

	return &miniRun{
		O: o, AgentCfg: agentCfg, Model: m, Env: e, Loop: l, Sess: sess, Store: store,
		Jobs:       jobs,
		ResumeMsgs: resumeMsgs, ResumeNote: resumeNote,
		ConfigName: configName, LogTarget: logTarget,
		// Reaping rides the one cleanup path both front ends already share, and
		// runs before the database closes so the ended states are recorded: a
		// job outlives a turn, never a session.
		closeStore: func() {
			if n := jobs.KillAll(); n > 0 {
				slog.Info("reaped live jobs on exit", "count", n)
			}
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
			"cost_limit":   floatOrUnset(o.costLimit),
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

func defaultOutputPath() string {
	dir, err := config.EnsureConfigDir()
	if err != nil {
		return "last_mini_run.traj.json"
	}
	return filepath.Join(dir, "last_mini_run.traj.json")
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

// newEnvironment builds the local environment from the `environment:` section.
func newEnvironment(cfg map[string]any) *env.Local {
	l := &env.Local{
		CWD:     config.String(cfg, "cwd", ""),
		Env:     map[string]string{},
		Timeout: time.Duration(config.Int(cfg, "timeout", int(env.DefaultTimeout.Seconds()))) * time.Second,
	}
	for k, v := range config.Sub(cfg, "env") {
		if s, ok := v.(string); ok {
			l.Env[k] = s
		} else {
			l.Env[k] = fmt.Sprint(v)
		}
	}
	return l
}

// loadGlobalEnv applies the global .env without overriding real environment
// variables, matching dotenv's default (override=False).
func loadGlobalEnv() {
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

// maybeRunFirstTimeSetup is configure_if_first_time(): the wizard runs once, and
// only when there is a terminal to talk to.
func maybeRunFirstTimeSetup(stdin *os.File, stdout io.Writer, o *options) error {
	if os.Getenv("MINI_CONFIGURED") != "" || os.Getenv("MINI_MODEL_NAME") != "" || o.modelName != "" {
		return nil
	}
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
