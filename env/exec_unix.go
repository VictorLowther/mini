//go:build !windows

package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// KillGrace is the INT-to-KILL window when a command is cancelled. Exported
// because the job manager promises a backgrounded command the same chance to
// clean up that a foreground one gets (job.DefaultKillGrace).
const KillGrace = 2 * time.Second

// processGroupExecHandler is interp.DefaultExecHandler with one change that
// matters: the child runs in its own session and cancellation signals the whole
// process group (negative PID), so a command that spawns grandchildren (npm
// install, pytest -n auto, a stray `&`) cannot orphan them. Python's
// start_new_session=True + os.killpg(SIGKILL) gives the same guarantee.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		cmd := exec.Cmd{
			Path:   path,
			Args:   args,
			Env:    execEnvList(hc.Env),
			Dir:    hc.Dir,
			Stdin:  hc.Stdin,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
			// Bounded drain of the inherited pipes: without this, Wait blocks
			// until an orphaned grandchild closes stdout.
			WaitDelay: killTimeout,
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		err = cmd.Start()
		if err == nil {
			stop := context.AfterFunc(ctx, func() {
				pgid := cmd.Process.Pid
				if killTimeout <= 0 {
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
					return
				}
				_ = syscall.Kill(-pgid, syscall.SIGINT)
				time.Sleep(killTimeout)
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			})
			defer stop()
			err = cmd.Wait()
		}
		return exitStatusFromError(ctx, hc.Stderr, err)
	}
}

// exitStatusFromError maps exec failures onto interp exit statuses the way
// DefaultExecHandler does: 127 for a missing binary, 128+signal for a signalled
// child (unless we killed it), and cancellation propagated as ctx.Err().
func exitStatusFromError(ctx context.Context, stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	switch e := err.(type) {
	case *exec.ExitError:
		if status, ok := e.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return interp.ExitStatus(128 + status.Signal())
		}
		return interp.ExitStatus(uint8(e.ExitCode()))
	case *exec.Error:
		fmt.Fprintf(stderr, "%v\n", e)
		return interp.ExitStatus(127)
	default:
		if errors.Is(err, exec.ErrWaitDelay) {
			// Exited cleanly; only the pipe drain with an orphan timed out.
			return nil
		}
		return err
	}
}

// execEnvList flattens the interpreter environment into the exported string
// pairs a child process receives.
func execEnvList(env expand.Environ) []string {
	var out []string
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			out = append(out, name+"="+vr.Str)
		}
		return true
	})
	return out
}
