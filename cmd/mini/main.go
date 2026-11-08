// Command mini is the local bash agent: the Go port of mini-swe-agent's `mini`
// executable, talking to a sushi server.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/ui"
)

// helpText and helpEpilog wrap the flag list: the header is printed first, the
// examples after the defaults. Both are written with io.WriteString so their
// backticks and quotes need no escaping.
const helpText = `mini - a local bash agent

Usage:
  mini [flags]

Flags:
`

const helpEpilog = `
Examples:
  mini -t "add a main() guard to a.py, then submit"
  mini -p -y -t "run 'echo hi' then submit" -o /tmp/traj.json
  mini -c mini.yaml -c model.max_tokens=2048

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
	showVersion     bool
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
	fs.BoolVar(&o.exitImmediately, "exit-immediately", false, "don't prompt when the agent wants to finish")
	fs.BoolVar(&o.printMode, "p", false, "no TUI: plain stdout plus stdin prompts")
	fs.BoolVar(&o.printMode, "print", false, "no TUI: plain stdout plus stdin prompts")
	fs.StringVar(&o.logFile, "log-file", "", "also write DEBUG logs here")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		var b strings.Builder
		fs.SetOutput(&b)
		fs.PrintDefaults()
		fs.SetOutput(os.Stderr)
		_, _ = io.WriteString(os.Stderr, helpText+b.String()+helpEpilog)
	}
	if err := fs.Parse(args); err != nil {
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

	closeLog := setupLogging(o.logFile, stderr)
	defer closeLog()

	loadGlobalEnv()
	if err := maybeRunFirstTimeSetup(stdin, stdout, o); err != nil {
		fmt.Fprintln(stderr, "mini:", err)
		return 1
	}

	merged, configName, err := buildConfig(o)
	if err != nil {
		fmt.Fprintln(stderr, "mini:", err)
		return 1
	}

	agentCfg := agent.ConfigFromMap(config.Sub(merged, "agent"))
	modelCfg := config.Sub(merged, "model")
	envCfg := config.Sub(merged, "environment")

	if agentCfg.OutputPath == "" {
		agentCfg.OutputPath = defaultOutputPath()
	}
	if agentCfg.SystemTemplate == "" || agentCfg.InstanceTemplate == "" {
		fmt.Fprintln(stderr, "mini: the agent config needs system_template and instance_template")
		return 1
	}

	m := model.NewSushi(modelCfg)
	if m.Model == "" {
		fmt.Fprintln(stderr, "mini: no model set. Pass -m NAME or run `mini config setup`.")
		return 1
	}
	e := newEnvironment(envCfg)

	if !o.printMode && !isTerminal(stdout) {
		o.printMode = true
	}
	printBanner(stdout, o.printMode, configName, m)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l := &agent.Loop{Cfg: agentCfg, Model: m, Env: e, Logger: slog.Default()}

	if o.printMode {
		p := ui.NewPrint(stdout, stdin, agentCfg)
		p.Loop = l
		h, err := ui.New(p, agentCfg, l)
		if err != nil {
			fmt.Fprintln(stderr, "mini:", err)
			return 1
		}
		l.Hooks = h
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
		info, err := l.Run(ctx, o.task)
		reportRun(l, info, err)
		fmt.Fprintf(stdout, "Saved trajectory to '%s'\n", agentCfg.OutputPath)
		return exitCode(info, err)
	}

	t, err := ui.NewTUI(ui.TUIOptions{Cfg: agentCfg, Loop: l, Model: m, ConfigName: configName})
	if err == nil {
		// A stalled pane with no explanation is indistinguishable from a crash,
		// so the retry schedule goes into the scrollback.
		m.OnRetry = func(attempt, max int, err error, wait time.Duration) {
			t.RetryNotice(attempt, max, err, wait)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "mini: could not start the TUI:", err)
		return 1
	}
	// The TUI needs the same interactive hooks as --print: it is the UI, not
	// the confirmation logic.
	h, err := ui.New(t, agentCfg, l)
	if err != nil {
		fmt.Fprintln(stderr, "mini:", err)
		return 1
	}
	l.Hooks = h
	info, err := t.Run(ctx, o.task)
	reportRun(l, info, err)
	fmt.Fprintf(stderr, "Saved trajectory to '%s'\n", agentCfg.OutputPath)
	return exitCode(info, err)
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
	// The banner goes to stderr so --print output stays pipeable; the TUI owns
	// the screen entirely.
	if !printMode {
		out = w
	}
	fmt.Fprintf(out, "mini-go %s . config %s . model %s @ %s\n", agent.Version, configName, m.Model, m.BaseURL)
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
