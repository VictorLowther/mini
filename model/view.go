package model

import (
	"bytes"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

// The two bounds of the model's output window, mirrored from what the
// observation template promises: the last MaxViewLines lines, then at most
// MaxViewChars runes. Both bounds are suffixes of the output, so the view is
// always one contiguous tail of the result file and a single byte offset
// describes it.
const (
	MaxViewLines = 100
	MaxViewChars = 10000
	// MaxViewBytes bounds a view read: MaxViewChars runes fit in at most four
	// bytes each.
	MaxViewBytes = 4 * MaxViewChars
	// tailWindowBytes is the first window ViewOf reads; it doubles until the
	// line bound is visible.
	tailWindowBytes = 1 << 20
)

// ViewStart is the one piece of tail arithmetic in the program. Given the bytes
// read from a result file it returns the offset within them where the model's
// view starts — the last MaxViewLines lines, then pushed forward to keep at
// most MaxViewChars runes aligned to the next line boundary — and how many
// lines come before it. A whole file that fits both bounds gives offset 0.
func ViewStart(data []byte) (offset int, elidedLines int) {
	if nl := bytes.Count(data, []byte{'\n'}); nl > MaxViewLines {
		// Keep the last MaxViewLines newline-terminated lines: advance past
		// the newline that closes the last elided line.
		elidedLines = nl - MaxViewLines
		at := 0
		for i := elidedLines; i > 0; i-- {
			at += bytes.IndexByte(data[at:], '\n') + 1
		}
		offset = at
	}
	if utf8.RuneCount(data[offset:]) > MaxViewChars {
		// Walk back from the end of data until MaxViewChars runes are used,
		// then advance to just past the next newline (line-aligned when a
		// newline follows, never a split rune either way).
		p, used := len(data), 0
		for p > offset && used < MaxViewChars {
			_, size := utf8.DecodeLastRune(data[:p])
			p -= size
			used++
		}
		offset = p
		if i := bytes.IndexByte(data[offset:], '\n'); i >= 0 {
			offset += i + 1
		}
	}
	return offset, elidedLines
}

// ViewOf computes the model's view once for a closed result file and returns
// the view text, the absolute byte offset it starts at, and the exact elided
// line count. The loop calls it once per finished command and stores the
// numbers; no later reader repeats this.
func ViewOf(path string) (view string, offset int64, elidedLines int, err error) {
	if path == "" {
		return "", 0, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, 0, err
	}
	size := info.Size()
	window := min(size, int64(tailWindowBytes))
	var (
		base int64
		data []byte
		off  int
	)
	for {
		base = size - window
		data = make([]byte, window)
		if _, err = f.ReadAt(data, base); err != nil && !errors.Is(err, io.EOF) {
			return "", 0, 0, err
		}
		off, elidedLines = ViewStart(data)
		// The window shows fewer lines than the bound: the true 100-line
		// boundary lies before the window, so the elided count would be
		// wrong. Double the window; base == 0 terminates with the whole file.
		if base > 0 && bytes.Count(data, []byte{'\n'}) < MaxViewLines {
			window = min(window*2, size)
			continue
		}
		break
	}
	offset = base + int64(off)
	view = string(data[off:])
	if base > 0 {
		// The lines in front of the window, counted with a buffered pass that
		// keeps nothing.
		pre, err := countNewlines(f, base)
		if err != nil {
			return "", 0, 0, err
		}
		elidedLines += pre
	}
	return view, offset, elidedLines, nil
}

// countNewlines is a 32 KiB buffered pass over [0, base) that keeps nothing.
func countNewlines(f *os.File, base int64) (int, error) {
	buf := make([]byte, 32*1024)
	total := 0
	for off := int64(0); off < base; {
		want := min(int64(len(buf)), base-off)
		n, err := f.ReadAt(buf[:want], off)
		total += bytes.Count(buf[:n], []byte{'\n'})
		off += int64(n)
		if int64(n) < want {
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
			break
		}
	}
	return total, nil
}

// ReadView is what every later reader uses: the stored view, no arithmetic. It
// reads [offset, min(end, offset+MaxViewBytes)), where end is the recorded
// output_bytes — the file's size when the view was computed — so a view never
// chases bytes a later writer appended; a stale or zero end falls back to the
// current size. An empty path is not an error: a command that never ran has no
// output anywhere.
func ReadView(path string, offset, end int64) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	if end <= 0 || end > info.Size() {
		end = info.Size()
	}
	data := make([]byte, min(end-offset, int64(MaxViewBytes)))
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(data), nil
}

// IsToolResult reports whether a message is an observation: an extra map
// carrying both command and returncode. Padded "action was not executed"
// results qualify; assistant, user, interrupt and exit messages never do,
// because none of them sets both keys.
func IsToolResult(m Message) bool {
	extra, ok := m["extra"].(map[string]any)
	if !ok {
		return false
	}
	_, hasCommand := extra["command"]
	_, hasReturncode := extra["returncode"]
	return hasCommand && hasReturncode
}
