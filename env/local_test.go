package env

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, l *Local, command string) Result {
	t.Helper()
	res, err := l.Execute(context.Background(), command, "")
	if err != nil {
		t.Fatalf("Execute(%q) error: %v", command, err)
	}
	return res
}

func TestExecuteBasics(t *testing.T) {
	l := &Local{Timeout: 10 * time.Second}
	cases := []struct {
		command string
		want    string
		code    int
	}{
		{"echo hi", "hi\n", 0},
		{"exit 1", "", 1},
		{"echo out; echo err >&2", "out\nerr\n", 0},
		{"echo $(echo nested) | tr a-z A-Z", "NESTED\n", 0},
		{"false && echo no || echo yes", "yes\n", 0},
		{"nosuchbinary-xyz", "\"nosuchbinary-xyz\": executable file not found in $PATH\n", 127},
		{"echo one; exit 3; echo two", "one\n", 3},
	}
	for _, c := range cases {
		res := run(t, l, c.command)
		if res.Output != c.want || res.ReturnCode != c.code {
			t.Errorf("Execute(%q) = (%q, %d), want (%q, %d) [exc=%q]", c.command, res.Output, res.ReturnCode, c.want, c.code, res.ExceptionInfo)
		}
	}
}

// TestBashGrammar is the observable proof that the interpreter speaks bash and
// not POSIX sh: /bin/sh on Linux is dash and rejects every construct below,
// which would make the prompts (they teach bash) lie to the model.
func TestBashGrammar(t *testing.T) {
	l := &Local{Timeout: 10 * time.Second}
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"test double bracket", `echo $( [[ 1 == 1 ]] && printf ok )`, "ok\n"},
		{"indexed array", `a=(x y); echo ${a[1]}`, "y\n"},
		{"arithmetic expansion", `echo $((111*9))`, "999\n"},
		{"function definition", `function f { printf called; }; f`, "called"},
		{"dollar quote newline", `printf %s $'a\nb'`, "a\nb"},
		{"brace expansion", `echo {a,b}c`, "ac bc\n"},
		{"extended glob", `shopt -s extglob; [[ abc != ?(a)bc ]] && echo no || echo yes`, "yes\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, l, c.cmd)
			if res.Output != c.want {
				t.Errorf("Execute(%q) = %q (code %d, exc %q), want %q", c.cmd, res.Output, res.ReturnCode, res.ExceptionInfo, c.want)
			}
		})
	}
	// A heredoc must actually write the file.
	dir := t.TempDir()
	res := run(t, l, "cat <<'EOF' > "+dir+"/f.txt\nhello\nEOF\ncat "+dir+"/f.txt")
	if res.Output != "hello\n" {
		t.Errorf("heredoc: %q code=%d exc=%q", res.Output, res.ReturnCode, res.ExceptionInfo)
	}
}

func TestEnvPrecedence(t *testing.T) {
	t.Setenv("MINI_TEST_VAR", "from-process")
	l := &Local{Env: map[string]string{"MINI_TEST_VAR": "from-config", "MINI_ONLY": "added"}, Timeout: 10 * time.Second}
	if got := run(t, l, `echo $MINI_TEST_VAR $MINI_ONLY`).Output; got != "from-config added\n" {
		t.Errorf("config env must win and extend the process env, got %q", got)
	}
	// The config env block also reaches the prompt variables.
	if vars := l.TemplateVars(); vars["MINI_TEST_VAR"] != "from-process" {
		t.Errorf("TemplateVars must expose the process environment, got %#v", vars["MINI_TEST_VAR"])
	}
}

func TestCWDPrecedence(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	os.MkdirAll(a, 0o755)
	os.MkdirAll(b, 0o755)

	l := &Local{Timeout: 10 * time.Second}
	if got, want := strings.TrimSpace(run(t, l, "pwd").Output), base; got != want {
		t.Errorf("default cwd = %q, want the process cwd %q", got, want)
	}
	l.CWD = b
	if got, want := strings.TrimSpace(run(t, l, "pwd").Output), b; got != want {
		t.Errorf("config cwd = %q, want %q", got, want)
	}
	res, err := l.Execute(context.Background(), "pwd", a)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(res.Output), a; got != want {
		t.Errorf("argument cwd = %q, want %q", got, want)
	}
}

