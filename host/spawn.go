package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/job"
	"github.com/VictorLowther/mini/model"
	"github.com/VictorLowther/mini/session"
)

// Spawn implements agent.Spawner: it opens a child session, registers it as an
// agent-kind job of this session, and returns as soon as it is running.
//
// The child is a job rather than a tool call because the registry already owns
// everything a subagent needs to be usable - a row that outlives the turn, an
// output file appended at EOF, a terminal state that writes a firing, and
// delivery at the parent's next step boundary - and because `jobs`, `job_kill`
// and the card a handed-off call renders in the window all work on it without
// knowing what it runs (docs/parallel-sessions.md §5.10, §9.5).
func (s *Session) Spawn(ctx context.Context, req agent.SpawnRequest) (agent.SpawnReply, error) {
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Prompt) == "" {
		return agent.SpawnReply{}, errors.New("a subagent needs a name and a prompt")
	}
	if s.Kind != session.KindTop {
		// The store refuses a grandchild and the tool is not advertised to a
		// subagent; this is the third layer, and the one that still answers if a
		// caller reaches the seam directly (§5.6: layers that agree cannot drift).
		return agent.SpawnReply{}, errors.New("a subagent cannot spawn another subagent")
	}
	if err := s.nameAvailable(req.Name); err != nil {
		return agent.SpawnReply{}, err
	}
	// A child's prompts reach the human who owns the parent, through the parent's
	// own hooks - decision 13. Before that existed this refused outright rather
	// than spawn an unattended child from a confirm-mode parent, because the
	// widening was invisible; now the widening is impossible instead, since the
	// only way a child gets past a confirmation is the same human approval that
	// gates its parent. Where the parent has no hooks - a batch run, or a parent
	// whose human already chose yolo - the child is unattended by inheritance and
	// anything that would prompt rejects with a reason (§3.7, omp's posture)
	// rather than waiting for a person who will never arrive.
	// (Built just below, once the child exists to point at.)

	child, err := s.proc.Open(Spec{
		// A copy, never the parent's map: a child's config is constructed here,
		// so nothing is inherited by accident - not the trajectory path, not a
		// resume id, not a widened mode (§9.8, §5.5).
		Config:   cloneConfig(s.Configs),
		Workdir:  s.Workdir, // no workspace isolation, by ruling (§5.9)
		Project:  s.Project,
		Kind:     session.KindSubagent,
		ParentID: s.ID.String(),
		Name:     req.Name,
		Model:    s.childModel(),
		Logger:   s.log.With("subagent", req.Name),
	})
	if err != nil {
		return agent.SpawnReply{}, err
	}

	run, err := s.Jobs.Start(job.Spec{
		Kind:        session.KindSubagent,
		Label:       req.Name,
		Description: "subagent: " + firstLine(req.Prompt),
		Command:     "subagent " + req.Name, // the registry's one text field
		TCNO:        0,
		Run:         s.childRunner(child, req.Prompt),
	})
	if err != nil {
		_ = child.Close()
		return agent.SpawnReply{}, err
	}
	// Wiring the child's own identity and its route to a person. Nothing has run
	// yet at this point - the job below is what starts the loop - so these two
	// assignments are race-free by ordering rather than by locking, and that is
	// worth the comment because a later caller that opened a child and ran it
	// somewhere else would break both silently.
	child.Loop.Origin = req.Name
	if parent := s.Loop.Hooks; parent != nil {
		child.Loop.Hooks = &bubblingHooks{parent: parent, child: child, label: req.Name}
	}

	// The child's registry is what decides whether the child is done, so its
	// quiet has to reach the wrapper - and a send that arrives later uses the
	// same channel, which is why the wake lives on the session rather than on
	// the job.
	child.Jobs.OnQuiescent = child.notify
	s.proc.registerAgentJob(s.ID.String(), run.ID(), child)
	return agent.SpawnReply{JobID: run.ID(), OutputPath: run.OutputPath(), Name: req.Name}, nil
}

// nameAvailable holds a name open until the work it labels has left the box.
// Uniqueness is scoped to the top-level session, which for a two-level tree is
// this parent's children, live or finished - and finished ones still hold the
// name while any job they started is running (§5.5, §9.10).
func (s *Session) nameAvailable(name string) error {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, kid := range s.proc.Children(s.ID) {
		if strings.ToLower(strings.TrimSpace(kid.Name)) != want {
			continue
		}
		if kid.Running() || kid.LiveJobs() > 0 {
			return fmt.Errorf("a subagent named %q is still working; pick another name or stop it first", name)
		}
	}
	// A child that finished and was dropped from the live set still shows in the
	// store, so a name is not reusable merely because the process forgot it.
	rows, err := s.Sess.ListJobs()
	if err == nil {
		for _, r := range rows {
			if r.Kind == session.KindSubagent && strings.ToLower(strings.TrimSpace(r.Label)) == want && r.State == session.JobRunning {
				return fmt.Errorf("a subagent named %q is still working; pick another name or stop it first", name)
			}
		}
	}
	return nil
}

