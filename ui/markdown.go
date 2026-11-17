package ui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
)

// Incremental markdown for streaming and committed text.
//
// The engine is a line scanner, not a second parser: every closed block is
// handed to fyne's own markdown RichText (or rendered as a foldable code
// card) exactly once, and only the open tail is re-parsed per flush. The
// scanner tracks the two block facts mini must own: fenced code (which can
// interrupt a paragraph and whose interior never ends a block) and blank
// lines outside fences (the one separator CommonMark respects at every
// level). Setext headings and tight lists never split, because they contain
// no blank line. A loose list can arrive as several list chunks - a
// numbering quirk we accept for a parser that never re-reads a closed block.
//
// codeFoldAfter: a fenced block up to this many lines renders open; longer
// ones fold into their accordion header, so pasted code never buries the
// thinking or answer around it. Expanded long code is capped and scrolled
// by the transcript grid inside.

const codeFoldAfter = 6

// scanBlocks walks src line by line from (pos, chunkStart, inFence, fence)
// and calls fn for each block that COMPLETES during this pass. It returns
// the scan cursor, the start of the still-open block (the live tail begins
// here, NOT at the cursor - a paragraph may span many scanned lines before
// its first blank), and the fence state, so a live stream resumes exactly
// here on the next chunk; bytes after the last newline are deliberately
// unscanned - a half-typed "“" must not become a fence. With final set, the
// open block is emitted too: complete text always renders completely.
func scanBlocks(src string, pos, chunkStart int, inFence bool, fence string, final bool, fn func(raw string, isFence bool, info string, lines []string)) (cursor, tailStart int, _ bool, _ string) {
	i := pos
	closeMarkdown := func(end int) {
		if raw := strings.TrimSpace(src[chunkStart:end]); raw != "" {
			fn(raw, false, "", nil)
		}
		chunkStart = end
	}
	for i < len(src) {
		e := strings.IndexByte(src[i:], '\n')
		if e < 0 {
			break
		}
		line := src[i : i+e]
		trimmed := strings.TrimLeft(line, " \t")
		switch {
		case inFence:
			if fenceCloses(trimmed, fence) {
				inFence = false
				// The block is rebuilt from src, never accumulated: a live
				// stream resumes mid-fence, and only the text knows what
				// the fence held. Indentation deeper than the opening
				// fence's is stripped; column-zero fences - all mini's
				// models write - pass through untouched.
				openLine, rest, _ := strings.Cut(src[chunkStart:i], "\n")
				info := ""
				ot := strings.TrimSpace(openLine)
				if mark := fenceOpen(ot); mark != "" {
					info = strings.TrimSpace(ot[len(mark):])
				}
				rest = strings.TrimSuffix(rest, "\n")
				var ls []string
				if rest != "" {
					for _, l := range strings.Split(rest, "\n") {
						ls = append(ls, strings.TrimPrefix(l, strings.Repeat(" ", (len(openLine)-len(strings.TrimLeft(openLine, " \t")))%4)))
					}
				}
				fn(strings.Join(ls, "\n"), true, info, ls)
				chunkStart = i + e + 1
			}
		case fenceOpen(trimmed) != "" && len(line)-len(trimmed) < 4:
			closeMarkdown(i)
			inFence = true
			fence = fenceOpen(trimmed)
		case strings.TrimSpace(line) == "":
			closeMarkdown(i + e + 1)
		}
		i += e + 1
	}
	if final && !inFence {
		closeMarkdown(len(src))
	}
	// final with an unclosed fence is malformed input (committed text whose
	// model never closed the block); the caller's final-flush renders it as
	// text rather than losing it.
	return i, chunkStart, inFence, fence
}

// fenceOpen returns the marker of an opening ``` or ~~~ line, or "".
func fenceOpen(trimmed string) string {
	if len(trimmed) < 3 {
		return ""
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return ""
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return ""
	}
	// A backtick fence's info string may not contain backticks; tildes may.
	if c == '`' && strings.Contains(trimmed[n:], "`") {
		return ""
	}
	return strings.Repeat(string(c), n)
}

