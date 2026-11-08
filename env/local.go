package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	// Timeout bounds one command. Zero means DefaultTimeout.
	Timeout time.Duration
}

func (l *Local) timeout() time.Duration {
	if l.Timeout <= 0 {
		return DefaultTimeout
	}
	return l.Timeout
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

	// yagni: no per-call timeout override; nothing in the reference passes one.
	cctx, cancel := context.WithTimeout(ctx, l.timeout())
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
		interp.ExecHandler(processGroupExecHandler(killGrace)),
	)
	if rerr != nil {
		return Result{ReturnCode: -1, ExceptionInfo: fmt.Sprintf("An error occurred while executing the command: %v", rerr)}, nil
	}

	// interp.Run can panic on exotic input; a panic must not kill the agent.
	// The command's bytes are already in out, so the Result only reports the
	// failure itself.
	defer func() {
		if rec := recover(); rec != nil {
			result = Result{
				ReturnCode:    -1,
				ExceptionInfo: fmt.Sprintf("An error occurred while executing the command: command execution panic: %v", rec),
				Extra:         map[string]any{"exception_type": "Panic", "exception": fmt.Sprint(rec)},
			}
			err = nil
		}
	}()

	runErr := runner.Run(cctx, file)

	switch {
	case ctx.Err() != nil:
		// The user (or the shutdown signal) stopped the run mid-command: an
		// interruption, not an observation.
		return Result{}, ctx.Err()
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		// Killed the whole process group, so partial output (already in out)
		// is all we get; returncode -1 mirrors subprocess.TimeoutExpired
		// handling.
		msg := fmt.Sprintf("Command %q timed out after %d seconds", command, int(l.timeout().Seconds()))
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
