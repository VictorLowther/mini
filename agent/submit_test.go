package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

func TestSubmitMarkerCarriesGitDiff(t *testing.T) {
	// The marker mixed into a chain still submits, because its output's first
	// line is the marker alone, and the submission is everything after it (the
	// staged diff).
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("print(1)\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "initial")
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("print(2)\n"), 0o644)

	l := &Loop{
		Cfg: &Config{SystemTemplate: "s", InstanceTemplate: "t", CostLimit: 10},
		Model: &model.Deterministic{CostPerCall: 1, Outputs: []model.Message{turn("done",
			"echo COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT && git add -A && git diff --cached")}},
		Env: &env.Local{CWD: dir, Timeout: 20 * time.Second},
	}
	info, err := l.Run(context.Background(), "commit the change")
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := info["submission"].(string)
	if info["exit_status"] != "Submitted" {
		t.Fatalf("info = %#v", info)
	}
	for _, want := range []string{"diff --git a/a.py b/a.py", "-print(1)", "+print(2)"} {
		if !strings.Contains(sub, want) {
			t.Errorf("submission missing %q:\n%s", want, sub)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
