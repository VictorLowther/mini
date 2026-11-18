package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A-9: the read budget is in bytes and the job log is UTF-8, so the cut can land
// inside a multi-byte character. Half a rune reaches the model as U+FFFD, and for
// a report ending in CJK that is up to the last bytes of the whole body.
func TestReadJobBodyNeverSplitsARune(t *testing.T) {
	cases := []struct {
		name    string
		content string
		n       int
	}{
		{"two-byte rune straddles the cut", strings.Repeat("a", 9) + "éxyz", 10},
		{"three-byte rune straddles the cut", strings.Repeat("a", 9) + "漢字", 11},
		{"four-byte rune straddles the cut", strings.Repeat("a", 9) + "🙂ok", 12},
		{"cut lands on a boundary", strings.Repeat("a", 9) + "bcd", 10},
		{"ascii only", strings.Repeat("x", 50), 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "job.log")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := readJobBody(path, tc.n)
			if !ok {
				t.Fatal("readJobBody declined to read a file that exists")
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
			if strings.ContainsRune(got, 0xFFFD) {
				t.Errorf("a split rune leaked U+FFFD into the model's view: %q", got)
			}
			if len(got) > tc.n+len("\n(truncated)") {
				t.Errorf("the byte budget was exceeded: %d bytes for n=%d", len(got), tc.n)
			}
		})
	}
}

// ASCII must not lose its last character to the back-off.
func TestReadJobBodyKeepsTheFullBudgetForASCII(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := readJobBody(path, 20)
	if !strings.HasPrefix(got, strings.Repeat("z", 20)) {
		t.Errorf("ASCII cut lost bytes it was allowed to keep: %q", got)
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("a cut body must say so: %q", got)
	}
}
