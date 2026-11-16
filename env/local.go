package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// DefaultTimeout matches LocalEnvironmentConfig.timeout.
const DefaultTimeout = 30 * time.Second

// Local runs commands on the machine that started the agent. Like the Python
// LocalEnvironment it is stateless: every action is a fresh shell, so `cd` and
// `export` do not persist.
type Local struct {
	// CWD is the default working directory; empty means the agent's cwd.
	CWD string
	// Env overrides the inherited process environment (the config's env block).
	Env map[string]string
	// Logger receives the interpreter's own failures. Nil is slog.Default().
	// It exists for one reason - the interp.Run recover below - so a panic
	// inside the third-party interpreter is reported with its stack instead of
	// arriving at the model as a bare command failure nobody can act on.
	Logger *slog.Logger
	// Timeout bounds one command. Zero means DefaultTimeout, and a negative
	// value means no deadline at all: only the caller's context can end the
	// run. That is what a background job needs
	// (docs/jobs-research.md §4 decision 2 - the model's timeouts are its own
	// and there is no upper bound), and it is why the job manager drives this
	// same Execute rather than a second runner of its own.
	Timeout time.Duration
}

func (l *Local) timeout() time.Duration {
	if l.Timeout == 0 {
		return DefaultTimeout
	}
	return l.Timeout
}

