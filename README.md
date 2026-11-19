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
| `cmd/mini` | The `mini` CLI: flag parsing, config resolution, `mini config` and `mini sessions` subcommands, trajectory output. |
| `agent` | The linear query–execute loop (port of `agents/default.py` + interactive control flow): step/cost/wall-time limits, format-error retries, trajectory JSON dumping, and session compaction (`compaction.go`): a rare, cache-reading summary event near the window edge - the live context is otherwise append-only. See `docs/compaction-research.md`. A turn's tool calls are a **batch and run concurrently** — sound because `env.Local` is stateless by contract (one fresh shell per action, so no call can depend on a sibling), and confined to the waiting: opening a call, announcing it to the front end, completing its row and appending to the transcript all stay on the agent goroutine, in issue order. `agent.max_parallel_calls` sizes the fan-out (0 = `agent.DefaultMaxParallelCalls`, negative = the whole batch at once); results are always reported in the order they were issued, whatever order they finished in. |
| `env` | Command execution. `Local` interprets bash in-process via `mvdan.cc/sh` (so `[[ ]]`, arrays, heredocs behave identically everywhere); `exec_*.go` handle process plumbing per-OS. |
| `model` | HTTP/SSE client for the sushi server: streaming (`stream.go`), tool-call → action parsing (`actions.go`), cost stats (`stats.go`), the transcript→context projection and token estimation (`compaction.go`), the startup model-info lookup that decides the context window (`info.go`: what `GET /v1/models` reports for this model, else `agent.context_window`, else `model.DefaultContextWindow`), and a scriptable `testmodel`. |
| `tpl` | Prompt rendering: a `text/template` engine for the prompt templates in `mini.yaml`, plus the custom filters they use (`toJSON`, `head`/`tail`/`charlen`, arithmetic). |
| `config` | mini-swe-agent's config layering: recursive merge with an UNSET sentinel, config-spec resolution (file / embedded name / `key=value`), global `.env` handling. Embedded configs live in `config/configs/`; `mini.yaml` is the only supported system config. |
| `session` | The SQLite session store: sessions, turns, and per-call tool records in `$HOME/.mini`; `mini -r` resumes from it, and `mini sessions export` rebuilds a trajectory document from it, and `mini sessions gc` (also run at every startup) collects session trees idle past the retention policy — running a session writes no trajectory file of its own. Its `settings` table is the global half — one row per key of interface state (thinking visibility, window size) that must outlive every session. |
| `ui` | The human end. The interactive UI is the Fyne desktop GUI (`fyneui.go`/`fyneview.go`); `print.go` is the headless `--print` front end. `blocks.go` is the transcript model both render from — the `block` type and the one `messageBlock` mapping every message-to-card conversion passes through, so live and resumed turns cannot drift — and `slash.go`/`interactive.go` carry the slash-commands, modes, and `Interactive`, which adapts a UI to `agent.Hooks` (confirm/yolo modes, whitelists); `prefs.go` is the sticky interface state both front ends share (thinking, window size), read and written through a two-method seam so the UI never imports `session`. The GUI mutes the process logger (`logtarget.go`) while a run is in flight, because nobody reads a terminal the window has superseded. |
| `testdata` | Offline fixtures: hand-written SSE scripts and `fakesushi`, an HTTP replay server for GPU-free testing. See `testdata/README.md`. |

## Usage

```bash
go build ./cmd/mini
./mini -t "add a main() guard to a.py, then submit"        # desktop window
./mini -p -y -t "run 'echo hi' then submit" -o traj.json   # headless yolo run
./mini -c mini.yaml -c model.max_tokens=2048               # config overrides
./mini -cpuprofile cpu.pprof -memprofile heap.pprof -r     # profile the whole run
./mini sessions export -o traj.json                        # write a stored session out
```

Running a session saves nothing but the store: no trajectory file appears unless
you asked for one with `-o` (written after every step, so a crash still leaves the
last complete one) or exported it afterwards with `mini sessions export` / the
GUI's `/export`. An export carries what the store kept — the session's own facts
and its full message list — and no `config` block, because the configuration a
session ran under was never recorded.

By default the config comes from the embedded `mini.yaml`; passing `-c`
replaces that default file. Point `-m`/`--url` at a sushi server (default
`http://127.0.0.1:1234`).

The GUI is compiled into every build, so the toolchain always needs CGO and
the platform's OpenGL/Metal headers (on linux: `libgl1-mesa-dev xorg-dev`).
When stdout is not a terminal — or with `-p` — `mini` skips the window and
runs headless on plain stdout with prompts on stdin, which is what scripts and
the test suite use.

### The window

The transcript is a series of interleaved text areas, one per message type,
each on a subtly tinted, rounded, bordered card — `user` for text the human
typed (right-justified, chat-app style), `harness` for feedback the harness
authored in the user's name (rejections, interruption records, format-error
prompts — left-justified, so the two never read alike), `thinking`,
`tool call` (fixed-width command grid), and `LLM output` (markdown). Each
area's header uses the body's own font and names its kind plus the message's
generated token count, generation time, and cost when non-zero
(`LLM output · 254 tok · 3.1s · $0.02`). The input box stays live whenever
the harness is waiting — including at the "Agent is ready" prompt,
where a new task continues the run — and the only ways to leave are `/quit`,
the native quit hotkey (⌘Q / Ctrl+Q, or the menu), or closing the window;
Enter sends, Shift+Enter (or ⌥Enter on macOS) adds a newline — multi-line
input is a keystroke, not a mode. Thinking toggles with ⌃⌘T on macOS (plain
⌘T stays free for future tabs) and Ctrl+T elsewhere.

Two pieces of interface state are sticky, kept in the session database's global
`settings` table rather than in the config file, which is run configuration that
scripts and `-c` overrides depend on and `--print` should not silently rewrite:
the thinking toggle (`/t` works headlessly too, and both front ends read and
write one key) and the window's size, applied before the window is first shown.
A key that was never written is not `false` — mini keeps its default and
writes nothing, so a preference is stored only once a human expresses it. Placement is deliberately *not* remembered: Fyne
2.8's public `Window` interface has no position getter and no move callback, and
its one write-side call (`desktop.Window.RequestPosition`) may be ignored and
has no coordinate contract across monitors, so a stored position could be
applied but never observed — and a stale one can park the window past an
unplugged display. To forget what is stored: `sqlite3 ~/.mini/sessions.db
'delete from settings'`.

## Testing

Everything is offline and read-only against `testdata/`, and the GUI suite
runs headless against an injected test app — no display needed:

```bash
go test ./...
```
