package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/config"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// The two trajectories under testdata/ are recorded runs of the Python agent
// (text-mode model, so observations are role=="user" messages rendered with
// default.yaml's observation template). Replaying them through the Go loop with
// the same commands and the same environment proves the port end to end.

type fixtureMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var fence = regexp.MustCompile("(?s)```mswea_bash_command\n(.*?)\n```")

func loadFixture(t *testing.T, name string) []fixtureMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []fixtureMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

// replayConfig returns the default.yaml agent config (which is what produced
// these fixtures) plus its observation template.
func replayConfig(t *testing.T) (*Config, string) {
	t.Helper()
	m, err := config.FromSpec("default")
	if err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromMap(config.Sub(m, "agent"))
	cfg.OutputPath = ""
	return cfg, config.String(config.Sub(m, "model"), "observation_template", "")
}

func cannedFromFixture(msgs []fixtureMessage) []model.Message {
	var out []model.Message
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		var actions []model.Action
		for _, cmd := range fence.FindAllStringSubmatch(m.Content, -1) {
			actions = append(actions, model.Action{Command: cmd[1]})
		}
		out = append(out, turnActions(m.Content, actions...))
	}
	return out
}

func TestReplayLocalTrajectory(t *testing.T) {
	fixture := loadFixture(t, "local.traj.json")
	cfg, obsTemplate := replayConfig(t)
	m := &model.Deterministic{CostPerCall: 1, Outputs: cannedFromFixture(fixture), ObsTemplate: obsTemplate}
	l := &Loop{Cfg: cfg, Model: m, Env: &env.Local{Timeout: 10 * time.Second}}
	l.Cfg.CostLimit = 10

	info, err := l.Run(context.Background(), "Blah blah blah")
	if err != nil {
		t.Fatal(err)
	}
	if info["exit_status"] != "Submitted" {
		t.Fatalf("info = %#v", info)
	}
	// Messages 2..4 of the fixture (assistant, observation, assistant) must be
	// reproduced byte for byte; the fixture's trailing empty observation is the
	// submitting turn, which produces none in this code path.
	got, want := l.Messages, fixture[2:]
	for i := 2; i < 5; i++ {
		assertMessageMatches(t, got[i], want[i-2], false)
	}
	exit := got[len(got)-1]
	if exit["role"] != "exit" {
		t.Fatalf("last = %#v", exit)
	}
}

func TestReplayGitHubIssueTrajectory(t *testing.T) {
	fixture := loadFixture(t, "github_issue.traj.json")
	cfg, obsTemplate := replayConfig(t)

	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The file exactly as the fixture's first `cat` of it reports it: the
	// missing colon on the def line.
	broken := "#!/usr/bin/env python3\n\n\ndef division(a: float, b: float) -> float\n    return a/b\n\n\nif __name__ == \"__main__\":\n    print(division(123, 15))\n\n"
	if err := os.WriteFile(filepath.Join(dir, "tests", "missing_colon.py"), []byte(broken), 0o755); err != nil {
		t.Fatal(err)
	}

	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "initial")

	m := &model.Deterministic{CostPerCall: 1, Outputs: cannedFromFixture(fixture), ObsTemplate: obsTemplate}
	l := &Loop{Cfg: cfg, Model: m, Env: &env.Local{CWD: dir, Timeout: 20 * time.Second}}
	l.Cfg.CostLimit = 50
	if _, err := l.Run(context.Background(), "GitHub Issue: SyntaxError: invalid syntax"); err != nil {
		t.Fatal(err)
	}

	// Byte-exact replay of the machine-independent turns: reading a missing
	// absolute path and reading the seeded file. The rest depends on the
	// machine the fixture was recorded on (`ls` contents, GNU `sed -i`,
	// /testbed paths in tracebacks), so the remaining turns are covered by
	// asserting the run still terminates on the marker with the staged diff.
	for _, msgIndex := range []int{3, 9} {
		assertMessageMatches(t, l.Messages[msgIndex], fixture[msgIndex], false)
	}
	last := model.Extra(l.Messages[len(l.Messages)-1])
	if l.Messages[len(l.Messages)-1]["role"] != "exit" {
		t.Fatal("the replay did not end with an exit message")
	}
	if last["exit_status"] != "Submitted" {
		t.Fatalf("exit_status = %v", last["exit_status"])
	}
	if sub, _ := last["submission"].(string); !strings.Contains(sub, "diff --git a/tests/missing_colon.py") {
		t.Errorf("submission is not the staged diff:\n%s", sub)
	}
	// The fix must actually have been applied by the replayed commands.
	final, err := os.ReadFile(filepath.Join(dir, "tests", "missing_colon.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(final), "-> float:") {
		t.Errorf("the replayed sed never fixed the file:\n%s", final)
	}
}

func assertMessageMatches(t *testing.T, got model.Message, want fixtureMessage, normaliseListing bool) {
	t.Helper()
	if got["role"] != want.Role {
		t.Fatalf("role = %v, want %s", got["role"], want.Role)
	}
	gotContent, _ := got["content"].(string)
	wantContent := want.Content
	if normaliseListing {
		gotContent, wantContent = stripListingNoise(gotContent), stripListingNoise(wantContent)
	}
	if gotContent != wantContent {
		t.Errorf("content mismatch\n go: %q\npy: %q", gotContent, wantContent)
	}
}

// stripListingNoise drops the volatile parts of `ls -la` output: owners,
// groups, sizes and modification dates.
func stripListingNoise(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "d") || strings.HasPrefix(l, "-") {
			fields := strings.Fields(l)
			if len(fields) >= 9 {
				lines[i] = strings.Join(fields[:1], " ") + " " + strings.Join(fields[len(fields)-1:], " ")
			}
		}
		if strings.Contains(l, "root root") {
			lines[i] = "<owner/date line>"
		}
	}
	return strings.Join(lines, "\n")
}

func TestSubmitMarkerCarriesGitDiff(t *testing.T) {
	// The final turn of the github_issue fixture: the marker mixed into a chain
	// still submits, because its output's first line is the marker alone, and
	// the submission is everything after it (the staged diff).
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
