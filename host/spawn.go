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

	// Wiring the child's identity and its route to a person happens BEFORE the job
	// starts, and the ordering - not any locking - is what makes it safe:
	// Jobs.Start launches the run goroutine, whose loop reads Loop.Hooks and
	// Loop.Origin with nothing guarding them, so an assignment made after it races
	// the child's first tool call. The losing order is a child running with no
	// confirmation gate, which is §5.5's widening reached by timing rather than by
	// design. (A comment here used to claim the opposite, about a job it thought
	// was started below rather than above.)
	child.Loop.Origin = req.Name
	// The child's registry is what decides whether the child is done, so its quiet
	// has to reach the wrapper - and a send that arrives later uses the same
	// channel, which is why the wake lives on the session rather than on the job.
	child.Jobs.OnQuiescent = child.notify

	// A window that shows the child attends it directly: its own transcript, its
	// own input, its own prompts (decision 15: "bubbling becomes routing", and a
	// child with a tab *is* attendable). An error here means the child could not be
	// given one, and an unanswerable child does not get to run.
	attended := false
	if hook := s.proc.OnChild; hook != nil {
		if err := hook(child); err != nil {
			_ = child.Close()
			return agent.SpawnReply{}, fmt.Errorf("the subagent could not be given a front end: %w", err)
		}
		attended = true
	}
	if !attended {
		// No front end of its own, so the child's prompts reach the human who owns
		// the parent through the parent's hooks - decision 13. Before that existed
		// this refused outright rather than spawn a confirm-mode parent's child,
		// because the widening was invisible; now the widening is impossible, since
		// the only way a child gets past a confirmation is the same human approval
		// that gates its parent. Where the parent has no hooks either - a batch run,
		// or a human who chose yolo - the child has no asker at all, and anything
		// that would prompt rejects with a reason (§3.7, omp's posture) rather than
		// wait for a person who will never arrive.
		if parent := s.Loop.Hooks; parent != nil {
			child.Loop.Hooks = &bubblingHooks{parent: parent, child: child, label: req.Name}
		}
	}
	if h := child.Loop.Hooks; h != nil {
		// Whoever attends the child, the child's endings are not its own to
		// negotiate. The front end a tab installs asks a human for the next task
		// when a run submits, which on a delegated session is a deadlock with a
		// prompt on it: the loop never returns, the agent row never goes terminal,
		// and the parent waits on a wake that can never fire.
		child.Loop.Hooks = childHooks{inner: h}
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
	s.proc.registerAgentJob(s.ID.String(), run.ID(), child)
	return agent.SpawnReply{JobID: run.ID(), OutputPath: run.OutputPath(), Name: req.Name}, nil
}

