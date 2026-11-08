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
| `tpl` | Prompt rendering: upstream Jinja templates mechanically translated to `text/template`, plus the filters the translation needs. |
| `config` | mini-swe-agent's config layering: recursive merge with an UNSET sentinel, config-spec resolution (file / embedded name / `key=value`), global `.env` handling. Embedded configs live in `config/configs/` (`default.yaml`, `mini.yaml`). |
| `ui` | The human end: a bubbletea/glamour TUI (`tui.go`/`tuimodel.go`) and a plain `--print` mode (`print.go`), slash-commands/modes (`slash.go`), and `Interactive` adapting a UI to `agent.Hooks` (confirm/yolo modes, whitelists). Both front ends render the run as a shell transcript (`transcript.go`: command, cwd, exit, output) from a message's `extra`; the TUI runs inline (no alternate screen) with a pane exactly as tall as the terminal, holding the transcript's newest rows plus the prompt, and pushes rows that leave the top of the pane into the terminal's own scrollback through an off-loop `printer` goroutine (`printer.go`) — that keeps the footer pinned to the bottom, which a pane that resizes to its content does not. The startup header is transcript, not chrome, for the same reason. It also mutes the process logger while it owns the terminal (`logtarget.go`), because a stray log write scrolls the screen behind the renderer. |
| `testdata` | Offline fixtures: recorded Python-run trajectories, render contexts, Jinja golden outputs (`gen_goldens.py` regenerates), hand-written SSE scripts, and `fakesushi`, an HTTP replay server for GPU-free testing. See `testdata/README.md`. |

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

## Testing

Everything is offline and read-only against `testdata/`:

```bash
go test ./...
```

`tpl` golden tests diff Go-rendered prompts against real Jinja output from a
sibling `../mini-swe-agent` checkout (skipped when absent).
