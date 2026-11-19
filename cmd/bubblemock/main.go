// Command bubblemock opens the transcript's bubble mockup: one card of every
// message kind, rendered by ui's real code path, with the outline's weight and
// strength and the light/dark theme on sliders so a design decision can be
// looked at rather than argued about. It shares no state with a session and
// writes nothing.
//
//	go run ./cmd/bubblemock
//
// It is a separate command and not a test because a Fyne window has to be built
// on the process's main thread; see the comment on ui.ShowMockup.
package main

import "github.com/VictorLowther/mini/ui"

func main() { ui.ShowMockup() }