// childModel asks the process for a spawned session's client, or nil to let the
// config decide.
func (s *Session) childModel() model.Model {
	if s.proc.ChildModel == nil {
		return nil
	}
	return s.proc.ChildModel(session.KindSubagent)
}

// childRunner is the job's body: it runs the child, and does not report an
// ending until the child is genuinely done - which, after §5.12, means it has
// stopped with no background job of its own still able to report.
//
// Everything new lives here rather than in agent.Loop. The child's loop behaves
// exactly as a top-level session's does, including returning beside a running
// job; what the wrapper adds is only the question "is anything still owed?" and
// the patience to wait for the answer. That is why the top-level ruling in §9.10
// costs nothing: no exit status, no loop contract, and no human-visible prompt
// changed on the way here.
func (s *Session) childRunner(child *Session, prompt string) func(context.Context, io.Writer) job.Result {
	return func(ctx context.Context, out io.Writer) job.Result {
		task := prompt
		var lastInfo map[string]any

		for {
			info, err := child.runChildTurn(ctx, task)
			lastInfo = info
			task = "" // a re-drive adds no new user message; the queue does that

			if err != nil {
				// Cancellation and limits are endings the parent must see; the
				// row records the state, the body says what happened.
				fmt.Fprintf(out, "subagent %s ended: %v\n", child.Name, err)
				return job.Result{Reason: fmt.Sprintf("subagent ended: %v", err)}
			}
			if child.LiveJobs() == 0 {
				break // quiescent: this turn's submission is the report
			}
			// Not an ending. The child has work in flight whose result will
			// drive it, so wait for the registry to empty - or for a send, or
			// for the parent to stop us - and then run it again.
			if !waitQuiet(ctx, child) {
				fmt.Fprintf(out, "subagent %s was stopped with %d job(s) still running\n", child.Name, child.LiveJobs())
				return job.Result{Reason: "cancelled with jobs still running"}
			}
		}

		report := submissionOf(lastInfo)
		if strings.TrimSpace(report) == "" {
			report = "(the subagent ended without a report)"
		}
		// The full report goes in the job's own file, where `jobs` already points
		// and nothing truncates it; the parent's wake carries the capped body.
		fmt.Fprint(out, report)
		code := 0
		return job.Result{ExitCode: &code, Reason: reportOf(report)}
	}
}

// runChildTurn runs one turn-series on the child's existing transcript. The
// first call is a normal run with the task; a later one seeds the resume slot
// from the live history, because Run starts a conversation rather than
// continuing one, and a re-drive is exactly a resume from the state in memory.
func (s *Session) runChildTurn(ctx context.Context, task string) (map[string]any, error) {
	if task == "" {
		s.Loop.Resume(s.Loop.Messages)
	}
	return s.Run(ctx, task)
}

// waitQuiet blocks until the child's registry empties, a message arrives for it,
// or ctx ends. It is the four-way select §9.10 warned about, and the cancel leg
// is the one that keeps a quit honest: a parent that cannot be interrupted while
// its child runs a server nobody asked for is the failure this whole design was
// supposed to remove.
func waitQuiet(ctx context.Context, child *Session) bool {
	for {
		select {
		case <-child.wake:
			// Either the registry just emptied, or a send arrived for a child
			// that is not running. Both mean "there is a turn worth taking".
			return true
		case <-ctx.Done():
			return false
		case <-time.After(quietWatchdog):
			// The watchdog is a belt against a missed edge, and it must not be
			// a timer: returning true here would re-drive the child, and every
			// re-drive is a model query. Waking a waiting subagent on a schedule
			// is precisely the polling this whole delivery design exists to
			// remove - and the prompt spends a paragraph telling the model not to
			// do it, which the harness would be a hypocrite to do instead.
			if child.LiveJobs() == 0 {
				return true // the edge was missed; this turn is real work
			}
			if ctx.Err() != nil {
				return false
			}
			// Still waiting on something that has not finished. Keep waiting.
		}
	}
}

// Send implements agent.Spawner.
func (s *Session) Send(ctx context.Context, jobID, text string) error {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(text) == "" {
		return errors.New("job_send needs an 'id' and the 'text' to send")
	}
	run, err := s.Jobs.Get(jobID)
	if err != nil {
		return err
	}
	if run.Kind() != session.KindSubagent {
		// §9.11: the refusal is flat, because there is nothing for the model to
		// do differently - a bash job's stdin is /dev/null, and making it
		// accept input is an interpreter change, not a branch in this tool.
		return errors.New("Cannot send to job: only subagents accept input for now")
	}
	child, ok := s.proc.childOfJob(s.ID.String(), jobID)
	if !ok {
		return fmt.Errorf("job %s is not a subagent of this session", jobID)
	}
	msg := model.Message{
		"role":    "user",
		"content": text,
		"extra":   map[string]any{"interrupt_type": "ParentMessage", "from_session": s.ID.String()},
	}
	if child.Running() {
		child.QueueMessage(msg) // the path typed input already takes
		return nil
	}
	// A finished or waiting child: queue the message and wake whoever is
	// holding the row, so the re-drive carries it.
	child.Loop.QueueMessage(msg)
	child.notify()
	return nil
}

