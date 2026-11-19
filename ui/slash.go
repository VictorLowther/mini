package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/VictorLowther/mini/agent"
	"github.com/VictorLowther/mini/model"
)

// modeCommands is InteractiveAgent._MODE_COMMANDS_MAPPING.
var modeCommands = map[string]string{"/u": "human", "/c": "confirm", "/y": "yolo"}

func isModeCommand(s string) bool { _, ok := modeCommands[s]; return ok }

// quitCommand ends the run at the exit prompt. Leaving is always deliberate:
// an empty enter there is a no-op, and ctrl-c opens the same exit dialog.
const quitCommand = "/quit"

// helpText is the /h block.
const helpText = `/y to switch to yolo mode (execute LM commands without confirmation)
/c to switch to confirmation mode (ask for confirmation before executing LM commands)
/u to switch to human mode (execute commands issued by the user)
/m to enter multiline comment
/t to show or hide the model's thinking text
/compact to summarize the earlier context into a card before the model's next turn
/quit to leave when the agent is ready (empty enter re-asks, never quits)`

// promptLine is the "> " input marker.
func promptLine() string { return "> " }

// slash applies the /y /c /u /h /m commands to one input line, the way
// _prompt_and_handle_slash_commands does, and reports whether the caller must
// re-prompt ("Already in <mode> mode."). cfg.Mode is updated in place.
//
// readMore supplies the /m multiline body.
func slash(cfg *agent.Config, input string, out io.Writer, readMore func() string) (text string, reprompt bool) {
	switch input {
	case "/m":
		return readMore(), false
	case "/h":
		fmt.Fprintf(out, "Current mode: %s\n%s\n", cfg.Mode, helpText)
		return "", true
	}
	if target, ok := modeCommands[input]; ok {
		if cfg.Mode == target {
			fmt.Fprintf(out, "Already in %s mode.\n", cfg.Mode)
			return "", true
		}
		cfg.Mode = target
		fmt.Fprintf(out, "Switched to %s mode.\n", target)
		return input, false
	}
	return input, false
}

func confirmationTitle(n int, why string) string {
	base := fmt.Sprintf("Execute %d action(s)? Enter to confirm, type a comment to reject, or /h to show available commands", n)
	if why == "" {
		return base
	}
	// The reason is the part a human can act on. "Whitelisted" and "asked" look
	// identical from the outside, and someone who is nagged without being told
	// what would stop the nagging turns the gate off instead of fixing the
	// pattern - which is how a security control dies.
	return base + "\nWhy it is asking: " + why
}

const newTaskTitle = "Agent is ready. Type a new task."
const interruptionTitle = "Interrupted. Type a comment or command (/h for available commands)"

// TaskPrompt is the opening question when no -t/--task was given.
const TaskPrompt = "What do you want to do?"

// interruptUser raises the message shape Python's _interrupt() builds: the
// harness speaking *about* an exchange, under the interrupt_type that says so.
func interruptUser(content, itype string) error {
	return &model.Interrupt{
		Kind:     model.KindUserInterruption,
		Messages: []model.Message{{"role": "user", "content": content, "extra": map[string]any{"interrupt_type": itype}}},
	}
}

// userWords is interruptUser for the cases where the text came off the keyboard.
// The harness still prefixes it with the sentence the model needs ("Interrupted
// by user: ..."), but a front end must not have to see through that wrapper to
// work out who to show it as, so the message says who spoke. Carried, exactly
// like Question.Origin (decision 13): attribution that is derived is
// attribution that is wrong the first time somebody adds a kind.
func userWords(content, itype string) error {
	return &model.Interrupt{
		Kind: model.KindUserInterruption,
		Messages: []model.Message{{"role": "user", "content": content, "extra": map[string]any{
			"interrupt_type": itype, userTypedKey: true,
		}}},
	}
}

// firstLine is the one-line summary of a multi-line message or task.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
