//go:build !gui

package main

import (
	"fmt"
	"os"
)

// runGUI lives behind the gui build tag: Fyne needs CGO plus the platform's
// OpenGL headers (on linux also xorg-dev / libgl1-mesa-dev), and the default
// build of mini must not require that toolchain.
func runGUI(args []string) int {
	fmt.Fprintln(os.Stderr, `mini gui requires: go build -tags "gui migrated_fynedo" ./cmd/mini`)
	return 1
}
