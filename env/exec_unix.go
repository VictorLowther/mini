//go:build !windows

package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
// processGroupExecHandler is interp.DefaultExecHandler with one change that
// matters: the child runs in its own session and cancellation signals the whole
// process group (negative PID), so a command that spawns grandchildren (npm
// install, pytest -n auto) cannot orphan them. Python's
// start_new_session=True + os.killpg(SIGKILL) gives the same guarantee.
//
// The sequence lives in cmd.Cancel, which os/exec calls from Wait on Wait's own
// goroutine, before the reap - which is the whole point. The previous shape ran
// it from a context.AfterFunc callback on a separate goroutine, so the final
// group SIGKILL could be issued after cmd.Wait had already reaped the leader.
// A pgid is only reusable once its leader is reaped, so that late signal was a
// live chance of killing an unrelated process group that had since been given
// the same number - and on Linux, where kernel.pid_max still defaults to 32768
// on many systems and pids are handed out sequentially with wraparound, a
// churny build can walk the whole space inside the two-second grace. A-12, and
// the harness's own rule that it never signals what it did not start.
//
// Doing it inside Cancel removes the race structurally instead of narrowing it:
// the process cannot be reaped while Cancel is running, because Wait is the only
// thing that reaps and Wait is blocked here. There is no goroutine of ours left
// to win a race against, no lock, and no OS-specific pid introspection - the
// kernel's own "unreaped means unrecycled" guarantee does the work. This is also
// the shape upstream's DefaultExecHandler already uses, so the group semantics
// are the only difference from stock rather than a second lifetime model.
//
// Note that Cancel only fires for a context attached to the Cmd itself, so the
// command is built with exec.CommandContext. The bare exec.Cmd literal this
// replaced had no context to consult, which is exactly why the callback had to
// be installed by hand.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		cmd := exec.CommandContext(ctx, path)
		cmd.Args = args
		cmd.Env = execEnvList(hc.Env)
		cmd.Dir = hc.Dir
		cmd.Stdin = hc.Stdin
		cmd.Stdout = hc.Stdout
		cmd.Stderr = hc.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		killGroup := func(sig syscall.Signal) {
			// Setsid made the child both session and process-group leader, so
			// its pid is the pgid of everything that stayed in the group.
			_ = syscall.Kill(-cmd.Process.Pid, sig)
		}
		if killTimeout > 0 {
			// Interrupt the group, give it the same window a foreground command
			// gets, then kill the group. Returning os.ErrProcessDone tells
			// Wait that this was an orderly shutdown it should not dress up as
			// an error of its own: the command's signalled status is the answer.
			cmd.Cancel = func() error {
				killGroup(syscall.SIGINT)
				// The grace is paid unconditionally, and that is a measured
				// cost worth naming: Wait runs Cancel to completion before it
				// reaps, so a command that dies the instant SIGINT lands still
				// costs killTimeout. env.TestTimeoutReturnsPartialOutput moved
				// from ~1s to ~3s for exactly this reason. It is 2s on a
				// cancellation or a timeout, never on a normal completion.
				//
				// Skipping it needs a way to ask "is the group gone yet"
				// without reaping, and os/exec offers none: Signal(0) succeeds
				// on the zombie leader, and a WNOHANG Wait4 would steal the
				// reap that Wait is about to do. So either the grace is waited
				// out here, or the group SIGKILL goes back to a goroutine that
				// must be fenced against the reap - which is A-12. Shortening
				// this sleep is that trade, not a free optimisation.
				//
				// Reviewed and accepted by the owner on 2026-11-18: two seconds on
				// a path the user already chose to interrupt is worth the
				// guarantee that the group is really dead. Do not reclaim that
				// latency by cutting the sleep or moving the kill back to a
				// goroutine - that is A-12 returning.
				time.Sleep(killTimeout)
				killGroup(syscall.SIGKILL)
				return os.ErrProcessDone
			}
		} else {
			// No grace requested: kill straight away, as upstream does.
			cmd.Cancel = func() error {
				killGroup(syscall.SIGKILL)
				return os.ErrProcessDone
			}
		}
		// Bounded drain of the inherited pipes: without this, Wait blocks until
		// an orphaned grandchild closes stdout. It no longer carries the kill
		// escalation, because Cancel does that before Wait ever reaches this.
		cmd.WaitDelay = killTimeout

		if err = cmd.Start(); err == nil {
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