// fenceCloses reports whether trimmed is a closing line for the open fence.
func fenceCloses(trimmed, fence string) bool {
	if trimmed == "" {
		return false
	}
	c := fence[0]
	if trimmed[0] != c {
		return false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	return n >= len(fence) && strings.TrimSpace(trimmed[n:]) == ""
}

// mdBlocks renders a complete markdown fragment as one widget per top-level
// block. Fenced code becomes a foldable card; everything else is one
// markdown RichText, so each block's wrapping, lists, tables and emphasis
// are fyne's parser's problem, once.
func mdBlocks(src string) []fyne.CanvasObject {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	var out []fyne.CanvasObject
	scan := src
	if !strings.HasSuffix(scan, "\n") {
		scan += "\n"
	}
	_, openAt, inFence, _ := scanBlocks(scan, 0, 0, false, "", true, func(raw string, isFence bool, info string, lines []string) {
		if isFence {
			out = append(out, codeCard(info, lines))
		} else {
			out = append(out, mdOrLabel(raw))
		}
	})
	if inFence {
		// A committed turn that ends inside an unclosed fence: render the
		// rest as markdown rather than swallowing the block.
		if raw := strings.TrimSpace(scan[openAt:]); raw != "" {
			out = append(out, mdOrLabel(raw))
		}
	}
	return out
}

// codeCard is a fenced block as an accordion titled by language and size:
// short blocks open, long ones fold so a paste cannot shove the conversation
// off screen. The body is the transcript grid, so an open long block still
// caps at ten scrolled lines rather than re-growing the wall.
func codeCard(info string, lines []string) fyne.CanvasObject {
	title := "code"
	if info != "" {
		title = info
	}
	noun := "lines"
	if len(lines) == 1 {
		noun = "line"
	}
	title += fmt.Sprintf(" · %d %s", len(lines), noun)
	acc := widget.NewAccordion(widget.NewAccordionItem(title, transcriptGrid(lines)))
	if len(lines) <= codeFoldAfter {
		acc.Open(0)
	}
	return acc
}

// alignTree sets the text alignment of every RichText under o, which is how
// right-justified user entries keep their chat-app look now that a block of
// text is a container of paragraphs rather than one RichText.
func alignTree(o fyne.CanvasObject, align fyne.TextAlign) {
	if align == fyne.TextAlignLeading {
		return
	}
	switch t := o.(type) {
	case *widget.RichText:
		alignRT(t, align)
	case *fyne.Container:
		for _, c := range t.Objects {
			alignTree(c, align)
		}
	}
}

// mdOrLabel picks the body for a prose block: a selectable Label unless the
// block contains markdown that would visibly change the rendering.
//
// The test is deliberately conservative - an underscore or an asterisk anywhere
// sends the block to the markdown path, even though plenty of such blocks would
// have rendered identically - because the two ways to get this wrong are not
// equal. Guess "plain" and a heading or an emphasis silently loses its
// formatting, which reads as the renderer being broken; guess "markdown" and the
// cost is that one styled paragraph cannot be drag-selected, while the card's
// whole text is still reachable from the code path and the rest of the turn.
// Getting text *out* matters more than one paragraph of italics.
func mdOrLabel(raw string) fyne.CanvasObject {
	if mdShowsFormatting(raw) {
		return markdownRT(raw)
	}
	// Importance(0) is the normal weight: prose at the default colour.
	return selectableLabel(raw, 0)
}

// mdShowsFormatting reports whether a block carries markdown a Label could not
// reproduce. Line prefixes are checked per line because a "# " mid-sentence is
// text, and only a marker at the start of a line is a heading.
func mdShowsFormatting(raw string) bool {
	for _, m := range []string{"*", "_", "`", "~~", "](", "<http", "<https"} {
		if strings.Contains(raw, m) {
			return true
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimLeft(line, " \t")
		switch {
		case strings.HasPrefix(t, "#"), strings.HasPrefix(t, ">"),
			strings.HasPrefix(t, "- "), strings.HasPrefix(t, "+ "),
			strings.HasPrefix(t, "* "):
			return true
		}
		if len(t) > 1 && t[0] >= '0' && t[0] <= '9' {
			// "1." / "2)" ordered list markers.
			i := 1
			for i < len(t) && t[i] >= '0' && t[i] <= '9' {
				i++
			}
			if i < len(t) && (t[i] == '.' || t[i] == ')') {
				return true
			}
		}
	}
	return false
}
