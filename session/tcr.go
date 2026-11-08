package session

import (
	"fmt"
	"path/filepath"
)

// tcrDir and tcrPath derive result-file locations from the session id and the
// store's base; the path is never stored in a row, so every reader computes it
// with this same expression (Transcript does the same for the model-facing
// extra["tcr_path"]).
func (s *Session) tcrDir() string { return filepath.Join(s.store.tcrBase, s.ID.String()) }

func (s *Session) tcrPath(tcno int) string {
	return filepath.Join(s.tcrDir(), fmt.Sprintf("%d.log", tcno))
}
