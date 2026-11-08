package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestViewStart(t *testing.T) {
	t.Run("small file is the whole view", func(t *testing.T) {
		data := []byte(strings.Repeat("short line\n", 50))
		off, elided := ViewStart(data)
		if off != 0 || elided != 0 {
			t.Errorf("ViewStart(200-byte file) = (%d, %d), want (0, 0)", off, elided)
		}
	})
	t.Run("line bound bites first", func(t *testing.T) {
		data := []byte(strings.Repeat("0123456789\n", 5000))
		off, elided := ViewStart(data)
		if elided != 4900 {
			t.Errorf("elided = %d, want 4900", elided)
		}
		want := strings.Repeat("0123456789\n", 100)
		if string(data[off:]) != want {
			t.Errorf("view is not the last 100 lines")
		}
		if data[off] != '0' {
			t.Errorf("data[%d] = %q, want the first byte of a line", off, data[off])
		}
	})
	t.Run("char bound aligns to a line", func(t *testing.T) {
		// Two huge multibyte lines, then a small final line: the rune walk
		// back lands mid-line-2 and the view starts at the next boundary.
		data := []byte(strings.Repeat("日本語", 20000) + "\n" + strings.Repeat("日本語", 20000) + "\n" + "tail\n")
		off, _ := ViewStart(data)
		view := string(data[off:])
		if utf8.RuneCountInString(view) > MaxViewChars {
			t.Errorf("view is %d runes, want <= %d", utf8.RuneCountInString(view), MaxViewChars)
		}
		if !utf8.ValidString(view) {
			t.Error("view split a rune")
		}
		if view != "tail\n" {
			t.Errorf("view = %q…, want the next line boundary", view[:min(len(view), 20)])
		}
	})
	t.Run("char bound with no newline after keeps the rune window", func(t *testing.T) {
		data := []byte(strings.Repeat("é", 30000)) // no trailing newline
		off, _ := ViewStart(data)
		view := string(data[off:])
		if utf8.RuneCountInString(view) > MaxViewChars || !utf8.ValidString(view) {
			t.Errorf("view = %d runes valid=%v", utf8.RuneCountInString(view), utf8.ValidString(view))
		}
		if off == 0 {
			t.Error("30 000-rune single line must not pass unelided")
		}
	})
}

func TestViewOfAndReadView(t *testing.T) {
	writeFile := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "out.log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name string
		data string
	}{
		{"small", strings.Repeat("short line\n", 50)},
		{"line bound", strings.Repeat("0123456789\n", 5000)},
		{"char bound", strings.Repeat("日本語", 20000) + "\n" + strings.Repeat("日本語", 20000) + "\n"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, c.data)
			view, offset, _, err := ViewOf(path)
			if err != nil {
				t.Fatal(err)
			}
			wantOff, _ := ViewStart([]byte(c.data))
			if view != c.data[wantOff:] {
				t.Error("ViewOf disagrees with ViewStart on the whole file")
			}
			// The stored offset is authoritative: ReadView reproduces the
			// view exactly, which is all resume and replay ever do.
			again, err := ReadView(path, offset, int64(len(c.data)))
			if err != nil || again != view {
				t.Errorf("ReadView(path, %d) = (%q…, %v), want the stored view", offset, first(again, 40), err)
			}
		})
	}
	t.Run("3 MiB file counts elided lines exactly", func(t *testing.T) {
		var b strings.Builder
		for i := range 200_000 {
			b.WriteString("line ")
			b.WriteString(itoa(i))
			b.WriteByte('\n')
		}
		path := writeFile(t, b.String())
		view, _, elided, err := ViewOf(path)
		if err != nil {
			t.Fatal(err)
		}
		if elided != 199_900 {
			t.Errorf("elided = %d, want 199900", elided)
		}
		lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
		if len(lines) != MaxViewLines || lines[0] != "line 199900" || lines[99] != "line 199999" {
			t.Errorf("view is not the last 100 lines: %q … %q", first(lines[0], 20), first(lines[len(lines)-1], 20))
		}
	})
	t.Run("the stored offset does not chase appended output", func(t *testing.T) {
		original := strings.Repeat("0123456789\n", 5000)
		path := writeFile(t, original)
		view, offset, _, err := ViewOf(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(strings.Repeat("appended line\n", 500))
		f.Close()
		// The call row recorded output_bytes at completion; that is the end
		// the reader uses, so the view is frozen even if the file grows.
		later, err := ReadView(path, offset, int64(len(original)))
		if err != nil || later != view {
			t.Error("a resumed session must show what the model was shown at the time")
		}
	})
	t.Run("empty path is not an error", func(t *testing.T) {
		if v, err := ReadView("", 0, 0); err != nil || v != "" {
			t.Errorf("ReadView(\"\") = (%q, %v)", v, err)
		}
	})
	t.Run("missing path errors", func(t *testing.T) {
		if _, err := ReadView(filepath.Join(t.TempDir(), "gone.log"), 0, 0); err == nil {
			t.Error("missing file must be an error the observation surfaces")
		}
	})
}

func first(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