// notify nudges the wrapper that something has arrived. Non-blocking: a full
// channel already means "there is an unread event", which is all the receiver
// needs to know.
func (s *Session) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// reportOf keeps the row's reason short: it is what `jobs` prints and what a
// card shows, and the report itself is in the output file.
func reportOf(report string) string {
	r := strings.TrimSpace(strings.ReplaceAll(report, "\n", " "))
	if len(r) > 200 {
		return r[:200] + "..."
	}
	if r == "" {
		return "reported nothing"
	}
	return r
}

func submissionOf(info map[string]any) string {
	if info == nil {
		return ""
	}
	s, _ := info["submission"].(string)
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// quietWatchdog bounds a wait that should never need bounding, and is checked
// without waking anybody. A subagent that
// started a server and forgot it is waiting on something that will not end, and
// §5.10 rules that the human decides - but the wrapper re-checking every so
// often is what lets `job_kill`, a send, or a quit reach it rather than leaving
// the goroutine parked on a channel forever.
const quietWatchdog = 30 * time.Second

// cloneConfig deep-copies a merged configuration. It exists because a child's
// config must be a copy (§9.8) and because the maps inside it are shared by
// reference otherwise: `agent` holds templates, `model` holds pricing, and a
// child that replaced one would edit its parent's mind.
func cloneConfig(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch tv := v.(type) {
		case map[string]any:
			out[k] = cloneConfig(tv)
		case []any:
			cp := make([]any, len(tv))
			for i, e := range tv {
				if em, ok := e.(map[string]any); ok {
					cp[i] = cloneConfig(em)
					continue
				}
				cp[i] = e
			}
			out[k] = cp
		default:
			out[k] = v
		}
	}
	// The two keys that must not survive the copy, whatever the parent was
	// configured with: a trajectory path two sessions would write, and a resume
	// identity that would make the child a second owner of the parent's session.
	if ag, ok := out["agent"].(map[string]any); ok {
		delete(ag, "output_path")
		delete(ag, "resume")
	}
	return out
}

// bubblingHooks is a child session's route to a person. It forwards exactly one
// thing - the gate that stands between a model and a command - to the parent's
// hooks, and answers everything else the way an unattended session must.
//
// The asymmetry is deliberate. BeforeExecute is the one hook whose absence
// silently widens authority, so it bubbles. The rest shape the *parent's* run:
// asking whether that conversation is over, offering to raise its limits, or
// taking human-mode commands from a child's turn would all be answering the wrong
// question with somebody else's state. A child that hits its own limits ends, and
// the reason reaches its parent as the report - which is a true statement about
// the child rather than a prompt nobody meant to direct at the parent's human.
type bubblingHooks struct {
	parent agent.Hooks
	child  *Session
	label  string
}

func (b *bubblingHooks) BeforeExecute(ctx context.Context, actions []model.Action, tvars map[string]any) error {
	// The child's own template variables carry its name as `asker`, so the
	// attribution travels with the request and the parent's front end can say
	// whose command this is (§5.5). Rebuilding them here would be wrong: the
	// variables the loop passed in are the child's, which is the point.
	if tvars == nil {
		tvars = b.child.Loop.TemplateVars()
	}
	return b.parent.BeforeExecute(ctx, actions, tvars)
}

// OnSubmit ends the child's run: there is no "keep going?" for a conversation
// whose human is somebody else's.
func (b *bubblingHooks) OnSubmit(context.Context, model.Message) error { return nil }

// OnLimits does not offer the parent's human a chance to raise a child's budget.
// Returning the pending interrupt ends the child with its limits named, and that
// is the report the parent gets.
func (b *bubblingHooks) OnLimits(_ context.Context, _ *agent.Config, pending *model.Interrupt) error {
	return pending
}

func (b *bubblingHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }

// OnMessage is ignored: a child's turn-by-turn printing belongs to the roster of
// step 7, not to the parent's transcript, where it would read as the parent
// talking to itself.
func (b *bubblingHooks) OnMessage(model.Message) {}

// OnInterrupt records the interruption in the child's own history through the
// loop's default, which is nil here - the parent's human pressed nothing.
func (b *bubblingHooks) OnInterrupt(context.Context) error { return nil }
