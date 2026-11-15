package ui

import (
	"log/slog"
	"math"
	"strconv"

	"fyne.io/fyne/v2"
)

// Sticky interface state. Two things are tedious to set again every launch and
// neither is about the session: whether the model's thinking is displayed (a
// preference about this pair of eyes, acquired the first time a reasoning model
// buried the actual answer under a page of deliberation), and how big the
// window was (the transcript is a reading surface, and its width is chosen
// once and then never thought about again). Both go in the store's global
// settings table, which - unlike the config file - is machine state rather
// than run configuration: scripts and -c overrides must keep the config's
// meaning, and nobody wants `mini -p` to silently rewrite their config.
//
// Absence is not false, it is "nobody has said yet", and both front ends agree
// on what that means: collapsed, which is the window's long-standing opening
// state and was also all --print had to offer, since nothing there showed
// thinking until the replayed-turn branch in Print.Message was repaired. One
// rule with two faces. The first toggle anywhere records the answer and every
// front end obeys it from then on - and mini never writes a key for a
// preference nobody expressed, so a default stays a default instead of being
// frozen into the database on the first quit.
//
// Window *placement* is deliberately not remembered, and it is an API gap
// rather than a shortcut: Fyne 2.8's public Window interface has no position
// getter and no move callback (the driver's own position code is internal),
// and the one write-side call - desktop.Window.RequestPosition - documents
// that the request may be ignored (Wayland) and that its coordinates have no
// contract across monitors. So mini could apply a stored position but never
// learn where the window actually ended up, and a value saved on one display
// setup can park the window beyond an unplugged monitor with no way for the
// user to reach it. A remembered size costs nothing and always lands.

// Settings is the seam a front end reads sticky state through:
// *session.Store satisfies it structurally over the settings table. Like
// agent.Recorder, the shape keeps ui out of importing session, so a GUI can be
// built against no database at all (tests, and any future non-stored host).
type Settings interface {
	Setting(key string) (string, bool, error)
	SetSetting(key, value string) error
}

// The keys, named for what they mean rather than for which front end wrote
// them: the window and --print share one value per key, which is the point.
const (
	settingThinkingShown = "thinking_shown"
	settingWindowWidth   = "window_width"
	settingWindowHeight  = "window_height"
)

// defaultWindowSize is the first-launch window: the size mini has always
// opened at, and the fallback for a stored value that cannot be right.
var defaultWindowSize = fyne.NewSize(960, 720)

// A restored size has to be one a window could actually be. The floor is
// generous (below this the transcript is a slot for one line, which is a
// corrupted value rather than a preference); the ceiling stops a bad row from
// asking the compositor for a wall-sized window.
const (
	minWindowSide = 200
	maxWindowSide = 65536
)

// defaultThinkingShown is how mini opens with no stored opinion: collapsed.
const defaultThinkingShown = false

// loadThinkingShown is the sticky thinking toggle, or defaultThinkingShown when
// nothing has ever been recorded (or what was recorded no longer parses).
func loadThinkingShown(s Settings) bool {
	v, ok := prefBool(s, settingThinkingShown)
	if !ok {
		return defaultThinkingShown
	}
	return v
}

func saveThinkingShown(s Settings, shown bool) {
	if shown == defaultThinkingShown {
		// Collapsing is what a run does with nothing stored anyway, so writing
		// it here would only pin today's default past the day it changes. Once
		// a human has spoken, even the default is their answer.
		if _, stored := prefBool(s, settingThinkingShown); !stored {
			return
		}
	}
	putPref(s, settingThinkingShown, strconv.FormatBool(shown))
}

// loadWindowSize is the window's last saved content size, or
// defaultWindowSize. Both sides must have been stored and both must be usable:
// half a size is as much of a mystery as none, and applying one would put the
// transcript in a window the user did not ask for.
func loadWindowSize(s Settings) fyne.Size {
	w, wok := prefFloat(s, settingWindowWidth)
	h, hok := prefFloat(s, settingWindowHeight)
	if !wok || !hok || !sizedWithin(w) || !sizedWithin(h) {
		return defaultWindowSize
	}
	return fyne.NewSize(float32(w), float32(h))
}

func saveWindowSize(s Settings, size fyne.Size) {
	if !sizedWithin(float64(size.Width)) || !sizedWithin(float64(size.Height)) {
		return // pre-layout or nonsense: keep what is stored rather than erase it
	}
	// A size nobody chose is nothing to remember. Storing the default would
	// freeze this version's opening size into every future launch - a shipped
	// default that can never move again, for a user who never touched it - so
	// the key stays absent until the observed size differs from the default.
	// Once somebody has expressed a preference, even the default is a
	// deliberate answer and gets written.
	if sameSize(size, defaultWindowSize) {
		if _, stored := prefFloat(s, settingWindowWidth); !stored {
			return
		}
	}
	putPref(s, settingWindowWidth, strconv.FormatFloat(float64(size.Width), 'f', 2, 32))
	putPref(s, settingWindowHeight, strconv.FormatFloat(float64(size.Height), 'f', 2, 32))
}

// sameSize compares two sizes with half-a-point tolerance: the driver converts
// the content size through the display scale on the way to the window and back
// on the way to the canvas, so a 960 the app asked for can read as 959.99994.
// Anything that small in the difference is the same window.
func sameSize(a, b fyne.Size) bool {
	return math.Abs(float64(a.Width-b.Width)) < 0.5 && math.Abs(float64(a.Height-b.Height)) < 0.5
}

func sizedWithin(side float64) bool {
	return side >= minWindowSide && side <= maxWindowSide
}

// prefBool and prefFloat read one stored value. A value that will not parse
// counts as absent: the setting was written by something whose vocabulary this
// binary does not share, and the front end's own default beats a guess.
func prefBool(s Settings, key string) (v, ok bool) {
	raw, found := prefString(s, key)
	if !found {
		return false, false
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Debug("ignoring unreadable setting", "key", key, "value", raw)
		return false, false
	}
	return parsed, true
}

func prefFloat(s Settings, key string) (v float64, ok bool) {
	raw, found := prefString(s, key)
	if !found {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		slog.Debug("ignoring unreadable setting", "key", key, "value", raw)
		return 0, false
	}
	return parsed, true
}

func prefString(s Settings, key string) (string, bool) {
	if s == nil {
		return "", false
	}
	raw, found, err := s.Setting(key)
	if err != nil {
		// A database that cannot be read is the transcript's problem to
		// complain about; here it only means a default.
		slog.Debug("could not read setting", "key", key, "err", err)
		return "", false
	}
	return raw, found
}

// putPref writes one key and reports failure without failing anything: a
// preference that would not save costs the next launch one reset, which the
// user learns about from this line and nothing else.
func putPref(s Settings, key, value string) {
	if s == nil {
		return
	}
	if err := s.SetSetting(key, value); err != nil {
		slog.Warn("could not save setting", "key", key, "err", err)
	}
}
