package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"database/sql"
	"uuid"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// cliEnv points the store, the global config and the wizard at temp dirs and
// returns (stateDir, runInRepo).
func cliEnv(t *testing.T, task string) (stateDir, traj string) {
	t.Helper()
	stateDir = t.TempDir()
	t.Setenv("MINI_STATE_DIR", stateDir)
	cfgDir := t.TempDir()
	t.Setenv("MINI_GLOBAL_CONFIG_DIR", cfgDir)
	t.Setenv("MINI_CONFIGURED", "true")
	t.Setenv("MINI_MODEL_NAME", "test-model")
	t.Setenv("MINI_SILENT_STARTUP", "1")
	traj = filepath.Join(cfgDir, "out.traj.json")
	return stateDir, traj
}

func runCLI(t *testing.T, url, task, traj string, extra ...string) int {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout, err := os.CreateTemp(t.TempDir(), "mini-out")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	args := append([]string{"-p", "-y", "-t", task, "--url", url, "-o", traj, "--exit-immediately"}, extra...)
	code := run(args, devnull, stdout, os.Stderr)
	if code != 0 {
		data, _ := os.ReadFile(stdout.Name())
		t.Fatalf("exit code %d, output:\n%s", code, data)
	}
	return code
}

func repoDir(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return repo
}