func TestTimeoutReturnsPartialOutput(t *testing.T) {
	l := &Local{Timeout: time.Second}
	start := time.Now()
	res := run(t, l, "echo $((111*9)); sleep 30")
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout not enforced, took %v", elapsed)
	}
	if !strings.Contains(res.Output, "999") {
		t.Errorf("partial output lost: %q", res.Output)
	}
	if res.ReturnCode != -1 {
		t.Errorf("returncode = %d, want -1", res.ReturnCode)
	}
	if res.Extra["exception_type"] != "TimeoutExpired" {
		t.Errorf("extra = %#v", res.Extra)
	}
	if !strings.Contains(res.ExceptionInfo, "timed out") {
		t.Errorf("exception_info = %q", res.ExceptionInfo)
	}
}

// NoOrphanProcesses proves the custom exec handler earns its keep: Python's
// start_new_session + killpg never orphaned grandchildren, and the default
// interp handler would.
func TestNoOrphanProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process groups")
	}
	l := &Local{Timeout: time.Second}
	res := run(t, l, "echo $((111*9)); sleep 30 & sleep 30")
	if !strings.Contains(res.Output, "999") {
		t.Fatalf("expected partial output, got %q", res.Output)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pgrepHas(t, "sleep 30") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("orphaned grandchildren survived the timeout")
}

func pgrepHas(t *testing.T, pattern string) bool {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func TestBusyLoopStopsOnContextCancel(t *testing.T) {
	l := &Local{Timeout: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := l.Execute(ctx, "while :; do :; done", "")
	if err == nil {
		t.Fatal("expected the cancelled context to surface as an error")
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("pure-shell loop ignored cancellation for %v", time.Since(start))
	}
}

func TestNoStdinDoesNotHang(t *testing.T) {
	l := &Local{Timeout: 3 * time.Second}
	done := make(chan Result, 1)
	go func() {
		res, err := l.Execute(context.Background(), "cat", "")
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Output != "" {
			t.Errorf("cat on /dev/null should be empty, got %q", res.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("`cat` blocked forever; stdin must reach EOF immediately")
	}
}

func TestParseErrorIsAnObservation(t *testing.T) {
	l := &Local{Timeout: 5 * time.Second}
	res := run(t, l, "if then fi")
	if res.ReturnCode != -1 || !strings.Contains(res.ExceptionInfo, "could not parse command") {
		t.Errorf("got %#v", res)
	}
}

func TestUnameTemplateVars(t *testing.T) {
	u := Uname()
	if u["system"] == "" || u["machine"] == "" {
		t.Fatalf("uname incomplete: %#v", u)
	}
	if u["system"] != titleGo(runtime.GOOS) && u["system"] != "Darwin" {
		t.Errorf("system = %q, want the capitalised GOOS", u["system"])
	}
	for _, want := range []string{"system", "node", "release", "version", "machine", "processor"} {
		if _, ok := u[want]; !ok {
			t.Errorf("missing uname field %q", want)
		}
	}
	if runtime.GOOS == "darwin" && u["system"] != "Darwin" {
		t.Errorf("Darwin must read %q, got %q (the instance template branches on it)", "Darwin", u["system"])
	}
	vars := (&Local{}).TemplateVars()
	if vars["system"] != u["system"] {
		t.Errorf("TemplateVars lost the uname merge: %#v", vars["system"])
	}
	if vars["PATH"] == nil || vars["PATH"] == "" {
		t.Error("TemplateVars must expose the whole process environment")
	}
	if _, ok := vars["env"].(map[string]any); !ok {
		t.Errorf("config dump missing env map: %#v", vars["env"])
	}
}
