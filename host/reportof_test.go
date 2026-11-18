package host

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A-9's other half: the short reason a parent reads was cut at 200 *bytes*, so a
// report containing an accented path, CJK prose or an emoji ended in a dangling
// prefix that arrived as U+FFFD.
func TestReportOfCutsOnRunes(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("漢", 300),        // 3 bytes per rune
		strings.Repeat("é", 300),        // 2 bytes per rune
		strings.Repeat("🙂", 300),        // 4 bytes per rune
		strings.Repeat("a", 199) + "漢字", // straddles the 200th byte exactly
	} {
		got := reportOf(name)
		if !utf8.ValidString(got) {
			t.Errorf("report is not valid UTF-8: %q", got)
		}
		if strings.ContainsRune(got, 0xFFFD) {
			t.Errorf("a split rune leaked U+FFFD into the job reason: %q", got)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("an over-long report must be marked as cut: %q", got)
		}
		if n := utf8.RuneCountInString(strings.TrimSuffix(got, "...")); n != 200 {
			t.Errorf("kept %d runes, want 200", n)
		}
	}
	// The unchanged fast path still passes short text through.
	if got := reportOf("done"); got != "done" {
		t.Errorf("short report = %q", got)
	}
}
