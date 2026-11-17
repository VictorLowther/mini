package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/env"
	"github.com/VictorLowther/mini/model"
)

// memSettings is the settings table as a map: the seam is two methods, and a
// GUI test that needed real SQLite would be testing the driver instead of the
// stickiness.
type memSettings struct {
	rows       map[string]string
	writeErr   error
	readCalled []string
}

func newMemSettings(rows ...string) *memSettings {
	s := &memSettings{rows: map[string]string{}}
	for i := 0; i+1 < len(rows); i += 2 {
		s.rows[rows[i]] = rows[i+1]
	}
	return s
}

func (s *memSettings) Setting(key string) (string, bool, error) {
	s.readCalled = append(s.readCalled, key)
	v, ok := s.rows[key]
	return v, ok, nil
}

func (s *memSettings) SetSetting(key, value string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.rows[key] = value
	return nil
}

// TestThinkingShownStoredValueBeatsTheDefault is the read side: a stored answer
// wins, and anything unreadable is absence rather than a false.
func TestThinkingShownStoredValueBeatsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []string
		want bool
	}{
		{"nothing stored opens collapsed", nil, defaultThinkingShown},
		{"stored false stays false", []string{settingThinkingShown, "false"}, false},
		{"stored true is honoured", []string{settingThinkingShown, "true"}, true},
		{"strconv's other vocabularies", []string{settingThinkingShown, "1"}, true},
		{"unparseable is absent", []string{settingThinkingShown, "sometimes"}, defaultThinkingShown},
		{"empty is absent", []string{settingThinkingShown, ""}, defaultThinkingShown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadThinkingShown(newMemSettings(tc.rows...)); got != tc.want {
				t.Errorf("shown = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPreferencesAreWrittenOnlyWhenExpressed: mini stores what a human chose,
// not what it defaulted to. Writing the default on first launch would freeze it
// - the next version's better opening size or thinking default would arrive and
// immediately be overridden by a row nobody remembers asking for.
func TestPreferencesAreWrittenOnlyWhenExpressed(t *testing.T) {
	s := newMemSettings()
	saveThinkingShown(s, defaultThinkingShown)
	saveWindowSize(s, defaultWindowSize)
	if len(s.rows) != 0 {
		t.Errorf("the untouched defaults were pinned to the store: %v", s.rows)
	}

	// Once a preference exists, the default is a deliberate answer and lands -
	// otherwise shrinking the window back, or hiding thinking again, would be
	// unrememberable.
	saveThinkingShown(s, true)
	saveWindowSize(s, fyne.NewSize(1440, 900))
	saveThinkingShown(s, false)
	saveWindowSize(s, defaultWindowSize)
	for _, want := range []struct{ key, value string }{
		{settingThinkingShown, "false"},
		{settingWindowWidth, "960.00"},
		{settingWindowHeight, "720.00"},
	} {
		if got := s.rows[want.key]; got != want.value {
			t.Errorf("%s = %q, want %q (all rows: %v)", want.key, got, want.value, s.rows)
		}
	}
	if got := loadWindowSize(s); !sameSize(got, defaultWindowSize) {
		t.Errorf("restored = %v, want the deliberately-chosen default", got)
	}

	// Sub-pixel drift from the display scale is not a resize worth storing.
	fresh := newMemSettings()
	saveWindowSize(fresh, fyne.NewSize(defaultWindowSize.Width+0.0001, defaultWindowSize.Height-0.0002))
	if len(fresh.rows) != 0 {
		t.Errorf("float drift invented a preference: %v", fresh.rows)
	}
}

func TestWindowSizeRoundTripAndGuards(t *testing.T) {
	s := newMemSettings()
	if got := loadWindowSize(s); got != defaultWindowSize {
		t.Errorf("absent size = %v, want the default %v", got, defaultWindowSize)
	}
	saveWindowSize(s, fyne.NewSize(1440.5, 900.25))
	if got := loadWindowSize(s); got.Width != 1440.5 || got.Height != 900.25 {
		t.Errorf("restored size = %v", got)
	}

	// Half a size is as unusable as none: both sides were stored together or
	// the pair is not the user's.
	half := newMemSettings(settingWindowWidth, "1200")
	if got := loadWindowSize(half); got != defaultWindowSize {
		t.Errorf("width without height = %v, want the default", got)
	}

	// Values a window cannot be: below the floor, above the ceiling, or zero.
	for _, bad := range [][2]float32{{20, 900}, {1440, 5}, {0, 0}, {999999, 999999}, {-1000, -1000}} {
		before := newMemSettings(settingWindowWidth, "1000", settingWindowHeight, "1000")
		saveWindowSize(before, fyne.NewSize(bad[0], bad[1]))
		if got := loadWindowSize(before); got.Width != 1000 {
			t.Errorf("saving %v erased the stored size (read %v)", bad, got)
		}
	}
	for _, bad := range []string{"", "wide", "-1", "1e999"} {
		s := newMemSettings(settingWindowWidth, bad, settingWindowHeight, bad)
		if got := loadWindowSize(s); got != defaultWindowSize {
			t.Errorf("stored %q restored %v, want the default", bad, got)
		}
	}
}

// TestGUIRestoresThinkingAndSize is the whole point of the feature: a window
// built from stored state opens thinking-shown at the stored size.
func TestGUIRestoresThinkingAndSize(t *testing.T) {
	c := newConfig("confirm")
	l := &agent.Loop{Cfg: c, Model: outputs(t, "echo hi"), Env: &env.Local{}}
	s := newMemSettings(
		settingThinkingShown, "true",
		settingWindowWidth, "1280",
		settingWindowHeight, "800",
	)
	g := NewGUI(GUIOptions{
		Post: newTestQueue(t).add,
		Cfg:  c, Loop: l, ConfigName: "mini", App: test.NewApp(), Settings: s,
		Model: &model.Sushi{Model: "m", BaseURL: "http://127.0.0.1:1"},
	})
	if !g.showReasoning() {
		t.Error("the stored thinking preference was ignored")
	}
	if got := g.sh.window.Canvas().Size(); got.Width != 1280 || got.Height != 800 {
		t.Errorf("window opened at %v, want the stored 1280x800", got)
	}

	// The toggle writes back through the same key, so quitting and resuming -
	// or opening --print - sees it.
	if on := g.ToggleReasoning(); on {
		t.Error("toggle should have hidden thinking")
	}
	if s.rows[settingThinkingShown] != "false" {
		t.Errorf("stored = %q, want false", s.rows[settingThinkingShown])
	}
	// And quitting saves the size the window ended at.
	s.rows = map[string]string{}
	g.sh.quit()
	if s.rows[settingWindowWidth] != "1280.00" || s.rows[settingWindowHeight] != "800.00" {
		t.Errorf("quit stored %+v, want the window's 1280x800", s.rows)
	}
}

// TestGUINoStore: the GUI must still open with no database behind it.
func TestGUINoStore(t *testing.T) {
	c := newConfig("confirm")
	g := NewGUI(GUIOptions{Cfg: c, ConfigName: "mini", App: test.NewApp(), Post: newTestQueue(t).add})
	if g.showReasoning() {
		t.Error("a window with no store opens collapsed")
	}
	if got := g.sh.window.Canvas().Size(); got != defaultWindowSize {
		t.Errorf("window = %v, want the default %v", got, defaultWindowSize)
	}
	g.ToggleReasoning() // must not panic on a nil store
}

// TestSettingsWriteFailureIsOnlyALog: a preference that cannot be saved costs
// the next launch one reset. It must not cost this one a toggle or a quit.
func TestSettingsWriteFailureIsOnlyALog(t *testing.T) {
	s := newMemSettings()
	s.writeErr = errors.New("database is locked")
	c := newConfig("confirm")
	g := NewGUI(GUIOptions{Cfg: c, ConfigName: "mini", App: test.NewApp(), Settings: s, Post: newTestQueue(t).add})
	if on := g.ToggleReasoning(); !on {
		t.Error("a failed write must not lie about the in-memory state")
	}
	g.sh.quit()
	if len(s.rows) != 0 {
		t.Errorf("the failed write invented rows: %v", s.rows)
	}
}

// TestPrintShowsStoredThinkingOfAReplayedTurn is the regression: --print gets
// the system and user turns before any reply, and every one of them ran
// through closeReasoning, which used to mark the reasoning as already shown.
// The only headless thinking path was therefore permanently gated off, stored
// preference or not.
func TestPrintShowsStoredThinkingOfAReplayedTurn(t *testing.T) {
	c := newConfig("confirm")
	out := &lockedBuffer{}
	p := NewPrint(out, strings.NewReader(""), c, newMemSettings(settingThinkingShown, "true"))
	for _, m := range []model.Message{
		{"role": "system", "content": "sys prompt"},
		{"role": "user", "content": "the task"},
		{"role": "assistant", "content": "a listing", "reasoning_content": "The user wants a listing."},
	} {
		p.Message(m)
	}
	got := out.String()
	if !strings.Contains(got, "[thinking] The user wants a listing.") {
		t.Errorf("the turns before the reply swallowed the thinking flag:\n%s", got)
	}
	if strings.Contains(got, "sys prompt") {
		t.Errorf("prompt plumbing reached the transcript:\n%s", got)
	}
}

// TestPrintThinkingToggle: /t is sticky headlessly too, and it gates the
// printed thinking block either way.
func TestPrintThinkingToggle(t *testing.T) {
	c := newConfig("confirm")
	s := newMemSettings()
	out := &lockedBuffer{}
	p := NewPrint(out, strings.NewReader("/t\n/quit\n"), c, s)
	p.Terminal = true

	if p.showThinking {
		t.Error("--print with nothing stored opens collapsed, like the window")
	}
	// While collapsed, an assistant turn's thinking stays out of the
	// transcript - this is the headless front end's only thinking path, since
	// nothing streams into a pipe.
	p.Message(model.Message{"role": "assistant", "content": "done",
		"reasoning_content": "pondering the meaning of ls"})
	if strings.Contains(out.String(), "pondering") {
		t.Errorf("hidden thinking was printed anyway: %q", out.String())
	}

	if _, err := p.Ask(context.Background(), Question{Kind: AskNewTask, Title: "ready"}); err != nil {
		t.Fatal(err)
	}
	if got := s.rows[settingThinkingShown]; got != "true" {
		t.Errorf("stored = %q, want true", got)
	}
	if !strings.Contains(out.String(), "thinking shown") {
		t.Errorf("output = %q", out.String())
	}
	// /t is an affordance, not an answer: the prompt re-asks, and the quit
	// that follows is what ends it.
	if got, want := len(s.readCalled) > 0, true; got != want {
		t.Error("the store was never consulted")
	}
	p.Message(model.Message{"role": "assistant", "content": "done again",
		"reasoning_content": "pondering the meaning of ls"})
	if !strings.Contains(out.String(), "[thinking] pondering the meaning of ls") {
		t.Errorf("shown thinking never appeared:\n%s", out.String())
	}
	reopened := NewPrint(&lockedBuffer{}, strings.NewReader(""), c,
		newMemSettings(settingThinkingShown, s.rows[settingThinkingShown]))
	if !reopened.showThinking {
		t.Error("the reopened headless front end forgot the stored toggle")
	}
}