// OpenSibling opens another top-level session beside this one - the same merged
// configuration, a different working directory, and no lineage: a sibling is a
// second conversation the human chose to start, not a delegation, so it is
// resumable by name and it inherits nothing but the config. The copy is the
// point (§9.8): the new session's trajectory path and any resume id are set by the
// caller's spec and by Open, never carried over from the session beside it.
func (s *Session) OpenSibling(workdir, project string) (*Session, error) {
	return s.proc.Open(Spec{
		Config:  cloneConfig(s.Configs),
		Workdir: workdir,
		Project: project,
		Kind:    session.KindTop,
	})
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
			child.setParked(true)
			quiet := waitQuiet(ctx, child)
			child.setParked(false)
			if !quiet {
				fmt.Fprintf(out, "subagent %s was stopped with %d job(s) still running\n", child.Name, child.LiveJobs())
				return job.Result{Reason: "cancelled with jobs still running"}
			}
		}

		// §9.9's promise, which the first version of this broke: a child that
		// ended any other way than by submitting still owes a report naming what
		// happened, and "(ended without a report)" is a summary of nothing. A
		// subagent stopped by its own cost limit is the common case - the limit is
		// the session's, so it is spent cumulatively across every run of it,
		// including the one a revival added - and the parent cannot act on a
		// silent blank when the truth is "your helper ran out of budget on pass
		// two".
		report := submissionOf(lastInfo)
		if strings.TrimSpace(report) == "" {
			report = endingOf(child, lastInfo)
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
	// Asked of the store rather than of the live handles, because that is the
	// rule this package is built on - the database is the source of truth about
	// what a job is, and a handle is only the mechanism for signalling one
	// (job.go:12-16). It is also the only version that can answer at all for the
	// case that matters most here: a finished subagent has no handle left, so a
	// handle-based lookup cannot see the very job a revival is addressed to.
	row, err := s.Sess.GetJob(jobID)
	if err != nil {
		return err
	}
	if row.Kind != session.KindSubagent {
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
	// Three states, and the middle one is the reason this is not simply a
	// queue-and-hope. A child that is *waiting* on its own background work has a
	// wrapper parked in waitQuiet, so a nudge reaches it. A child that has
	// **finished** has no wrapper at all - its row is terminal and its goroutine
	// is gone - and delivering a message there would park it in the queue
	// forever, with the parent waiting on a second report that nothing was left
	// alive to write. That is §9.3's revival, and it needs a new run, not a
	// notification.
	child.Loop.QueueMessage(msg)
	if child.Running() {
		return nil // the next step boundary drains it
	}
	if child.wakeUp() {
		return nil // a parked wrapper will re-drive and pick it up
	}
	return s.revive(child, child.Name)
}

// wakeUp nudges a parked wrapper and reports whether one exists. The distinction
// is held in memory rather than inferred from the registry: a row can be
// non-terminal because the wrapper is still waiting on the child's own jobs, or
// because the child is mid-turn, and only the first is a parked wrapper.
func (s *Session) wakeUp() bool {
	s.mu.Lock()
	parked := s.parked
	s.mu.Unlock()
	if !parked {
		return false
	}
	s.notify()
	return true
}

// revive starts a second agent row over the same child session - §9.3's answer to
// "is a follow-up a reopened job or a new one", taken because the state machine
// has no legal trip from delivered back to running and inventing one would forfeit
// the structural termination the jobs design leans on. The transcript is the
// durable thing; the row is one run of it.
//
// The name carries over and the label says so, because the parent will otherwise
// see two rows for what it thinks of as one helper, and the honest reading is that
// the second one is a continuation rather than a second worker.
func (s *Session) revive(child *Session, label string) error {
	if strings.TrimSpace(label) == "" {
		label = child.ID.String()[:8] // a child opened without a name still needs one in the row
	}
	run, err := s.Jobs.Start(job.Spec{
		Kind:        session.KindSubagent,
		Label:       label,
		Description: "resumed subagent: " + label,
		Command:     "subagent " + label,
		Run:         s.childRunner(child, ""),
	})
	if err != nil {
		return err
	}
	s.proc.registerAgentJob(s.ID.String(), run.ID(), child)
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

// endingOf is the report for a child that stopped without submitting: which
// ending, how far it got, and where its transcript is, because all three change
// what the parent does next. A child that hit its cost limit is a child to
// re-delegate more narrowly; a child that hit a step limit is one whose task was
// too vague; and "it stopped" distinguishes neither.
func endingOf(child *Session, info map[string]any) string {
	status := "without a report"
	if info != nil {
		if s, _ := info["exit_status"].(string); s != "" {
			status = strings.ToLower(s)
		}
	}
	return fmt.Sprintf(
		"subagent %q ended %s after %d model calls and $%.2f of its own spend, with no submission to report. "+
			"Its transcript is at %s.",
		child.Name, status, child.Loop.NCalls, child.Loop.Cost, child.Sess.Workdir)
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

// childHooks applies the ending rules of a delegated session on top of whichever
// front end attends it - the gate, the messages and the interruption text all belong
// to that front end and are forwarded untouched.
//
// A child's turn with no tool call is not an invitation to its own human for the
// next task: it is the report, and only the job wrapper decides whether the run may
// end (§5.12, §9.10). Asking instead strands everybody - Loop.Run never returns, the
// agent row stays running, no wake is fired, and the parent waits forever for a
// report that cannot arrive. Human mode is the same story from the other side: a
// child takes direction through job_send, not by holding its loop open on a prompt.
//
// A child with no hooks of its own is deliberately left unwrapped, because nil
// Hooks is the loop's "nobody can answer, so refuse" signal (§5.5) and a wrapper
// over nothing would turn that refusal into silent approval.
type childHooks struct{ inner agent.Hooks }

func (c childHooks) BeforeExecute(ctx context.Context, actions []model.Action, tvars map[string]any) error {
	return c.inner.BeforeExecute(ctx, actions, tvars)
}

// OnSubmit ends the run: the submission is the report, not a question.
func (c childHooks) OnSubmit(context.Context, model.Message) error { return nil }

// NextHumanCommand declines to hold a child open for typed commands.
func (c childHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }

func (c childHooks) OnMessage(m model.Message) { c.inner.OnMessage(m) }

func (c childHooks) OnInterrupt(ctx context.Context) error { return c.inner.OnInterrupt(ctx) }

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

func (b *bubblingHooks) NextHumanCommand(context.Context) (string, bool) { return "", false }

// OnMessage is ignored: a child's turn-by-turn printing belongs to the roster of
// step 7, not to the parent's transcript, where it would read as the parent
// talking to itself.
func (b *bubblingHooks) OnMessage(model.Message) {}

// OnInterrupt records the interruption in the child's own history through the
// loop's default, which is nil here - the parent's human pressed nothing.
func (b *bubblingHooks) OnInterrupt(context.Context) error { return nil }
