package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"uuid"

	"github.com/VictorLowther/mini/model"
)

// Trajectory export: writing a conversation out to a file is a thing a human asks
// for, not a side effect of running a session. The store is the source of truth for
// transcripts, so an export is rendered from it - which is what makes one code path
// serve a session that is still on screen, one that ended yesterday, and a subagent
// that never had a window of its own.
//
// The document keeps mini-swe-agent's shape where the store can honour it - an
// `info` block, the message list, and the format tag that says what the rest is -
// and omits `info.config` rather than inventing it. The store never recorded the
// configuration a session ran under, so filling that block with today's config
// would attach a document to the past that the past never had.

// Trajectory builds the exportable document for this session: its own row's facts
// plus its stored transcript, in the order it was recorded.
func (s *Session) Trajectory() (map[string]any, error) {
	msgs, err := s.Transcript()
	if err != nil {
		return nil, err
	}
	exit, submission := ending(msgs)
	return map[string]any{
		"info": map[string]any{
			"session_id": s.ID.String(),
			"kind":       s.Kind,
			"parent_id":  s.ParentID,
			"root_id":    s.RootID,
			"title":      s.Title,
			"project":    s.Project,
			"workdir":    s.Workdir,
			"model":      s.ModelName,
			"model_stats": map[string]any{
				"instance_cost": s.Cost,
				"api_calls":     s.NCalls,
			},
			"exit_status": exit,
			"submission":  submission,
			"created_at":  s.CreatedAt,
			"updated_at":  s.UpdatedAt,
		},
		"messages":          msgs,
		"trajectory_format": TrajectoryFormat,
	}, nil
}

// TrajectoryFormat names the document shape, so a reader can tell what it is
// holding without opening the first message.
const TrajectoryFormat = "mini-swe-agent-1.1"

// ending pulls the two facts the loop used to carry in memory - how the session
// stopped and what it submitted - out of the last message that has them, because
// those are recorded as turn extras rather than on the session row.
func ending(msgs []model.Message) (exit, submission string) {
	for i := len(msgs) - 1; i >= 0; i-- {
		extra, ok := msgs[i]["extra"].(map[string]any)
		if !ok {
			continue
		}
		if exit == "" {
			exit, _ = extra["exit_status"].(string)
		}
		if submission == "" {
			submission, _ = extra["submission"].(string)
		}
		if exit != "" && submission != "" {
			break
		}
	}
	return exit, submission
}

// ExportTrajectory writes this session's document to path and returns the path
// written. An empty path picks one in the working directory named after the
// session, so the default destination is somewhere the human was already standing
// rather than a state directory nobody looks in.
func (s *Session) ExportTrajectory(path string) (string, error) {
	if path == "" {
		path = DefaultTrajectoryPath(s.ID)
	}
	doc, err := s.Trajectory()
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding trajectory: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	// Written through a temporary file even for an export: a half-written
	// trajectory is worse than none, and a JSON document that fails to encode
	// halfway through is unreadable rather than incomplete.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// DefaultTrajectoryPath is where an export with no destination of its own goes.
func DefaultTrajectoryPath(id uuid.UUID) string {
	return filepath.Join(".", fmt.Sprintf("mini-%s.traj.json", id.String()[:8]))
}
