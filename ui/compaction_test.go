package ui

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"

	"github.com/VictorLowther/mini/model"
)

func TestMessageBlockCompaction(t *testing.T) {
	m := model.Message{
		"role":    model.RoleCompaction,
		"content": "## Goal\nkeep going",
		"extra": map[string]any{
			"kept_n":           2,
			"tokens_before":    468400,
			"tokens_after":     181000,
			"compaction_model": "qwen-local",
		},
	}
	b, show, taskNote := messageBlock(m, "hdr", "task", func() {}, nil)
	if !show || taskNote {
		t.Fatalf("show=%v taskNote=%v, want the row displayed", show, taskNote)
	}
	if b.kind != "compaction" {
		t.Errorf("kind = %q, want compaction", b.kind)
	}
	if b.text != "## Goal\nkeep going" {
		t.Errorf("text = %q, want the summary verbatim", b.text)
	}
	for _, want := range []string{"context compacted", "468k", "181k", "qwen-local"} {
		if !strings.Contains(b.title, want) {
			t.Errorf("title %q lacks %q", b.title, want)
		}
	}
}

// The card exists so the visual transcript never restarts at the boundary:
// one titled markdown card in the ordinary scroll, not a special region.
func TestBlockCardsCompaction(t *testing.T) {
	test.NewApp()
	cards := blockCards(block{kind: "compaction", title: "context compacted", text: "## Goal\nkeep going"}, true)
	if len(cards) != 1 {
		t.Fatalf("%d cards, want 1", len(cards))
	}
	a, ok := cards[0].(*area)
	if !ok {
		t.Fatalf("card is %T, want an area card", cards[0])
	}
	if a.label != "context compacted" {
		t.Errorf("card label = %q", a.label)
	}
}

func TestCompactionTitleWithoutStats(t *testing.T) {
	got := compactionTitle(map[string]any{})
	if got != "context compacted" {
		t.Errorf("title = %q, want the bare form when no counts are recorded", got)
	}
}