// The end-to-end store check: one run leaves exactly one session row tagged
// with the repo root, a transcript, and untrimmed result files; the exported
// trajectory still carries raw_output read back from them.
func TestRunPrintModeWritesSession(t *testing.T) {
	srv := fixtureServer(t, "toolcall.sse", "submit.sse")
	defer srv.Close()
	stateDir, traj := cliEnv(t, "")
	repo := repoDir(t)

	task := "list the directory, then submit"
	runCLI(t, srv.URL, task, traj)

	workdir, project := projectOf(repo)
	if project != workdir {
		t.Fatalf("projectOf did not find the .git root: %q vs %q", project, workdir)
	}
	store, err := session.Open(filepath.Join(stateDir, "sessions.db"), filepath.Join(stateDir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.List("", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("sessions = %d rows, want 1", len(rows))
	}
	s := rows[0]
	if s.Project != project {
		t.Errorf("project = %q, want %q", s.Project, project)
	}
	if s.Title != task {
		t.Errorf("title = %q, want %q", s.Title, task)
	}
	if s.ExitStatus != "Submitted" || s.NCalls != 2 {
		t.Errorf("row = %+v", s)
	}
	sess, err := store.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 6 {
		t.Errorf("transcript has %d turns, want >= 6", len(msgs))
	}

	// Result files: ls -la only. The completion turn makes no tool call, so
	// it earns no call row and no result file.
	logs, err := os.ReadDir(filepath.Join(stateDir, "tcr", s.ID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("result files = %v, want just 1.log", logs)
	}
	lsBytes, err := os.ReadFile(filepath.Join(stateDir, "tcr", s.ID.String(), "1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lsBytes), "total ") {
		t.Errorf("ls result file = %q, want a directory listing", lsBytes)
	}

	// The trajectory export keeps raw_output, read back from the files.
	data, err := os.ReadFile(traj)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []struct {
			Extra map[string]any `json:"extra"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	observed := 0
	for _, m := range doc.Messages {
		if m.Extra == nil {
			continue
		}
		if _, ok := m.Extra["raw_output"]; !ok {
			continue
		}
		observed++
		if p, _ := m.Extra["tcr_path"].(string); p != "" {
			b, err := os.ReadFile(p)
			if err != nil || string(b) != m.Extra["raw_output"] {
				t.Errorf("raw_output %q != file %s contents %q (%v)", m.Extra["raw_output"], p, b, err)
			}
		} else if m.Extra["raw_output"] != "" {
			t.Errorf("observation with no tcr_path carries %q", m.Extra["raw_output"])
		}
	}
	if observed == 0 {
		t.Error("trajectory lost raw_output")
	}
}

// Parallel tool calls: one issuing turn, two call rows, two files, each
// observation linked back to its own call.
func TestRunParallelToolCallRows(t *testing.T) {
	srv := fixtureServer(t, "parallel_toolcalls.sse", "submit.sse")
	defer srv.Close()
	stateDir, traj := cliEnv(t, "")
	repoDir(t)

	runCLI(t, srv.URL, "run two commands then submit", traj)

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(stateDir, "sessions.db")+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	type call struct {
		turn, idx, tcno int
		id, command     string
		status          string
		resultTurn      sql.NullInt64
	}
	rows, err := db.Query(`SELECT turn, idx, tcno, id, command, status, result_turn FROM tool_calls ORDER BY tcno`)
	if err != nil {
		t.Fatal(err)
	}
	var calls []call
	for rows.Next() {
		var c call
		if err := rows.Scan(&c.turn, &c.idx, &c.tcno, &c.id, &c.command, &c.status, &c.resultTurn); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	rows.Close()
	if len(calls) < 2 {
		t.Fatalf("call rows = %d, want >= 2", len(calls))
	}
	c1, c2 := calls[0], calls[1]
	if c1.turn != c2.turn {
		t.Errorf("the two calls name different issuing turns: %d vs %d", c1.turn, c2.turn)
	}
	want := []struct {
		idx int
		tc  int
		id  string
		cmd string
	}{{0, 1, "call_1", "echo one"}, {1, 2, "call_2", "echo two"}}
	for i, w := range want {
		c := calls[i]
		if c.idx != w.idx || c.tcno != w.tc || c.id != w.id || c.command != w.cmd || c.status != "completed" {
			t.Errorf("call %d = %+v, want idx=%d tcno=%d id=%s command=%s completed", i, c, w.idx, w.tc, w.id, w.cmd)
		}
	}
	// Each observation turn's tool_call_id matches the call it answers.
	for i, c := range calls[:2] {
		if !c.resultTurn.Valid {
			t.Fatalf("call %d never got a result_turn", i)
		}
		var tcID string
		if err := db.QueryRow(`SELECT tool_call_id FROM turns WHERE seq = ?`, c.resultTurn.Int64).Scan(&tcID); err != nil {
			t.Fatal(err)
		}
		if tcID != c.id {
			t.Errorf("call %d answered by turn with tool_call_id %q, want %q", i, tcID, c.id)
		}
	}
	var sid string
	if err := db.QueryRow(`SELECT id FROM sessions`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	for i, wantBody := range []string{"one\n", "two\n"} {
		b, err := os.ReadFile(filepath.Join(stateDir, "tcr", sid, itoaMini(i+1)+".log"))
		if err != nil || string(b) != wantBody {
			t.Errorf("call file %d = %q, %v; want %q", i+1, b, err, wantBody)
		}
	}
}

// A resumed run loads the stored transcript, replays it to the plain front
// end, and continues the same sessions row instead of forking a new one.
func TestResumeContinuesSession(t *testing.T) {
	stateDir, traj := cliEnv(t, "")
	srv := fixtureServer(t, "toolcall.sse", "submit.sse")
	defer srv.Close()
	var wire [][]string
	srv2 := serveFixtures(t, &wire, "toolcall.sse", "submit.sse")
	defer srv2.Close()
	repoDir(t)

	runCLI(t, srv.URL, "first task then submit", traj)

	sessID := mustSessionID(t, stateDir)

	runCLI(t, srv2.URL, "second task then submit", traj, "-r", sessID[:8])

	// The resumed run rebuilt the conversation: its first request carried the
	// first run's turns plus the new task, and none of the transcript's exit
	// rows - role "exit" is bookkeeping, not model context, and an
	// OpenAI-compatible server would reject it.
	if len(wire) == 0 {
		t.Fatal("the resumed run never queried the model")
	}
	first := wire[0]
	for _, r := range first {
		if r == "exit" {
			t.Fatalf("resumed request carried an exit row on the wire: %v", first)
		}
	}
	want := []string{"system", "user", "assistant", "tool", "assistant", "user"}
	if len(first) != len(want) {
		t.Fatalf("resumed request roles = %v, want %v", first, want)
	}
	for i, w := range want {
		if first[i] != w {
			t.Fatalf("resumed request roles = %v, want %v", first, want)
		}
	}

	store, err := session.Open(filepath.Join(stateDir, "sessions.db"), filepath.Join(stateDir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, _ := store.List("", true, 10)
	if len(rows) != 1 {
		t.Fatalf("resume created a second session: %d rows", len(rows))
	}
	if rows[0].NCalls != 4 {
		t.Errorf("n_calls = %d, want 4 (2 per run)", rows[0].NCalls)
	}
	if rows[0].Title != "first task then submit" {
		t.Errorf("title changed on resume: %q", rows[0].Title)
	}
	sess, _ := store.Get(rows[0].ID)
	msgs, err := sess.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	// Two runs of 6-7 turns: the resumed one appended, not reset.
	if len(msgs) < 11 {
		t.Errorf("transcript = %d turns, want >= 11", len(msgs))
	}
	if msgs[len(msgs)-1]["role"] != "exit" {
		t.Errorf("last turn = %v", msgs[len(msgs)-1]["role"])
	}
	// The resumed run appended the new task as a UserNewTask message.
	found := false
	for _, m := range msgs[6:] {
		if e, _ := m["extra"].(map[string]any); e != nil && e["interrupt_type"] == "UserNewTask" {
			found = true
			if !strings.Contains(m["content"].(string), "second task") {
				t.Errorf("new-task message = %v", m["content"])
			}
		}
	}
	if !found {
		t.Error("resumed run did not record the new task message")
	}
}

// A process killed mid-command leaves an assistant turn whose call never
// completed. Because recording pre-allocated the call's slot, the transcript
// is already a legal history - the slot answers with the aborted observation -
// and -r rebuilds a conversation a strict server accepts. Before slots, the
// dangling tool_calls poisoned the session: every resume 400'd and appended
// its own exit row over the same hole.
func TestResumeAfterMidCommandCrash(t *testing.T) {
	stateDir, traj := cliEnv(t, "")
	// The fixture server must be built before repoDir chdirs the test into
	// the scratch repo: the scripts are read by relative path.
	var wire [][]string
	srv := serveFixtures(t, &wire, "toolcall.sse", "submit.sse")
	defer srv.Close()
	repo := repoDir(t)

	st, err := session.Open(filepath.Join(stateDir, "sessions.db"), filepath.Join(stateDir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	_, proj := projectOf(repo)
	sess, err := st.Create(session.Row{ID: uuid.NewV7(), Workdir: repo, Project: proj, ModelName: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []model.Message{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "hang on"},
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{
			{"id": "call_hang", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": `{"command":"sleep 99"}`}},
		}, "extra": map[string]any{"actions": []model.Action{{Command: "sleep 99", ToolCallID: "call_hang"}}}},
	} {
		if _, err := sess.RecordMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	if code := runCLI(t, srv.URL, "second task", traj, "-r", sess.ID.String()[:8]); code != 0 {
		t.Fatalf("resume after a crash exited %d", code)
	}
	if len(wire) == 0 {
		t.Fatal("the resumed run never queried the model")
	}
	first := wire[0]
	want := []string{"system", "user", "assistant", "tool", "user"}
	if len(first) != len(want) {
		t.Fatalf("resumed request roles = %v, want %v", first, want)
	}
	for i, w := range want {
		if first[i] != w {
			t.Fatalf("resumed request roles = %v, want %v", first, want)
		}
	}
}

func mustSessionID(t *testing.T, stateDir string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(stateDir, "sessions.db")+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow(`SELECT id FROM sessions`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func itoaMini(i int) string {
	b, _ := json.Marshal(i)
	return strings.Trim(string(b), `"`)
}

// mini sessions lists; mini sessions rm cascades the rows and the files.
func TestSessionsCommandListAndRemove(t *testing.T) {
	stateDir, traj := cliEnv(t, "")
	srv := fixtureServer(t, "toolcall.sse", "submit.sse")
	defer srv.Close()
	repoDir(t)
	runCLI(t, srv.URL, "list and submit", traj)

	var out strings.Builder
	if err := runSessionsCommand([]string{}, &out); err != nil {
		t.Fatal(err)
	}
	listing := out.String()
	sid := mustSessionID(t, stateDir)
	if !strings.Contains(listing, sid) || !strings.Contains(listing, "list and submit") || !strings.Contains(listing, "STEPS") {
		t.Fatalf("sessions listing:\n%s", listing)
	}

	resultFile := filepath.Join(stateDir, "tcr", sid, "1.log")
	if _, err := os.Stat(resultFile); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSessionsCommand([]string{"rm", sid[:12]}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Deleted session "+sid) {
		t.Errorf("rm output = %q", out.String())
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(stateDir, "sessions.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"SELECT count(*) FROM sessions", "SELECT count(*) FROM turns", "SELECT count(*) FROM tool_calls"} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s = %d, %v; want 0", q, n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "tcr", sid)); !os.IsNotExist(err) {
		t.Errorf("result directory survived rm: %v", err)
	}
	if err := runSessionsCommand([]string{"rm", uuid.NewV7().String()[:8]}, &out); err == nil {
		t.Error("rm of an unknown prefix succeeded")
	}
}

// mustRealpath applies the same canonicalisation projectOf does (macOS
// /var -> /private/var).
func mustRealpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResumeFlagParsing(t *testing.T) {
	o, err := parseFlags([]string{"-p", "-r", "-t", "x"})
	if err != nil || !o.resume.present || o.resume.value != "" {
		t.Errorf("bare -r = %+v, %v; want present with empty value", o, err)
	}
	if o.task != "x" {
		t.Errorf("task = %q, want x", o.task)
	}
	o, err = parseFlags([]string{"-r", "01ab", "-t", "y"})
	if err != nil || !o.resume.present || o.resume.value != "01ab" || o.task != "y" {
		t.Errorf("-r 01ab = %+v, %v", o, err)
	}
	o, err = parseFlags([]string{"--resume=01ab"})
	if err != nil || !o.resume.present || o.resume.value != "01ab" {
		t.Errorf("--resume=01ab = %+v, %v", o, err)
	}
	o, err = parseFlags([]string{"-t", "z"})
	if err != nil || o.resume.present {
		t.Errorf("-r absent = %+v, %v; want not present", o, err)
	}
}

func TestProjectOf(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	nested := filepath.Join(repo, "deep", "er")
	os.MkdirAll(nested, 0o755)
	os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: x\n"), 0o644)

	wd, project := projectOf(nested)
	if filepath.Base(wd) != "er" || project != mustRealpath(t, repo) {
		t.Errorf("projectOf(nested) = %q, %q; want the .git root %q", wd, project, repo)
	}
	// A symlink to the tree must resolve to the same root, not a second
	// history.
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	_, projectViaLink := projectOf(filepath.Join(link, "deep"))
	if projectViaLink != project {
		t.Errorf("through the symlink: %q, want %q", projectViaLink, project)
	}
	// Not a repo: the project is the canonical workdir itself.
	outside := filepath.Join(base, "plain")
	os.MkdirAll(outside, 0o755)
	wd, project = projectOf(outside)
	if wd != project || project != mustRealpath(t, outside) {
		t.Errorf("non-repo: wd=%q project=%q", wd, project)
	}
}

// TestSessionsHidesAndProtectsSubagentSessions is the user-facing half of
// docs/parallel-sessions.md §5.4 and §5.11: a subagent session is listed by
// neither resume nor rm, and it goes away only with its parent - rows and files
// together. It is written against the real command rather than the store because
// the two bugs it guards are both in what a human types: `mini -r` choosing the
// newest row in the project, and `mini sessions rm` reaching something it should
// not be able to name.
func TestSessionsHidesAndProtectsSubagentSessions(t *testing.T) {
	stateDir, traj := cliEnv(t, "")
	srv := fixtureServer(t, "toolcall.sse", "submit.sse")
	defer srv.Close()
	repoDir(t)
	runCLI(t, srv.URL, "the human's task", traj)
	parentID := mustSessionID(t, stateDir)

	st, err := session.Open(filepath.Join(stateDir, "sessions.db"), filepath.Join(stateDir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := uuid.Parse(parentID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := st.Get(pid)
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Create(session.Row{
		ID: uuid.NewV7(), ParentID: parent.ID.String(), Kind: session.KindSubagent,
		Title: "quantise 70b", Workdir: parent.Workdir, Project: parent.Project, ModelName: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// The child's result directory, as OpenToolCall and JobPath would leave it:
	// inside the per-session directory that the row's id names.
	childDir := filepath.Join(stateDir, "tcr", child.ID.String())
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "job-j1.log"), []byte("loss=0.42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runSessionsCommand([]string{}, &out); err != nil {
		t.Fatal(err)
	}
	listing := out.String()
	if !strings.Contains(listing, parentID) {
		t.Errorf("the human's own session is missing from:\n%s", listing)
	}
	if strings.Contains(listing, child.ID.String()) {
		t.Errorf("the listing shows a subagent session, which is what §3.5.7 forbids:\n%s", listing)
	}
	if !strings.Contains(listing, "(1 sub)") {
		t.Errorf("the listing does not account for the subagent under this run:\n%s", listing)
	}

	// rm cannot name a child. The message is the existing "no session matching",
	// which is the right shape: the row is not a thing this command can point at,
	// not a thing that failed to be removed.
	out.Reset()
	err = runSessionsCommand([]string{"rm", child.ID.String()}, &out)
	if err == nil || !strings.Contains(err.Error(), "no session matching") {
		t.Errorf("rm of a subagent err = %v, want it refused as unnameable", err)
	}
	if _, statErr := os.Stat(childDir); statErr != nil {
		t.Error("the refused rm still removed the child's files")
	}

	// Removing the parent takes the subtree, including the files the cascade
	// alone would have stranded.
	out.Reset()
	if err := runSessionsCommand([]string{"rm", parentID}, &out); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(stateDir, "tcr", parentID),
		childDir,
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived the removal of its tree", dir)
		}
	}

	st2, err := session.Open(filepath.Join(stateDir, "sessions.db"), filepath.Join(stateDir, "tcr"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.Get(child.ID); err == nil {
		t.Error("the child row survived its parent's removal")
	}
}
