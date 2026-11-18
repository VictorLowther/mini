package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// The old contract read COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT out of command
// output and treated the run as over. That marker is dead: the echo now runs
// like any other command, earns its observation, and the loop keeps going -
// only a response without a tool call ends the run, and its text is the
// submission.
func TestSentinelEchoIsJustACommand(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("print(1)\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "initial")
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("print(2)\n"), 0o644)

	l := &Loop{
		Cfg: &Config{SystemTemplate: "s", InstanceTemplate: "t"},
		Model: &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{
			turn("chained", "echo COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT && git add -A && git diff --cached"),
			done("The staged change is the only edit: print(1) became print(2)."),
		}},
		Env: &env.Local{CWD: dir, Timeout: 20 * time.Second},
	}
	info, err := l.Run(context.Background(), "commit the change")
	if err != nil {
		t.Fatal(err)
	}
	if l.NCalls != 2 {
		t.Errorf("n_calls = %d, want 2: the marker echo must not end the run by itself", l.NCalls)
	}
	if info["exit_status"] != "Submitted" {
		t.Fatalf("info = %#v", info)
	}
	if sub, _ := info["submission"].(string); sub != "The staged change is the only edit: print(1) became print(2)." {
		t.Errorf("submission = %q", sub)
	}
	// The marker turn earned an observation: its output is an executed
	// command's result, not an exit.
	observed := false
	for _, msg := range l.messages {
		if e, ok := msg["extra"].(map[string]any); ok && e["command"] != nil {
			observed = true
		}
	}
	if !observed {
		t.Error("the marker turn produced no observation - it was treated as special")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
