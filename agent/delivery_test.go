package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// The delivery contract, tested as a shape rather than as prose: a wake says
// which job ended, how it ended, where its bytes are and how many of them there
// are - and never carries the bytes themselves. The body used to be inlined for
// a subagent, which was affordable while a boundary reported one job and is not
// now that it reports the whole pending set: eight inlined reports is eight
// times the message, competing with the live transcript for the window.
//
// These seed rows and firings directly, because the contract is about what a
// boundary says about a finished row, not about how the row came to finish.

// seedEndedJob writes a job's output file and its row, ends the row, and queues
// the exit firing: exactly the state a delivery is made from, without needing a
// process to produce it.
func seedEndedJob(t *testing.T, jm *job.Manager, id, kind, body string, code int) string {
	t.Helper()
	path := jm.Session.JobPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := jm.Session.CreateJob(session.JobRow{
		ID: id, Kind: kind, Command: "run " + id, Label: "seeded " + id, OutputPath: path,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := jm.Session.FinishJob(id, session.JobCompleted, &code, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jm.Session.RecordJobEvent(session.JobEventRow{
		JobID: id, MonitorID: "exit", Reason: row.State, ExitCode: &code,
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func wakeText(t *testing.T, l *Loop) string {
	t.Helper()
	got := l.jobWake()
	if len(got) != 1 {
		t.Fatalf("jobWake = %d messages, want 1", len(got))
	}
	return fmt.Sprint(got[0]["content"])
}

func countLines(n int, word string) string {
	return fmt.Sprintf("%d %ss", n, word)
}

// A subagent's ending used to paste its report into the wake. Now the wake says
// how big the report is and where it is, and the parent reads it in one call.
func TestDeliveryCarriesCountsAndPathNotBody(t *testing.T) {
	l, jm, _ := newJobLoop(t, &model.Deterministic{CostPerCall: 1}, &env.Local{Timeout: 5 * time.Second})
	body := "the findings, in full\n" + strings.Repeat("detail line\n", 400) + "SECRET-DO-NOT-PASTE\n"
	path := seedEndedJob(t, jm, "j1", session.KindSubagent, body, 0)
	wantLines := strings.Count(body, "\n")

	text := wakeText(t, l)
	if strings.Contains(text, "SECRET-DO-NOT-PASTE") {
		t.Errorf("the delivery inlined the job's output; the body must stay in the file:\n%s", text)
	}
	for _, want := range []string{
		"job j1 ended: completed", "(exit 0)",
		"out=" + path,
		fmt.Sprintf("(%s, %s, last write", countLines(wantLines, "line"), countLines(len(body), "byte")),
		"read it with:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("delivery missing %q:\n%s", want, text)
		}
	}
	if extra := len(text); extra > 2000 {
		t.Errorf("a delivery should be a record, not a report: %d bytes", extra)
	}
	// A report leads with its answer, so the guidance heads the file rather than
	// tailing it. That distinction is the excerpt rule the inline body used to
	// apply, now expressed as a command.
	if !strings.Contains(text, "sed -n '1,") {
		t.Errorf("a subagent's guidance should read from the head:\n%s", text)
	}
	if strings.Contains(text, "tail -n") {
		t.Errorf("tailing a report reads its least useful end:\n%s", text)
	}
	if !strings.Contains(text, "for the first 100 lines") {
		t.Errorf("the guidance must say which end of a report to read:\n%s", text)
	}
}

// A bash job's log ends with its answer, so its guidance is the tail - and a
// file small enough to read at once is called that, because instructing the
// model to page through two lines is worse than telling it not to.
func TestDeliveryGuidanceFollowsTheCounts(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		body  string
		wants []string
	}{
		{"tiny bash log is read whole", session.KindBash, "one line\n",
			[]string{"read it with: cat", "1 line, 9 bytes", "the whole output is 1 line"}},
		{"big bash log is tailed", session.KindBash, strings.Repeat("log line\n", 400),
			[]string{"read it with: tail -n 100", "grep -n PATTERN",
				"400 lines, 3600 bytes", "for the last 100 lines"}},
		{"empty output says so", session.KindBash, "",
			[]string{"0 lines, 0 bytes", "the job wrote nothing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, jm, _ := newJobLoop(t, &model.Deterministic{CostPerCall: 1}, &env.Local{Timeout: 5 * time.Second})
			seedEndedJob(t, jm, "j1", tc.kind, tc.body, 0)
			text := wakeText(t, l)
			for _, want := range tc.wants {
				if !strings.Contains(text, want) {
					t.Errorf("delivery missing %q:\n%s", want, text)
				}
			}
		})
	}
}

// The batching the record shape makes possible: every owed ending reaches the
// model in one message, each in its own labelled section, so eight jobs that
// finished during one turn cost one turn of attention instead of eight.
func TestDeliveryBatchesOneSectionPerJob(t *testing.T) {
	l, jm, _ := newJobLoop(t, &model.Deterministic{CostPerCall: 1}, &env.Local{Timeout: 5 * time.Second})
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("j%d", i)
		seedEndedJob(t, jm, id, session.KindBash, strings.Repeat(fmt.Sprintf("body %d\n", i), 5), i)
	}
	text := wakeText(t, l)
	if !strings.HasPrefix(text, "3 background jobs ended while you were working on something else.") {
		t.Errorf("a batch must say how many it holds:\n%s", text)
	}
	// One section per job, oldest first, each ruled and labelled with the id it
	// reports - so a section quoted back or clipped still names its job.
	for i := 1; i <= 3; i++ {
		want := wakeSection(fmt.Sprintf("j%d", i))
		if n := strings.Count(text, want); n != 1 {
			t.Errorf("separator for j%d appeared %d times, want 1:\n%s", i, n, text)
		}
		if !strings.Contains(text, fmt.Sprintf("job j%d ended: completed", i)) ||
			!strings.Contains(text, fmt.Sprintf("(exit %d)", i)) {
			t.Errorf("j%d is missing its record:\n%s", i, text)
		}
		if !strings.Contains(text, "5 lines, 35 bytes") {
			t.Errorf("j%d is missing its counts:\n%s", i, text)
		}
	}
	if i := strings.Index(text, "job j1 ended"); i > strings.Index(text, "job j2 ended") ||
		strings.Index(text, "job j2 ended") > strings.Index(text, "job j3 ended") {
		t.Errorf("sections must arrive oldest first:\n%s", text)
	}
	// One message, not three: the batch is the whole point.
	if n := strings.Count(text, "ended while you were working"); n != 1 {
		t.Errorf("header appeared %d times, want 1", n)
	}
	// And the batch stays a record: three five-line logs must not drag three
	// bodies in with them.
	if strings.Contains(text, "body 2") {
		t.Errorf("a batched delivery inlined output:\n%s", text)
	}
	if len(text) > 4000 {
		t.Errorf("a three-job batch should be a digest, was %d bytes:\n%s", len(text), text)
	}
	// The debt is cleared for the whole batch at once.
	if n := jm.OwedDeliveries(); n != 0 {
		t.Errorf("after a batched delivery, owed = %d", n)
	}
}

// A job that ended without ever writing a file is the one case where "read it
// with:" would be a lie, so the record says there is nothing and does not
// pretend the counts came from somewhere.
func TestDeliveryHandlesAMissingOutputFile(t *testing.T) {
	l, jm, _ := newJobLoop(t, &model.Deterministic{CostPerCall: 1}, &env.Local{Timeout: 5 * time.Second})
	row, err := jm.Session.CreateJob(session.JobRow{ID: "j1", Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err := jm.Session.FinishJob(row.ID, session.JobCompleted, &code, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(row.OutputPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := jm.Session.RecordJobEvent(session.JobEventRow{
		JobID: row.ID, MonitorID: "exit", Reason: session.JobCompleted, ExitCode: &code,
	}); err != nil {
		t.Fatal(err)
	}
	text := wakeText(t, l)
	for _, want := range []string{"no file at that path", "nothing to read yet"} {
		if !strings.Contains(text, want) {
			t.Errorf("delivery missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"cat ", "tail -n", "read it with"} {
		if strings.Contains(text, bad) {
			t.Errorf("the delivery tells the model to read a file that does not exist (%q):\n%s", bad, text)
		}
	}
}

// The counts are a scan, so they have to be right on the cases where a scan is
// not trivially the file length: a final line with no newline, a file larger
// than the read buffer, and a path that is not a readable file at all.
func TestJobOutputStatCounts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	big := strings.Repeat("x\n", 200_000) // 400 KB, past the 256 KB chunk
	if n := len(big); n < 256<<10 {
		t.Fatalf("the fixture is not larger than the scan buffer: %d", n)
	}
	cases := []struct {
		name          string
		path          string
		wantLines     int64
		wantBytes     int64
		wantExists    bool
		wantShortness bool
	}{
		{"empty file", write("empty.log", ""), 0, 0, true, false},
		{"one full line", write("one.log", "hello\n"), 1, 6, true, false},
		{"no trailing newline counts its last line", write("partial.log", "a\nb"), 2, 3, true, false},
		{"crlf lines count once", write("crlf.log", "a\r\nb\r\n"), 2, 6, true, false},
		{"multi-chunk scan", write("big.log", big), 200_000, int64(len(big)), true, false},
		{"missing path", filepath.Join(dir, "nope.log"), 0, 0, false, false},
		{"empty path", "", 0, 0, false, false},
		{"a directory opens but cannot be read", dir, 0, 0, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jobOutputStat(tc.path)
			if got.Exists != tc.wantExists {
				t.Errorf("Exists = %v, want %v", got.Exists, tc.wantExists)
			}
			if got.Lines != tc.wantLines {
				t.Errorf("Lines = %d, want %d", got.Lines, tc.wantLines)
			}
			if got.Bytes != tc.wantBytes {
				t.Errorf("Bytes = %d, want %d", got.Bytes, tc.wantBytes)
			}
			if (got.Short != nil) != tc.wantShortness {
				t.Errorf("Short = %v, want its presence to be %v", got.Short, tc.wantShortness)
			}
			if tc.wantExists && got.Age == "" {
				t.Error("Age is empty for a file that exists")
			}
		})
	}
}

// The guidance is the model's only way into the file now, so it must not
// overstate what it is pointing at: an empty file is empty, a missing file has
// nothing to read, and a count that stopped partway is not a size.
func TestReadGuidanceQualifiesWhatItCannotProve(t *testing.T) {
	j := &session.JobRow{ID: "j1", Kind: session.KindBash, OutputPath: "/tmp/j1.log"}
	empty := jobReadGuidance(j, jobOutputInfo{Exists: true, Age: "1s"})
	if !strings.Contains(empty, "is empty") {
		t.Errorf("an empty file should be called empty: %q", empty)
	}
	missing := jobReadGuidance(j, jobOutputInfo{})
	if !strings.Contains(missing, "nothing to read yet") || strings.Contains(missing, "read it with") {
		t.Errorf("a missing file must not be offered for reading: %q", missing)
	}
	short := jobReadGuidance(j, jobOutputInfo{Exists: true, Short: errors.New("is a directory"), Age: "1s"})
	if !strings.Contains(short, "could not be counted") {
		t.Errorf("a truncated scan must say so: %q", short)
	}
	if strings.Contains(short, "is empty") {
		t.Errorf("an unreadable file is not an empty one: %q", short)
	}
}