// runContext applies the command's budget and hands back its cancel and the
// number of seconds it settled on, so the timeout message can name the deadline
// that actually expired instead of the environment default.
//
// A deadline already carried by ctx wins, which is how a per-call budget reaches
// an Environment whose Execute signature has no timeout field: the caller wraps
// the context. That keeps the interface something a docker or singularity
// environment can imitate, and makes the precedence one rule instead of a second
// budget to keep in sync. l.Timeout is the default for callers with no opinion,
// and negative means no default at all, which is what a background job needs.
func (l *Local) runContext(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	if deadline, ok := ctx.Deadline(); ok {
		return ctx, func() {}, time.Until(deadline)
	}
	d := l.timeout()
	if d < 0 {
		// No deadline: cctx.Err() can never report DeadlineExceeded, so the
		// timeout branch in Execute stays unreachable without a fake timer to
		// prove it.
		return ctx, func() {}, 0
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	return cctx, cancel, d
}

// logger is the interpreter's own sink; nil means the process default.
func (l *Local) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// recoveredPanic turns a panic from inside mvdan.cc/sh into the failed Result
// the caller expects, after saying so loudly: level error, the offending
// command, and the stack, because the stack is the only thing that distinguishes
// "an input shape we can tell the model to avoid" from "a bug worth filing
// upstream" - and without it logged, both look identical to a command that
// simply did not work.
//
// The command text is logged in full rather than truncated. It is the repro, so
// a shortened copy is not actionable, and a panic path is not where a size limit
// earns anything.
func (l *Local) recoveredPanic(rec any, command, dir string) Result {
	l.logger().Error("interpreter panic: recovered so the run survives, but this is a bug in mvdan.cc/sh or an input shape worth working around",
		"panic", fmt.Sprint(rec),
		"command", command,
		"cwd", dir,
		"stack", string(debug.Stack()))
	return Result{
		ReturnCode:    -1,
		ExceptionInfo: fmt.Sprintf("An error occurred while executing the command: command execution panic: %v", rec),
		Extra:         map[string]any{"exception_type": "Panic", "exception": fmt.Sprint(rec)},
	}
}

// Execute runs one command and returns its Result. The only Go error it
// returns is the caller's context cancellation, which the agent turns into a
// user interruption; timeouts come back as a partial-output Result.
func (l *Local) Execute(ctx context.Context, command, cwd string, out io.Writer) (result Result, err error) {
	dir := cwd
	if dir == "" {
		dir = l.CWD
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if out == nil {
		out = io.Discard
	}

	// The budget comes from l.Timeout; a caller that wants a different one per
	// call wraps the context itself, which is how the job manager runs one
	// command with no deadline at all (Timeout < 0).
	cctx, cancel, budget := l.runContext(ctx)
	defer cancel()

	file, perr := syntax.NewParser().Parse(strings.NewReader(command), "")
	if perr != nil {
		return Result{
			ReturnCode:    -1,
			CWD:           dir,
			ExceptionInfo: fmt.Sprintf("An error occurred while executing the command: could not parse command: %v", perr),
			Extra:         map[string]any{"exception_type": "ParserError", "exception": perr.Error()},
		}, nil
	}

	devnull, derr := os.Open(os.DevNull)
	if derr == nil {
		defer devnull.Close()
	}
	runner, rerr := interp.New(
		// /dev/null as stdin: immediate EOF for children and for `read`,
		// without mvdan's pipe-copy goroutine.
		//
		// `out, out` is deliberate, not a typo to fix: interp assigns the same
		// writer to cmd.Stdout and cmd.Stderr (interp/handler.go:155-156),
		// and os/exec returns an *os.File writer unchanged (exec.go:591-603),
		// so fd 1 and fd 2 are dup'd from one file description and share one
		// offset — the kernel's own ordering, byte-identical to
		// `cmd > file 2>&1`, with no copying goroutine and no cap. Wrapping
		// the file in a counting writer would destroy that property, which is
		// why output_bytes comes from Stat.
		interp.StdIO(orNil(devnull, derr), out, out),
		interp.Interactive(false),
		interp.Env(expand.ListEnviron(append(os.Environ(), l.envList()...)...)),
		interp.Dir(dir),
		// The singular, deprecated option on purpose: interp.ExecHandlers
		// always appends DefaultExecHandler, which signals only the child PID
		// and would orphan grandchildren.
		interp.ExecHandler(processGroupExecHandler(KillGrace)),
	)
	if rerr != nil {
		return Result{ReturnCode: -1, ExceptionInfo: fmt.Sprintf("An error occurred while executing the command: %v", rerr)}, nil
	}

	// interp.Run can panic on exotic input; a panic must not kill the agent.
	// Recovering here is deliberate and it is the only recover in mini's
	// runtime path (docs/parallel-sessions.md §9.12): every other panic is a
	// bug in mini and is left loud and fatal on purpose. This one is not our
	// bug, so it is caught - but "caught" must not mean "quiet", or a repeatable
	// interpreter fault reaches the model as an unexplained exit -1 and nobody
	// ever learns there is an upstream issue to file or a syntax to work around.
	// Hence recoveredPanic, which logs the stack at error level before building
	// the Result.
	defer func() {
		if rec := recover(); rec != nil {
			result = l.recoveredPanic(rec, command, dir)
			err = nil
		}
	}()

	runErr := runner.Run(cctx, file)

	switch {
	case ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		// A cancellation somebody asked for - the user, or the shutdown signal -
		// stopped the run mid-command: an interruption, not an observation.
		// Testing Canceled-ness rather than "ctx.Err() != nil" matters now that a
		// caller may carry the deadline itself: with no deadline of its own,
		// ctx IS cctx, so a model-set timeout would otherwise be reported as
		// though the human had pressed Ctrl-C, and the partial output - the
		// useful half of a timed-out build - would be dropped on the floor.
		return Result{}, ctx.Err()
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		// Either budget expiring lands here - the environment's default or a
		// per-call one the model asked for. Killed the whole process group, so
		// partial output (already in out) is all we get, and returncode -1 is the shape the observation template
		// already renders for a command that could not finish. budget is the
		// number the model asked for when it asked for one, which is the only
		// honest thing to name in a message about a deadline it can read back.
		secs := int(budget.Seconds())
		if secs < 1 {
			secs = 1
		}
		msg := fmt.Sprintf("Command %q timed out after %d seconds", command, secs)
		return Result{
			ReturnCode:    -1,
			CWD:           dir,
			ExceptionInfo: "An error occurred while executing the command: " + msg,
			Extra:         map[string]any{"exception_type": "TimeoutExpired", "exception": msg},
		}, nil
	}

	code := 0
	if runErr != nil {
		var es interp.ExitStatus
		if errors.As(runErr, &es) {
			code = int(es)
		} else {
			code = 1
		}
	}
	return Result{ReturnCode: code, CWD: dir}, nil
}

func (l *Local) envList() []string {
	out := make([]string, 0, len(l.Env))
	for k, v := range l.Env {
		out = append(out, k+"="+v)
	}
	return out
}

func orNil(f *os.File, err error) io.Reader {
	if err != nil {
		return nil
	}
	return f
}
