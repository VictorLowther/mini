package ui

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2/widget"
)

// collect runs scanBlocks and names each completed block: fenced as
// "FENCE:<info>(<lines>)", markdown as its trimmed source. Input is
// newline-padded like mdBlocks: the scanner only trusts complete lines.
func collect(src string, final bool) []string {
	if !strings.HasSuffix(src, "\n") {
		src += "\n"
	}
	var got []string
	scanBlocks(src, 0, 0, false, "", final, func(raw string, isFence bool, info string, lines []string) {
		if isFence {
			got = append(got, "FENCE:"+info+"("+strings.Join(lines, "/")+")")
		} else {
			got = append(got, raw)
		}
	})
	return got
}

func TestScanBlocksSplits(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		final bool
		want  []string
	}{
		{"blank line separates", "para one\n\npara two\n", false, []string{"para one"}},
		{"final flushes the open block", "only paragraph", true, []string{"only paragraph"}},
		{"setext stays whole", "Title\n===\n", true, []string{"Title\n==="}},
		{"fence interrupts a paragraph", "text\n```go\nx := 1\n```", true,
			[]string{"text", "FENCE:go(x := 1)"}},
		{"blank lines inside a fence are content", "```\na\n\nb\n```", true, []string{"FENCE:(a//b)"}},
		{"partial fence marker is not a fence", "a\n``", true, []string{"a\n``"}},
		{"backticks in info disqualify", "```a`b\ncode\n```", true, []string{"```a`b\ncode"}}, // the bare trailing ``` opens a new, unclosed fence
		{"longer fence closes", "```\nx\n````", true, []string{"FENCE:(x)"}},
		{"tilde fence", "~~~\nx\n~~~", true, []string{"FENCE:(x)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collect(c.src, c.final)
			if len(got) != len(c.want) {
				t.Fatalf("blocks = %q, want %q", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("block %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// An open fence survives the pass: no block closes, the state is returned,
// and the preview repair is the caller's job.
func TestScanBlocksFenceStaysOpen(t *testing.T) {
	var blocks []string
	cur, open, inFence, fence := scanBlocks("intro\n\n```go\nx := 1\n", 0, 0, false, "", false,
		func(raw string, isFence bool, info string, lines []string) { blocks = append(blocks, raw) })
	if len(blocks) != 1 || blocks[0] != "intro" {
		t.Fatalf("blocks = %q, want just the closed paragraph", blocks)
	}
	if !inFence || fence != "```" {
		t.Errorf("state = (%v, %q), want fence open", inFence, fence)
	}
	if open != len("intro\n\n") || cur != len("intro\n\n```go\nx := 1\n") {
		t.Errorf("cursor/open = %d/%d, want tail at the fence line", cur, open)
	}
}

func TestCodeCardFolding(t *testing.T) {
	long := codeCard("py", make([]string, 12)).(*widget.Accordion)
	if long.Items[0].Open {
		t.Error("a 12-line block must start folded")
	}
	if want := "py · 12 lines"; long.Items[0].Title != want {
		t.Errorf("title = %q, want %q", long.Items[0].Title, want)
	}
	short := codeCard("", []string{"a"}).(*widget.Accordion)
	if !short.Items[0].Open {
		t.Error("a one-line block must start open")
	}
	if want := "code · 1 line"; short.Items[0].Title != want {
		t.Errorf("title = %q, want %q", short.Items[0].Title, want)
	}
}

// mdBlocks is the committed path: every block of finished text renders,
// including the final open-at-eof one.
func TestMDBlocksRendersEveryBlock(t *testing.T) {
	blocks := mdBlocks("Intro **bold**.\n\n```go\nx := 1\n```\n\nTrailing text.")
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want paragraph, code card, paragraph", len(blocks))
	}
	if _, ok := blocks[1].(*widget.Accordion); !ok {
		t.Errorf("fenced block = %T, want the folding accordion", blocks[1])
	}
	if _, ok := blocks[0].(*widget.RichText); !ok {
		t.Errorf("markdown block = %T, want RichText", blocks[0])
	}
}
