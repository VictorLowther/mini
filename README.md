# mini

A Go port of [mini-swe-agent](https://github.com/SWE-agent/mini-swe-agent)'s
`mini-swe-agent-internal` linear bash-agent loop (see the single commit:
"Port mini-swe-agent's linear bash agent loop to Go"). It is an LLM agent that
iteratively issues bash commands against a local environment to solve a task
and then submits its result. The model is served by a local OpenAI-compatible
"sushi" server streaming SSE responses.

Notably, the system/instance prompts in `config/configs/mini.yaml` are the very
prompts driving agents like the one reading this file.

## Layout

| Package | Contents |
|---|---|
| `cmd/mini` | The `mini` CLI: flag parsing, config resolution, `mini config` subcommand, trajectory output. |
| `agent` | The linear query–execute loop (port of `agents/default.py` + interactive control flow): step/cost/wall-time limits, format-error retries, trajectory JSON dumping. |
| `env` | Command execution. `Local` interprets bash in-process via `mvdan.cc/sh` (so `[[ ]]`, arrays, heredocs behave identically everywhere); `exec_*.go` handle process plumbing per-OS. |
| `model` | HTTP/SSE client for the sushi server: streaming (`stream.go`), tool-call → action parsing (`actions.go`), cost stats (`stats.go`), and a scriptable `testmodel`. |
| `tpl` | Prompt rendering: a `text/template` engine for the prompt templates in `mini.yaml`, plus the custom filters they use (`toJSON`, `head`/`tail`/`charlen`, arithmetic). |
| `config` | mini-swe-agent's config layering: recursive merge with an UNSET sentinel, config-spec resolution (file / embedded name / `key=value`), global `.env` handling. Embedded configs live in `config/configs/`; `mini.yaml` is the only supported system config. |
| `ui` | The human end: a bubbletea/glamour TUI (`tui.go`/`tuimodel.go`), a plain `--print` mode (`print.go`), and a Fyne desktop GUI (`fyneui.go`/`fyneview.go`, behind the `gui` build tag), plus slash-commands/modes (`slash.go`) and `Interactive` adapting a UI to `agent.Hooks` (confirm/yolo modes, whitelists). All front ends render the run as a shell transcript (`transcript.go`: command, cwd, exit, output) from a message's `extra` through one shared `messageBlock`; the TUI runs inline (no alternate screen) with a pane exactly as tall as the terminal, holding the transcript's newest rows plus the prompt, and pushes rows that leave the top of the pane into the terminal's own scrollback through an off-loop `printer` goroutine (`printer.go`) — that keeps the footer pinned to the bottom, which a pane that resizes to its content does not. The startup header is transcript, not chrome, for the same reason. It also mutes the process logger while it owns the terminal (`logtarget.go`), because a stray log write scrolls the screen behind the renderer. |
| `testdata` | Offline fixtures: hand-written SSE scripts and `fakesushi`, an HTTP replay server for GPU-free testing. See `testdata/README.md`. |

## Usage

```bash
go build ./cmd/mini
./mini -t "add a main() guard to a.py, then submit"        # interactive TUI
./mini -p -y -t "run 'echo hi' then submit" -o traj.json   # headless yolo run
./mini -c mini.yaml -c model.max_tokens=2048               # config overrides
```

By default the config comes from the embedded `mini.yaml`; passing `-c`
replaces that default file. Point `-m`/`--url` at a sushi server (default
`http://127.0.0.1:1234`).

### Desktop GUI

The GUI is a Fyne app, so it needs CGO and the platform's OpenGL/Metal
headers; every Fyne-importing file sits behind the `gui` build tag and the
default build never compiles it (`mini gui` without the tag just prints the
build command).

```bash
go build -tags "gui migrated_fynedo" ./cmd/mini   # migrated_fynedo is Fyne's 2.9 threading default
./mini gui -t "run 'echo hi' then submit"         # same flags as the terminal
```

On linux the toolchain needs `libgl1-mesa-dev xorg-dev` installed first.
The window is the TUI's contract with widgets: the transcript is a series of
interleaved text areas, one per message type, each on a subtly tinted,
rounded, bordered card — `user` for text the human typed (right-justified,
chat-app style), `harness` for feedback the harness authored in the user's
name (rejections, interruption records, format-error prompts — left-justified,
so the two never read alike), `thinking`, `tool call` (fixed-width command
grid), and `LLM output` (markdown). Each area's header uses the body's own
font and names its kind plus the message's generated token count, generation
time, and cost when non-zero (`LLM output · 254 tok · 3.1s · $0.02`). The
input box stays live whenever the harness is waiting — including at the
"Agent wants to finish?" prompt, where a new task continues the run — and the
only ways to leave are `/quit`, the native quit hotkey (⌘Q / Ctrl+Q, or the
menu), or closing the window; Enter sends, Shift+Enter (or ⌥Enter on macOS)
adds a newline — multi-line input is a keystroke, not a mode. Thinking
toggles with ⌃⌘T on macOS (plain ⌘T stays free for future tabs) and Ctrl+T
elsewhere.

## Testing

Everything is offline and read-only against `testdata/`:

```bash
go test ./...
go test -tags "gui migrated_fynedo" ./ui/   # the GUI suite; headless, no display needed
```
