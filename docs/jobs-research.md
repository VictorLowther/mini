# Background Jobs Research

A survey of how the agentic harnesses checked out beside mini (`~/src/llm_stuff/`)
run commands that outlive a tool call, distilled into approaches with advantages,
disadvantages, and implementation difficulty, ending with the agreed design for
mini's job-control tooling mapped onto its architecture (SQLite transcript store with per-call result files on
disk, one `bash` tool, mvdan.cc/sh in-process interpreter, GUI plus headless
front ends, local sushi server, long autonomous bash turns).

Sources: `crush` (Go), `zero` (Go), `oh-my-pi`/omp (Rust), `qwen-code` (TS),
`little-coder` + its `.pi/extensions/bg-shell` (TS), `mini-swe-agent` (Python).
All findings read from the working copies on 2026-11-15.

**Status.** §1-3 are a survey. §4 is the agreed design for mini's job-control
tooling, fixed on 2026-11-15 in the implementation review that turned it from a
recommendation into a spec. Six decisions in §4 are settled and no longer open
for debate: a job is a *run* that happens to be a tool call today and a
subagent tomorrow; timeouts are the model's and have no upper bound; the
foreground wait is auto-backgrounded on a fixed threshold; every backgrounded
job owns a monitor whose exit rule is always armed; text-match rules are added
and removed by tool call after start; the model can block on monitors with
first-fire-wins; and firings that nobody waits on batch up to the next
injection point. Survey rows that disagreed with those decisions were corrected
in place rather than quietly edited, because the disagreement is the useful
part. §2.1 also carries an empirical correction: the mechanism mini originally
documented for why `nohup x &` dies is not the real one, and `setsid` does not
exist on macOS.

---

## 1. The design space

mini's current contract is one tool, one command, one 30-second budget
(`env/local.go:18` `DefaultTimeout = 30 * time.Second`, applied at
`env/local.go:55` with the comment "yagni: no per-call timeout override; nothing
in the reference passes one"). Every harness surveyed has moved past that, in
seven independent decisions worth naming because they are chosen separately and
combine freely:

1. **Who decides a command is long-running.** The model declares it
   (`run_in_background`, `is_background`, a `ShellStart` tool), the harness
   decides for it (auto-background after N seconds, promote-on-timeout), or the
   shell does (`cmd &`, which is only an option when the harness owns the shell
   like omp does). Declaring costs model attention and training-data habits;
   auto-deciding costs the model its ability to reason about which of its own
   calls will come back as a job id.
2. **How the model finds out what happened.** Pull (a poll/output tool the model
   calls when it wants), push (the harness wakes the agent when a job exits or
   matches a pattern), or block-with-deadline (`wait: true` / `block: true` with
   a timeout, returning current output rather than failing when the deadline
   passes). Push is the only one that does not cost a turn; it is also the only
   one that can be abused into a loop.
3. **Where output lives while the job runs.** In-memory capped buffer (crush),
   append-only file on disk (zero, qwen-code), ring buffer of *lines* in the
   harness (little-coder's 5 000). Files win for anything that can outlive the
   harness; line buffers win for what the model wants to *page through*.
4. **What bounds the output when it reaches the model.** Truncate-and-spill with
   a path back to the whole thing (crush, mini's tcr), a digest plus a pointer
   ("last 60 of 10 431 lines, `ShellLog` for more" — little-coder), or a byte
   cap on the notification only (qwen's 8 192). This is the axis where a jobs
   feature quietly destroys the context window a compaction system just saved.
5. **Process identity and reaping.** Kill the whole process group so nothing
   outlives the call (mini, zero `hardenProcessLifetime`, mini-swe-agent's
   `_run`), or build explicit machinery for things that do: a watchdog baked
   into the job (little-coder), session-scoped reaping with documented
   semantics (omp), `KillAll`/`KillRunning` on shutdown (crush, zero). The
   version of this that nobody gets right by accident is **PID reuse**: zero
   checks the PID still belongs to the task before signalling it.
6. **Lifetime across turns, sessions, and restarts.** little-coder states the
   rule best: *"a job must outlive a TURN but must never outlive the SESSION."*
   zero adds the restart case: a task reloaded as `running` is rewritten to
   `error` because process ownership was lost, with a warning surfaced to the
   human rather than a silent lie.
7. **Interactivity.** Only one harness surveyed lets the model write to a job's
   stdin after the fact (little-coder `ShellSend`). Everything else treats a
   background job as fire-and-forget, which quietly means "no REPL, no
   confirm-prompting installer, no interactive git".
8. **Permissions and safety.** A background shell is the same capability as a
   foreground one, so *starting* one must inherit the same gate. little-coder
   registers `ShellStart` in the whitelist gate's `SHELL_TOOLS` set precisely
   because a new shell tool that skipped the gate "would silently reopen issue
   #70"; zero marks `TaskStop` as `SideEffectShell` + `PermissionPrompt` even
   though it is only a kill - and that is the one place in this survey where the
   reasoning does not transfer. A kill can only end runs this session already
   started, and those already passed whatever gate applied to launching them; it
   cannot begin anything, cannot touch a process the harness did not launch, and
   cannot lose data that was not already being written. Gating it makes the
   mitigation slower than the problem it mitigates, which on a single-GPU box is
   the difference between a runaway training job costing one turn and costing
   however long it takes a human to notice. §4 therefore gates `bash` and exempts
   `jobs` and `job_kill`.

The axes combine into three recognisable shapes: **promote** (crush, qwen-code:
one bash tool that becomes a job), **registry** (zero: jobs are first-class
task objects with their own tools), and **shell-native** (omp, little-coder:
the job exists because the shell has jobs).

mini lands on a fourth combination, which nobody surveyed reaches on its own:
**promoted registry with per-job monitors**. Promote, because the model should
never be punished for guessing a command's runtime wrong (axis 1, and the
harness decides the common case for it). Registry, because the object has to
outlive the tool call, the turn, and possibly the process that started mini, and
because a subagent will want the same row shape (axis 6). And a monitor per
job, because mini's workload is hours-long training runs where the interesting
event is *a line in the log*, not the exit: axis 2's push and pull and
block-with-deadline all three, as one mechanism with two consumers, rather than
three mechanisms — and with the pull leg answered by `bash` reading the job's
file rather than by a fourth tool, which is why mini ends up with fewer tools
than every harness surveyed here.

---

## 2. Per-harness profiles

### 2.1 mini — the wall, and the group kill behind it

`model/model.go:90-96` advertises exactly one tool, `bash`, taking only
`command`. Execution (`env/local.go:42-130`) parses with mvdan.cc/sh, runs
in-process, and wraps the caller's context in the 30s budget. On expiry the
cancellation path (`env/exec_unix.go:18-58`) sends **SIGINT to the whole process
group, waits `killGrace = 2 * time.Second`, then SIGKILLs the group**; the result
comes back `returncode: -1` with `exception_type: "TimeoutExpired"`
(`env/local.go:120-127`), mirroring Python's `subprocess.TimeoutExpired`.

Two consequences the model has to work around today, both easy to miss:

- Partial output survives a timeout (the Result carries what was written), but
  the shape of the error is indistinguishable from "your command hung", so the
  model's learned behaviour is to retry smaller rather than to detach. Note the
  asymmetry at `env/local.go:114-117`: when the *caller's* context is done, the
  function returns `Result{}, ctx.Err()` and the observation is dropped
  entirely, while the deadline branch at `:118-127` keeps it. Any promotion
  path has to be a third case, distinct from both.
- Nothing the model can write inside a `bash` command detaches successfully.
  `nohup long_job &` dies at the deadline, and this was originally documented
  here as "the job stays in the shell's process group, so the group kill takes
  it down with the shell; surviving it requires `setsid`". That explanation is
  wrong in three ways, all of which matter to the implementation:

  1. There is no shell to belong to. mini interprets in-process, and every
     external command **already** runs as its own session leader:
     `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}`
     (`env/exec_unix.go:46`, present since the initial port `a8ec4d6`). The
     group kill at `:50-58` therefore signals the backgrounded job's *own*
     group, not a shared one.
  2. `setsid` is not a thing to discover: **the binary does not exist on
     macOS** (`command not found`, exit 127; absent from `/usr/bin`, `/bin`,
     and `/opt/homebrew/bin`). The knowledge tax is not a tax the model can pay
     on mini's only platform. The session the design needs has to come from Go,
     via `SysProcAttr`, not from a command line.
  3. The real reason `&` fails is the interpreter. mvdan runs a background
     statement as an *in-process* subshell on the same context:
     `r2 := r.subshell(true)` then `go r2.Run(ctx, &st2)`
     (`interp/runner.go:307-328`, v3.14.1). The call's deadline, and the
     `defer cancel()` when `Execute` returns, both still reach the handler and
     still `kill(-pgid)`. A shell job list is not a job system.

  Verified against `env.Local` with a 2 s timeout, checking for survivors after
  `Execute` returned: `nohup sleep 12 >f 2>&1 &` — killed;
  `(nohup sleep 12 >f 2>&1 &)` — killed; `setsid ...` — 127, not found;
  `bash -c 'nohup sleep 12 >f 2>&1 &'` — **survived**, because the external
  `bash` exits immediately, its handler's `defer stop()` (`exec_unix.go:60`)
  deregisters the cancel func, and nothing is left to signal the group.

  The lesson for §4 is precise, and it is about *lifetime*, not about which
  shell answers: a job cannot be produced by running a command through the
  interpreter and hoping it escapes, because the escape has nowhere to go. What
  the manager must own is the **context**, so the run outlives the call that
  started it while staying inside the same interpreter that serves the
  foreground. And it must not be sold as "the setsid fix", because that half is
  already done.

### 2.2 mini-swe-agent — the same wall, inherited

`src/minisweagent/environments/local.py:16` is `timeout: int = 30`, and `_run`
(docstring, line 73) is "Like subprocess.run, but kills the whole process group
on timeout so no children are orphaned". No jobs, no background, no per-call
override. It is the honest baseline, and mini ports it faithfully — the
difference is that mini runs on hardware where the interesting commands are
hours long.

### 2.3 crush — auto-background on one bash tool (Go, same interpreter as mini)

The closest prior art, because crush builds this on top of the *same*
`mvdan.cc/sh` interpreter mini embeds (`internal/shell/` wires
`mvdan.cc/sh/x/coreutils` and a `processGroupExecHandler(killTimeout)` at
`exec_unix.go:44`).

The bash tool's parameters (`internal/agent/tools/bash.go:26-31`) keep one tool
and add two knobs:

- `run_in_background` — the model declares it;
- `auto_background_after` — seconds before the harness decides for it
  (`DefaultAutoBackgroundAfter = 60`, applied with `cmp.Or` at `bash.go:397`).

The foreground path (`bash.go:386-451`) starts the command under a **detached
context so it can survive being moved to the background**, then waits on
completion, the threshold, or cancellation. If the threshold wins, the tool
returns instead of killing: *"Command is taking longer than expected and has
been moved to background. Background shell ID: … Use job_output tool to view
output or job_kill to terminate."* There is a fast-failure grace check first
(`bash.go:373`) so a command that dies in 400ms is reported as the failure it
is rather than laundered into a job.

Two companion tools: `job_output` (`job_output.go:36-90`) takes `shell_id` and
`wait`, prints `Status: running|completed` plus `Exit code N`, spills through
`TruncateOutput(output, spillDir)`, substitutes a `BashNoOutput` placeholder for
empty output, and attaches response metadata (`shell_id`, `command`,
`description`, `done`, `working_directory`) so the TUI can render the job without
re-reading the text. `job_kill` takes `shell_id`.

The registry (`internal/shell/background.go:20-66`) is **in-memory only**:
`BackgroundShell{ID, Command, Description, Shell, WorkingDir, ctx, cancel,
stdout *cappedBuffer, stderr *cappedBuffer, done chan, exitErr, completedAt
atomic.Int64}`, with `Start(..., maxJobs)`, `prune()` (which counts what is still
running), `take/Get/Remove/Kill/List/KillAll`. That is crush's trade: excellent
ergonomics for one session, nothing that survives a restart, and a capped buffer
rather than a file — so a 400MB build log is not a design question for them,
while it is one for mini (whose tcr files already solve it).

### 2.4 zero — a task registry with honest ownership (Go)

zero keeps `bash` foreground but gives the *model* a timeout dial
(`internal/tools/bash.go:52`): `timeout_ms`, **default 120 000, max 600 000,
min 1** (`bash.go:18-19`) — flexible budgets without any jobs machinery at all,
and the cheapest idea in this survey. Its foreground command also calls
`hardenProcessLifetime(command)` with the rationale at `bash.go:159-160`: *"Kill
the shell as a process group on timeout and bound the post-kill I/O wait, so a
backgrounded child cannot outlive the command or hang Run()"* — and it captures
into `newBoundedBuffer` on both streams.

Jobs are a separate, first-class subsystem: `internal/background/manager.go`
(623 lines) is a task registry, and the interesting content is what it refuses
to lie about.

- `Task` (`:26-40`): `ID`, `Type`, `SpecialistName`, `Description`, `ParentID`,
  `PID`, `Status`, `OutputFile`, `StartedAt`, `CompletedAt`, `ExitCode`. Statuses
  are `running|completed|error|killed` (`:17-23`). IDs are validated against
  `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$` (`:66`) because they become path names.
- One JSON metadata file per task under a 0700 root, `persistTaskLocked`
  (`:480`), reloaded at open (`loadTasks`, `:384`).
- **Reload cannot resurrect a process.** A task that comes back `running` is
  rewritten to `error` with the note *"marked reloaded running background task
  %s as error; original process ownership was lost"* (`:475`) and the string is
  collected in `LoadWarnings()` (`:118`) for the human. This is the single most
  transferable idea in the package.
- **Kill is not `kill(pid)`.** `killTarget` (`:309`), `isRunningPID` (`:325`),
  `markKilledIfStillRunning` (`:336`) and `restoreRunningAfterFailedKill`
  (`:367`) exist to avoid killing an unrelated process that recycled the PID, and
  to put the task back to `running` if the signal did not land.
- Parent/child structure: `ListByParent` (`:525`) supports tree-shaped work;
  `KillRunning()` (`:296`) is the shutdown sweep.

The model-facing surface is three tools. `Task` spawns a *specialist* (zero's
sub-agent) rather than an arbitrary shell command, which is why `TaskOutput`
(`output_tool.go:41-75`) can be so simple: `task_id` required, `block` (default
false), `timeout` 30 000 ms. `readOutput` slurps the whole file and runs it
through `summarizeTaskData(data, exitCode)`, returning `Status: …` text plus
`Meta{task_id, status}`; `blockOnOutput` polls `manager.Get` on a ticker and,
crucially, **returns the latest output on ctx-cancel or timeout instead of
erroring** — a blocked read never loses the run. `TaskStop` (`stop_tool.go:33-48`)
is `SideEffectShell` + `PermissionPrompt` with the reason "Terminates a
background specialist process". Output-size handling for these tools is
centralised (`internal/tools/output_boundary.go:138` special-cases `Task` and
`swarm_collect`).

### 2.5 qwen-code — two launch paths, one truth, and a status file (TS)

The largest and most production-scarred implementation:
`packages/core/src/tools/shell.ts` (7 231 lines) plus
`packages/core/src/services/backgroundShellRegistry.ts`.

Two ways to get a background shell, deliberately kept from drifting:
`is_background: true` (`shell.ts:2869,3278,3510`) and **foreground→background
promotion**. Promotion is implemented as a *purpose-built abort reason*: the
foreground run carries a `promoteAbortController` whose `abort({kind:
'background', shellId})` hands the still-running child to the registry instead of
killing it, so the executor returns `result.promoted: true` and the display says
`Promoted to background: <shellId>` (`shell.ts:3529-3537, 5400`). The two paths
share a `settle` helper and a `origin: 'promote' | 'background'` tag
(`shell.ts:2281`), and even cancellation is path-specific —
`PROMOTE_CANCEL_SIGKILL_TIMEOUT_MS = 200` (`shell.ts:2251-2257`) because a
promoted-then-cancelled child needs a different grace than a plain one.

The registry: `ShellTask` entries with `BackgroundShellStatus`, capped
retention (`MAX_RETAINED_TERMINAL_SHELLS = 32`, `:163`), bounded notification
payloads (`MAX_NOTIFICATION_OUTPUT_TAIL_BYTES = 8 192`, `:41`), an
output-tail reader that respects UTF-8 boundaries
(`readBackgroundShellOutputTail`, `:77`, with the continuation-byte check at
`:100`), plus register/notification/status-change callbacks that feed a `/tasks`
dialog and a background-tasks UI with live per-entry updates.

Two pieces of model-facing prose are worth stealing verbatim in spirit:

- **Liveness must not be inferred from output** (`shell.ts:150-162`, shared by
  both launch paths so they cannot diverge, citing #7626). Each background run
  gets a *separate status file* next to its output file
  (`statusFilePathFor(outputPath)`, `:237`) holding
  `status: running|completed|failed|cancelled`, and the guidance says: *"Do NOT
  infer liveness from the output file: programs often block-buffer stdout when
  not attached to a TTY, so an empty output file is normal while the process is
  alive (for live output, re-run with e.g. `python -u` or `stdbuf -oL`)."* Any
  harness that shells out to a GPU box will learn this the expensive way.
- **A long foreground run should teach, not just report**
  (`buildLongRunningForegroundHint`, `shell.ts:2455-2470`): after a slow
  foreground command it appends forward-looking guidance to use
  `is_background: true` next time — and immediately defuses the danger that
  creates: *"(This is forward-looking guidance for FUTURE invocations — do NOT
  re-run the command that just completed; for stateful operations like deploys,
  migrations, or git push, that would cause double side effects.)"*

It also guards the silly cases: `stripTrailingBackgroundAmp` (`shell.ts:168-178`)
removes a bare trailing `&` from a background-mode command while leaving `&&` and
escaped `\&` alone in linear time, and foreground sleeps over ten minutes are
refused with a pointer to `is_background` or the separate `Monitor` tool
(`shell.ts:2550`) — a watch-for-condition primitive rather than a poll loop.

### 2.6 little-coder / pi `bg-shell` — wake on events, not on a timer (TS)

A 535 + 336-line extension, and the most *agent-shaped* design surveyed. Its
header states the thesis: *"Background shell jobs that wake the agent on events
in the job, not on a timer. See jobs.ts for why polling is the wrong primitive
here."*

Five tools: `ShellStart` (`command`, `label`, `wake_on`), `ShellList`,
`ShellLog` (`id`, `lines` default 60 max 400, `offset` absolute "omit for the
tail"), `ShellSend` (`id`, `text` — stdin injection, unique in this survey),
`ShellStop` (`id`, or the literal `'all'`).

`wake_on` is the interesting schema: `exit` (default true), `match` (regex or
literal strings — "worth waking for, e.g. `['Traceback','val_loss=']`"),
`silence` (a duration string), and `every_n_matches` to throttle a chatty
pattern. The tool description is written to change model behaviour directly:
*"Use this instead of bash for anything that takes minutes (training, builds,
installs, servers, watchers) — bash blocks until the command exits. Declare what
is worth interrupting you for in wake_on; you will be told automatically when it
happens, so do NOT poll the job in a loop."*

The discipline lives in `jobs.ts`:

- `DEFAULT_RULES = { exit: true, match: [] }`; **permissive** rule normalisation
  ("a malformed field falls back to its default rather than failing the whole
  call, because a job that failed to start is strictly worse than a job with one
  rule missing"), and `parseDuration` that returns **null on a typo** so
  `silence: "10 mintues"` disables that rule loudly instead of silently meaning
  0 ms.
- `MIN_WAKE_INTERVAL_MS = 15_000`: *"Never wake for the same job more often than
  this, whatever the rules say. A job that matches its own error pattern every
  second must not be able to drive the agent in a loop."*
- `evaluateSilence` fires only for a job that produced output first, and only
  once per quiet period — a compile that has been silent since it started is not
  stalled.
- `MAX_BUFFER_LINES = 5_000`, explicitly: *"The buffer is the model's ability to
  look back, so it has to be generous — but it lives in the harness, never in the
  context window, so generous is cheap."*
- `WAKE_CONTEXT_LINES = 12` around a matching line.
- `formatWakePayload` is a bounded digest — label, id, reason, command, elapsed,
  exit code, "last N of total lines" in a fence, and for a non-exit wake a
  pointer: `ShellLog({"id":…}) for more. The job is still running.` The doc
  comment: *"Deliberately NOT the whole log: a progress bar can be a hundred
  thousand lines, and dumping it would destroy the context window this project
  exists to conserve."*
- **Delivery lanes** (`laneDelivery`, `jobs.ts:244-252`) map urgency onto the
  host agent's real injection points: `now → steer` (after the current tool
  calls, before the next LLM call, starts a turn), `soon → followUp` (once the
  agent has no more tool calls), `idle → nextTurn` (queued; interrupts nothing,
  triggers nothing). Urgency is a *routing* decision, not a boolean.

Process discipline (`index.ts:25-40, 375-390`): `MAX_JOBS = 8` with an error that
names the remedy ("Stop one with ShellStop first"); `STALL_TICK_MS = 30_000` as
the only periodic element; spawn `detached: true` so the job owns its process
group — because *"a `python train.py` under bash is a grandchild — killing only
the shell would orphan the thing actually holding the GPU"* — plus
`wrapWithParentWatchdog(command, process.pid)`, which pushes the orphan guarantee
*into the job*: a watchdog polls the parent pid and takes the whole group down
with `kill -TERM 0` when the parent vanishes, because signal handlers cannot
cover SIGKILL, a crash, or a terminal that disappears. Normal completion kills
the watchdog and preserves the command's exit status. Reaping on
`session_shutdown` uses SIGTERM→SIGKILL escalation, with a comment recording that
an earlier version gated on `proc.killed` and so "could never actually fire"
(PR #102).

### 2.7 oh-my-pi — jobs belong to the shell (Rust)

omp wrote its own bash (`crates/pi-shell`), so jobs are a *language feature*
rather than a tool set, and the documentation of lifetime semantics is the
clearest anywhere (`session.rs:44-56`):

- A cancel targets the in-flight command only; **background jobs an earlier
  command left running are spared by the cancel but ended when the session
  closes.**
- What `nohup` started is **detached**: neither cancel nor closing the session
  ends it (`tools/nohup.rs:1-10`, which also documents its deviations from
  coreutils: SIGHUP is *not* masked, output is not redirected to `nohup.out`,
  and a signal aimed at the nohup'd job still reaches the command).
- **Subshell jobs that run in-engine work** (builtins, loops) end with the
  subshell via `EngineConfig::end_orphan_jobs`, because "no process stands for
  them, so nothing could reach them afterwards"; a job that is just an external
  runs on as its own process, as in bash.
- Session exposes the accounting a harness needs — a count of background jobs
  still running an external process — and ships `timeout` and `nohup` as tools
  alongside it.

The lesson is not "write a shell"; it is that **every case distinction above has
to be answered by someone.** Decide it in the shell and the semantics are
uniform; decide it in the tool layer and the semantics are what the prompt says
they are.

---

## 3. Cross-cutting components ranked by value-per-line for mini

| Component | Seen in | LOC ballpark | Pays off for mini? |
|---|---|---|---|
| Model-settable per-call timeout (`timeout_s`) | zero `bash.go:52` (120 s default, 600 s max) | ~15 | **Yes, first, minus the clamp.** Deletes mini's "yagni: no per-call timeout override" and covers a large share of the pain with no new state at all. Take zero's *dial* and not zero's *ceiling*: §4 decision 2 forbids an upper bound, so the 600 s max is the one part of zero's shape that is rejected. |
| Do not let the group kill destroy a job the model *meant* to detach; document the escape | mini `env/exec_unix.go:50-58`, zero `:159-160` | prompt text | **Yes, but re-scoped.** `Setsid: true` is already in place (§2.1), so this is not a process-group fix: the fix is running a job on a **manager-owned context** through the same interpreter, because `&` inherits the call's ctx via `interp/runner.go:307-328` and dies with it. Cancelling that context already signals the whole group of whatever the job has in flight. The prompt-text half is still owed, and it must not recommend `setsid`, which does not exist on macOS. |
| Auto-background a foreground call past a threshold, returning a job id + next-step text | crush `bash.go:386-451` (default 60 s) | ~60 | **Yes — and this is the spine, not an option.** crush's threshold becomes mini's fixed 30 s foreground budget (§4 decision 3), so today's `DefaultTimeout` survives as the handoff point instead of the kill point. The fast-failure grace is what makes it not lie. |
| Output stays in the tcr result file; the job tool *pages* it (offset/lines, tail default) | little-coder `ShellLog`, mini `model/view.go` | ~40 | **The file, yes; the paging tool, no.** mini's reader is `bash` plus the path, which the system prompt already teaches (`config/configs/mini.yaml:57-59`), so this row's first half is the design and its second half is deleted in §4 — see §"Why there is no `job_output` tool". Full output already lives on disk per call; a job is a row whose file keeps growing. crush/qwen had to build buffers; mini already has the file. Both halves of the read exist: `ReadView(path, offset, end)` (`model/view.go:140`) for byte ranges and `countNewlines` (`:116`) for the line numbers a `match` firing has to report. The one rule that keeps this safe is append-at-EOF, because result paths are *derived*, not stored (`session/tcr.go:12-16`), and rows carry fixed `tcr_offset`s that a rewrite would invalidate. The one part still owed is `output_path` on the `jobs` listing, because a path the model has never been given is a path it cannot read. |
| Status is asserted, never inferred from output (status file or DB column) | qwen `statusFilePathFor` + the block-buffering guidance | ~30 | **Yes, and now load-bearing.** With monitors as the push mechanism (§4), a block-buffered program whose output file is empty looks identical to a stalled one — the exit rule still fires, but `match` silently never does. sushi-box commands are exactly the block-buffering kind, so `-u`/`stdbuf -oL` belongs in the tool description. |
| Bounded wake/read payloads: digest + "last N of M lines" + pointer to the pull tool | little-coder `formatWakePayload`, qwen 8 192-byte tail | ~40 | **Yes.** Without it, a jobs feature undoes the context economy mini just bought with compaction. mini's sizes are already fixed by the observation window (`model/view.go:16-19`: 100 lines / 10 000 characters), so the digest budget is "comfortably under one window", not a new constant invented from nothing — and it is the reason there is no read tool to bound (§4). |
| Registry that survives restart, with "reloaded as running ⇒ error + warning" | zero `manager.go:475`, `LoadWarnings` | ~120 | **Yes.** mini already has SQLite. "One migration" was too glib: the schema has no version tracking at all yet (`session/store.go:26-28`, deferred until the first breaking change), so a `jobs` table is one more `CREATE TABLE IF NOT EXISTS` in the same const and the ownership sweep must be Go code run at `session.Open` (`session/store.go:94`), not SQL. The ownership rule is what keeps it honest. |
| `wait`/`block` with deadline that returns latest output instead of failing | crush `job_output`, zero `blockOnOutput` | ~25 | **Yes, promoted to `job_wait`.** Cheapest possible "sleep" primitive, and it never loses the run. §4 decision 5 generalises it from "wait on one job" to "wait on the first *monitor* across a set" — the crush/zero rule that a deadline answers with state rather than an error is what makes that safe to call in a loop-free way. |
| Job cap with an actionable refusal | little-coder `MAX_JOBS = 8`, crush `Start(..., maxJobs)` + `prune()` | ~10 | **Yes.** Local GPU: two `torch` runs are already one too many. |
| Kill the *group*, with PID-reuse and failed-signal handling | zero `killTarget`/`isRunningPID`/`restoreRunningAfterFailedKill`, little-coder `kill -TERM 0` | ~80 | **Yes.** mini already signals `-pgid`; it has nothing for PID reuse. |
| Wake-on-event (exit/match/silence) delivered as a queued message | little-coder `wake_on`, `MIN_WAKE_INTERVAL_MS` | ~200 | **Yes — promoted from "second phase" to the core of the design.** §4 decision 4 makes it one always-armed exit rule plus model-managed text matches on a per-job monitor, rather than a launch-time `wake_on` argument, because the model only learns that `val_loss=nan` matters forty minutes into a run. mini's `Loop.QueueMessage` (`agent/agent.go:213`) is already the delivery lane; the throttle is a mandatory companion, the silence rule is deferred. |
| Delivery lanes (steer / follow-up / next-turn) | little-coder `laneDelivery` | ~30 once wakes exist | Medium — mini has one injection point today; add the enum when the second appears. |
| Parent watchdog baked into the job | little-coder `wrapWithParentWatchdog` | ~30 | Medium — mini is a foreground TUI/GUI that reaps in-process; the case that needs it is a crash, and mini's GUI can `KillRunning`-style sweep on next start instead. |
| stdin injection (`job_send`) | little-coder `ShellSend` (only harness with it) | ~40 | Medium — genuinely enables REPLs/installers, and it is the one feature that turns "background" into "detached interactive session". Defer: it multiplies the states the GUI must show. |
| Monitor/watch-for-condition tool distinct from jobs | qwen `monitor.ts` | ~300 | **No — subsumed.** Once every job owns a monitor with removable text matches, "watch for a condition" is `bash {background: true, command: "until ..."}` plus a match rule, or a monitor on an existing job. A second, parallel watching primitive would only drift from the first. |
| Promote-on-timeout as an abort *reason* | qwen `promoteAbortController` | ~100 | Low — crush's select-on-threshold achieves the same from the outside in half the code, and mini's interpreter boundary makes an abort-reason plumbing awkward. |
| Session-close vs cancel semantics, written down | omp `session.rs:44-56` | policy, 0 | **Yes — the documentation is the feature.** Pick the rule and put it in the tool description. |
| Coaching hint after a long foreground call, with the double-side-effect caveat | qwen `buildLongRunningForegroundHint` | ~25 | Yes — cheap, and the caveat is the part that makes it safe. |
| Background tasks surfaced in the human UI (`/tasks`, TUI dialog, GUI card) | qwen, crush (metadata on the response) | ~150 GUI | Yes late — mini's footer is already fed by `Snapshot`; a live-job count belongs there before a full dialog does. |
| Reuse the same permission gate for the new shell tool | little-coder `SHELL_TOOLS`, zero `TaskStop` prompt | ~10 | **For launching, mandatory; for ending, refused.** A backgrounded `bash` is the same capability as a foreground one and prompts identically. `jobs` reads a table and `job_kill` can only stop work already permitted, so neither asks - see §1 axis 8. `Action.NeedsConfirmation()` is the one-line statement of that policy, with a table of tests on it. |

---

## 4. The design: a promoted registry with per-job monitors

This section is the spec, not a proposal. Six decisions are **settled** and are
labelled as such; everything else is implementation detail derived from them.
The survey rows in §3 that disagreed with a settled decision were corrected
rather than the decision.

### The six settled decisions

1. **[settled] A job is a run, not a tool call.** On day one the registry holds
   exactly one kind of run — a `bash` tool call that outlived its foreground
   budget — but the mechanisms have to be general enough to hold a **subagent**
   when subagents land, which is the next big ticket item after this one. The
   test used while writing this: *nothing in the registry, the monitor engine,
   the wait path, or the injection path may know that a job has a pid.* See
   §"Extensibility" for what that forbids and what it costs (almost nothing).
2. **[settled] Timeouts are the model's, and there is no upper bound.**
   `timeout_s` is optional. Absent means the model expressed no opinion. There
   is deliberately **no clamp** — zero's 600 s max (`bash.go:18-19`) is rejected
   here, and with it the row this doc used to carry ("clamped to something like
   1–600 — zero's shape"), which contradicted the first deployment fact below:
   interesting commands are hours long. A ceiling on a training run is not a
   guardrail; it is a bug that fires at 3 a.m. The guardrail is no longer the
   deadline, it is reaping (§"Process discipline").
3. **[settled] Jobs may start backgrounded, and any foreground call becomes a
   job on a fixed threshold.** The threshold is **30 s** — crush's mechanism
   (`auto_background_after`, `bash.go:386-451`) at mini's existing number
   (`DefaultTimeout`, `env/local.go:18`), so today's experience is unchanged up
   to the wall and what happens *at* the wall changes from a kill to a handoff.
4. **[settled] Every backgrounded job owns a monitor, and its exit rule is
   always armed.** Text matches are specified by the model, are pattern-based,
   and can be added and removed after the job has started — which is the whole
   reason this is a mutable object with its own tool rather than little-coder's
   launch-time `wake_on` argument: forty minutes into a run is when the model
   learns that `val_loss=nan` was worth interrupting for, and by then it must be
   able to arm that rule without restarting the job.
5. **[settled] The model can wait for monitors; the first to fire wins.** The
   report is causal, not just "done": which job, which rule, whether the job
   exited and with what status, and for a text match the **line number** plus
   the matched text.
6. **[settled] When the model is not waiting, firings batch up** and are
   injected at the next point the harness can append to the conversation — the
   tool-loop boundary, which mini already has.

### The deployment facts behind them

1. **The interesting commands are hours long, and the box is the bottleneck.**
   mini exists to drive a single Mac with one GPU: training runs, quantisations,
   server starts, soak tests, `llama.cpp` builds. A 30-second budget is not a
   guardrail for this workload, it is a wall in the middle of the reason for the
   tool. And an abandoned-but-running job is worse than a slow one: it holds
   memory nothing else can reclaim.
2. **mini already has the two hardest pieces of a jobs subsystem.** Every tool
   call already writes its complete merged output to a durable, path-named file
   (`<tcrBase>/<session>/<tcno>.log`, derived at `session/tcr.go:12-16`), and the
   store is an append-only SQLite transcript with per-call rows and a
   byte-offset view for the model. crush holds capped buffers and qwen invents
   output files because they do not have this; for mini a job is *a row whose
   file is still growing*. The read side is already written too:
   `ReadView(path, offset, end)` and `countNewlines` in `model/view.go:116-168`
   are a byte-range paged reader and a line counter, which is half the monitor
   engine — and the other half of what a bespoke reader tool would do is already
   handled, because `config/configs/mini.yaml:57-59` already teaches the model
   to read a result file with `sed -n`, `tail`, or `grep`, states the size of the
   window it gets, and tells it to `wc -l` before guessing. A job's file is that
   same kind of file.
3. **Context is the expensive resource, and compaction just made it
   tractable.** A jobs feature that dumps a progress bar into history
   undoes `docs/compaction-research.md` §4. So reads are always bounded with a
   pointer to more, and every wake is a digest.
4. **mini has a turn-boundary injection primitive already.**
   `Loop.QueueMessage` appends a user message before the next model call
   (`agent/agent.go:210-216`) — which is exactly little-coder's `nextTurn` lane,
   so event wakes cost no new plumbing. The `steer` lane (interrupt mid-run)
   does not exist and should not be invented for jobs; the comment at
   `agent/agent.go:211` calling `QueueMessage` "steering" is about the user
   typing, not about delivery urgency, and should not be read as an
   existing mid-run injection point.
5. **Subagents are next, and they will want a job row.** A subagent run is
   exactly a long-lived thing with an output file, a completion event, a parent,
   and a need to wake the parent without polling. Designing the registry around
   `tool_calls` would mean writing it twice in six months, which is the specific
   failure decision 1 exists to prevent.

### Surface: two parameters and four tools

Keep `bash` as the workhorse. Everything new is named `job_*` so the family is
obvious in a tool list. **The gate divides by whether an action starts work:**
`bash` prompts in confirm mode exactly as it always has - including when
`background: true`, because a background shell is the same capability as a
foreground one - while `jobs` and `job_kill` never prompt. zero marks even its
`TaskStop` as a permissioned side effect; that caution does not survive contact
with a single-GPU box, where the reason a model reaches for `job_kill` is that
something is wasting memory *now*. `Action.NeedsConfirmation()` says so in one
place, and `ui/interactive.go` consults it rather than pattern-matching command
text, which is also what lets a non-shell tool exist without falling through the
whitelist into "prompt for everything".

**There is no alternative to `job_kill`, and that is a structural point rather
than a UX preference.** The interpreter runs in-process, so a single command can
be holding several external children at once - a pipeline, three `&` clauses,
`make -j`, a loop that forks - and the interpreter gives each of them its own
session, which is exactly what makes the harness able to end them. So there is no
process, and no command line, that names "the job" for a `pkill -f` to match; and
a `kind='subagent'` job will have no process at all. Cancellation reaches every
one of them because the run owns the context they were all started under, which
is the capability no shell command can imitate. `TestKillEndsEveryConcurrent-
ExternalOfARun` holds that guarantee: four simultaneous `sleep` children, one
kill, none surviving.

| Tool | Parameters | Notes |
|---|---|---|
| `bash` *(modified)* | `command`, **`background`** (bool, default false), **`timeout_s`** (int, optional, **unbounded above**) | `timeout_s` bounds the *foreground wait*, not the command's life. Description states the 30 s handoff, says an unbounded command is fine, and warns about buffering (below). |
| `jobs` | none, or `id` | ✅ built.  id, kind, state, exit code and reason, elapsed, label/command, **`output_path`, the log's current size and the age of its last write** (not a line count: that is a full scan per listing over logs that are routinely hundreds of megabytes, and `wc -l` on the path is exact and only ever run on the one job being asked about), armed monitor rules, **why it is still around**, and how long since last output. Parent shown as an indent so a subagent tree reads as a tree. The listing is where liveness and location come from; `bash` is how the bytes get read. |
| `job_monitor` | `id`, `add` (patterns), `remove` (patterns), `list` | Add/remove/list text-match rules. `mode` picks `literal` or `regex` (default literal), `every_n_matches` throttle, per-rule min interval. Exit rule is not addressable — it cannot be removed. |
| `job_wait` | `ids` (list or `all`), `timeout_ms` (optional, default 30 000, unbounded above) | Blocks until the **first** monitor across the set fires; returns *state of the set* on deadline, never an error. |
| `job_kill` | `id`, or `all` | Cancels the run's context, which is what ends *every* external command it started; env then does SIGINT → `KillGrace` → SIGKILL per command group. Never prompts, including `all`. An orphaned row from before a restart reports that it is not this process's to signal rather than guessing at a pid. |

Two rules about the parameters that are easy to get wrong later:

- **Match rules are keyed by their pattern text, not by a secondary id.** Models
  are much better at naming a rule by what it matches than at remembering a
  handle, and a pattern is already unique within one monitor.
- **The 30 s handoff in decision 3 is a property of running commands, not of
  tool calls.** `job_wait` must not be auto-backgrounded, or waiting on jobs
  would background a wait on jobs. It gets its own `timeout_ms`, whose default
  is the same 30 s so the rhythm the model learns is one number: *roughly half a
  minute, then you are back in control with a pointer to more.*

### Why there is no `job_output` tool

Every harness surveyed has one, and mini should not. crush, zero, little-coder
and qwen all ship a paging reader because in their architectures the tool *is*
the only door to the bytes: crush keeps a `cappedBuffer` in memory, zero hides a
task's output file behind `TaskOutput`, little-coder's ring buffer lives inside
the extension. mini's job output is a normal file at a normal path, and the
system prompt already tells the model how to read one
(`config/configs/mini.yaml:57-59`: *"read `output_path` with `sed -n`, `tail`, or
`grep` before concluding anything about the omitted part"*, now followed by the
bound itself and the paging recipe, and echoed by the spill warning in the
observation template at `:136-158`). The model does this today, successfully,
for every truncated command. A job is the same file, still growing. What the
design needed was not a tool but **a path in the listing**.

The bound is worth quoting because it is the part that makes the case: the view
is `model.MaxViewLines = 100` lines or `model.MaxViewChars = 10000` characters
(`model/view.go:16-19`), whichever comes first, and it is a *suffix*, so
**asking for more than 100 lines cannot ever return more than 100 lines** — the
re-read comes back truncated to the same window and the turn is spent. A
dedicated reader with its own `lines`/`offset` knobs would have been teaching
the model a second set of paging semantics for a window it already has to
respect.

- **The test for a tool is whether the harness has to help.** Arming rules,
  blocking, killing, and knowing liveness all do. Reading bytes does not — the
  filesystem is already the interface, and `bash` is already the tool that
  speaks it.
- **Two doors to the same bytes drift.** `job_output`'s `lines`/`offset`/
  `from_start`, its tail default, and its "last N of M lines" phrasing would all
  have to agree forever with what `sed -n` and `tail` mean on the identical
  file. mini's whole promise in this design is that a job is a row whose file is
  still growing; a second reader quietly contradicts it by implying the file is
  not directly usable.
- **`bash` is strictly more capable than the tool it replaces.** `grep -c` to
  count epochs, `grep -n` for line numbers, `awk` on the loss curve, `wc -l` for
  the total, `sort | uniq -c` on the failures. `lines`/`offset` is a subset of
  `sed -n` with less room to be clever, and this is a model on a GPU box it
  owns.
- **It serves decision 1.** A subagent's output is a transcript file, so
  `Output() → path` is identical for every kind. A `job_output` tool would have
  needed kind-specific rendering — an exit code for a run that has none, a
  status word for something that isn't a shell. Dropping it removes one tool and
  one place where `kind` would have leaked into the generic path.
- **Context stays bounded by the mechanism mini already has.** Whatever the
  model's `sed` prints is itself subject to truncate-and-spill, so a runaway
  read costs a capped view plus another path; it cannot destroy the window.
  `job_output`'s "hard max ~400 lines" was defending against a failure mode
  already solved one layer down.

The four things `job_output` carried, and where each lands now:

| Its job | Now |
|---|---|
| Tell the model where the bytes are | `jobs` returns `output_path` per job, and the promotion message and every wake digest carry it too, so reading never needs a lookup call first |
| Tell the model whether the job finished | `jobs`: `state`, `exit_code`, `exit_reason`, elapsed, last-output time. **Never inferred from the file** — that is this doc's own buffering point, and crush's and zero's `Status:` line inside a reader is exactly the inference being refused here |
| Bound what reaches the context | truncate-and-spill for pulls; bounded digests for pushes, which stay the harness's job because the model is not the one choosing them |
| Save a turn | Nothing to save: `job_output` and `bash` + `tail` are both one call. It would have saved *schema*, which is the real win |

Residual costs, named so they are not discovered later:

- **The bound moves from mechanism to prose.** A `cat` of a 2 GB log is
  context-safe but I/O-slow, and past 30 s **it is auto-backgrounded** — so
  "read the job" can spawn a job whose output is the reading. Promotion handles
  that correctly (nothing is killed, the bytes are kept, it is killable), but the
  tool description must say *`tail`/`sed -n`/`grep`, never `cat`* on a job file,
  and `jobs` should print the log's current size so the model can choose a
  strategy. A guardrail demoted from code to text is the price; it is small, and
  it is being paid against a pattern that already works.
- **No typed response metadata for the GUI.** crush's TUI renders a job from the
  `Meta{shell_id, done, command}` its `job_output` attaches. mini's GUI will
  read the row and the file itself — free in-process, but it means the job card
  is a real component rather than a renderer for metadata someone else handed it.

### Numbers the prompts quote

Deleting the reader tool exposed a bug that had nothing to do with tools, and it
is worth writing down as a rule for the rest of this feature.

The observation template's spill warning used to say: *"read it with `sed -n
'1,200p' <path>`"* — while `ViewStart` was cutting the model's view to the last
100 lines. The prompt was advising a read that was guaranteed to come back
truncated, in the one message whose whole job is telling the model how to read
what it just lost. Nobody noticed because the number is stated in prose in one
file and enforced in constants in another.

So: **every number a prompt restates must be interpolated, never retyped.**
`agent.TemplateVars` now publishes `view_lines` / `view_chars` from
`model.MaxViewLines` / `MaxViewChars` (`agent/agent.go:305-317`), and both
prompt sites use them. `tpl`'s shipped-template fixture carries the literals and
`agent/prompt_test.go`'s `TestShippedPromptsStateTheWindow` fails if the prompts
stop resolving them, stop matching the constants, or resurrect the 200-line
advice. (`tpl` cannot import `model` to share the consts — `model` imports
`tpl` — which is precisely why the guard is a test and not a symbol.)

Jobs brings its own quotable numbers, and the same discipline applies on the day
each is added rather than as a later cleanup: the 30 s foreground threshold,
`MAX_JOBS` and its actionable-refusal wording, `MIN_WAKE_INTERVAL`, the default
and maximum digest size, `WAKE_CONTEXT_LINES`. All of them belong in
`TemplateVars` next to the view bounds, because each one is a number the `bash`
tool description has to state honestly — and a tool description that lies about
a limit is worse than no description, which is the lesson of mini's original
30-second wall.

One direct consequence for this design: a wake digest is competing with the
model's own next read for the same 100-line budget, so `WAKE_CONTEXT_LINES`
(~12) and the batch cap should stay well inside one window rather than merely
"bounded". The batched message of decision 6 is a *message*, not a tool result,
so nothing enforces that — it enforces itself or it costs a turn per firing.

### The registry: jobs are runs

`jobs` table:

```
id            TEXT PRIMARY KEY   -- ^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$ (zero :66): it becomes a path component
kind          TEXT               -- 'bash' today, 'subagent' later; nothing generic switches on it
session_id    TEXT
parent_id     TEXT NULL          -- job or call that spawned it; the tree zero needs ListByParent for
tcno          INTEGER NULL       -- join to tool_calls, populated ONLY for kind='bash', never dereferenced by generic code
label         TEXT               -- model-supplied "quantise llama 70b", the thing a wake quotes back
command       TEXT               -- kind-specific payload; for subagents, the task description
description   TEXT               -- the model's own note, crush's shape
state         TEXT               -- running|completed|failed|killed|orphaned
pid           INTEGER NULL       -- only for a kind that forks a wrapper; NULL for an in-process run
pgid          INTEGER NULL       -- signalled as -pgid; nil when there is no one pid for the job
output_path   TEXT NOT NULL      -- STORED, not derived; see below
started_at    INTEGER
ended_at      INTEGER NULL
exit_code     INTEGER NULL       -- NULL for a kind whose exit is not a process exit
exit_reason   TEXT               -- completed|failed|killed|orphaned|promoted, and why
updated_at    INTEGER
```

Plus `job_events`: `(id, job_id, monitor_id, seq, fired_at, reason, line_no,
byte_offset, matched, context, delivered_at)`. A firing is a row, not a
callback, so `job_wait` and the batch injector consume the same record and
delivery is at-most-once per firing.

**`output_path` is stored, which breaks a mini convention on purpose.** Result
file paths are never stored today; every reader recomputes them from the session
id and `tcno` (`session/tcr.go:12-16`). A job cannot do that, because its file is
still being written after the row that created it was finalized, and the writer's
lifetime is not the row's. So a job owns its path: `<tcrDir>/job-<id>.log`,
which keeps the file in the directory the session's cleanup already removes
(`os.RemoveAll(<tcrBase>/<uuid>)`, `session/store.go:265`) — which is also a
hazard to handle explicitly: deleting a session yanks the file out from under a
still-running writer, so `session.Delete` must reap the session's live jobs
first, not merely unlink their rows.

**The one invariant that makes this safe: a job may only append at EOF.** Nothing
may rewrite or truncate a file that a transcript row points at. Rows carry fixed
`tcr_offset`s and compaction computes views from them
(`model/view.go:32-65`), so growth past the end is harmless while any in-place
edit would corrupt every existing pointer. This is the whole reason mini can
reuse its ledger instead of building crush's `cappedBuffer` or qwen's parallel
status files.

**A job's existence is never a fact the model has to keep in its context.** The
table, not the transcript, is the source of truth for live jobs: `jobs` is cheap
and always correct, so a wake that compaction later summarizes away costs a tool
call and not a lost run. That matters because `docs/compaction-research.md` §4
puts durable state in "the summary and the tcr ledger" and keeps only recency
verbatim — the `jobs` table has to be added to that list of durable state, or
the tail-preservation rule will periodically strand the model's knowledge of
what it left running on the GPU.

Schema has no version tracking yet and `session/store.go:26-28` defers
`PRAGMA user_version` to the first breaking change. An additive table needs
neither: `jobs` and `job_events` are two more `CREATE TABLE IF NOT EXISTS`
statements in the same const. The restart sweep, by contrast, **cannot be
SQL** — see below.

### Monitors: one per job, exit always armed

The monitor is the feature; the tools around it are paging.

- **Lifecycle.** Created with the job, in the same transaction, with `exit`
  armed and `match` empty. There is no job without a monitor and no way to
  disarm the exit rule, so "did the thing finish" is never a polling question.
- **`exit`** fires on the transition to `completed|failed|killed|orphaned`,
  once, carrying status and exit code (nullable, because a subagent's exit is
  not a process exit).
- **`match`** fires on a pattern crossing. Literal and regex both, because
  "val_loss=nan" is a literal and "step [0-9]+/5000" is not, and the model
  should not have to think about which it is writing; `mode` is there for when
  it wants to say.
- **Evaluation is a scan of new bytes only.** A single tick holds no timer per
  job; it reads past `last_scanned_offset`, updates the running line count
  (`countNewlines` already does the walking), and records firings. Three
  properties fall out of that choice: matching is O(new bytes) so a 400 MB build
  log is not a design question; the log never has to fit in memory because it
  already lives on disk; and line numbers are stable across ticks, which
  decision 5 requires. Bound the read per tick, so a job that wrote 200 MB since
  the last sweep cannot stall the loop.
- **Throttles, all of them mandatory companions.** `MIN_WAKE_INTERVAL` per
  monitor, 15 s, whatever the rules say — little-coder's rule verbatim: *"a job
  that matches its own error pattern every second must not be able to drive the
  agent in a loop."* `every_n_matches` per pattern for a chatty step line. A cap
  on patterns per monitor. A cap on queued undelivered firings (qwen's
  `MAX_RETAINED_TERMINAL_SHELLS = 32` is the shape) with **coalescing**: if a
  monitor fires again before its queued digest was delivered, the queued digest
  is *replaced* with the newer state, not appended, and the delivery says how
  many were folded in.
- **Permissive normalisation** (little-coder): a malformed field falls back to
  its default rather than failing the call, because a job that failed to start
  is strictly worse than a job missing one rule; and a duration that fails to
  parse means *this rule is off*, loudly, never 0 ms.
- **Liveness is asserted, never inferred from output.** `state` plus a
  `killTarget`-style liveness check (zero `manager.go:309-336`), not
  "the file stopped growing". And qwen's block-buffering warning
  (`shell.ts:150-162`) now belongs in the `bash` and `job_monitor` descriptions,
  because an empty output file is *normal* for a block-buffered program and
  monitors make that a correctness problem rather than a cosmetic one: `-u`,
  `stdbuf -oL`.
- **Caps.** `MAX_JOBS` per session with an error naming the remedy
  (little-coder's `MAX_JOBS = 8`; on a single-GPU box two `torch` runs is
  already one too many).

### Promotion, and what a deadline means now

Foreground path: run, then wait on completion / the 30 s threshold /
cancellation, **started on a context that can survive being handed to the
manager**. If the threshold wins, do not kill: register the running command as a
job and return crush's message shape — id, what happened, which tool to call
next (`jobs`, `job_wait`, `job_monitor`, `job_kill`) **and its `output_path`**.

- **Fast-failure grace first** (crush `bash.go:373`): something that dies in
  400 ms is reported as the failure it is, never laundered into a job.
- **A model-set `timeout_s` expires into a promotion too**, not a kill. That is
  how decision 2 has to read: the model asked to be told at T, not asked to be
  stopped at T. If it wants a hard kill it can say so inside the command
  (`timeout 20 …`, which exists); the harness deadline is a handoff, not an
  execution. This is the one place where the feedback left an ambiguity and
  §4 resolves it, so it is the thing to overrule if the intent was different.
- **Promotion is a third case, not an interruption.** Today a done caller
  context returns `Result{}, ctx.Err()` and drops the observation
  (`env/local.go:114-117`) while the deadline branch keeps partial output
  (`:118-127`). A promoted call must keep its bytes *and* stay readable, so
  neither existing branch is reusable as-is.
- **Teach, don't just report** (qwen `buildLongRunningForegroundHint`,
  `shell.ts:2455-2470`): after a slow foreground command, point at
  `background: true` next time — and carry qwen's caveat, *"do NOT re-run the
  command that just completed; for deploys, migrations, or git push that would
  cause double side effects."* The caveat is the part that makes the hint safe.

### Delivery: wait or batch, never both

One firing has one consumer, decided by whether a `job_wait` is outstanding when
it lands.

- **Waiting** (decision 5): the first firing across the waited set returns a
  digest — job id, label, command, kind, elapsed, which monitor and which rule,
  `reason: exit|match`, exit status and code when it was an exit, and for a
  match the **line number** plus the matched text with `WAKE_CONTEXT_LINES`
  (~12, little-coder) around it — then "last N of M lines", then the path with
  the read recipe, drawn inside the window so the next move needs no lookup
  call and no re-truncation (`tail -n 100 <path>`,
  `sed -n '4300,4340p' <path>`, `grep -n <pattern> <path>`). Deliberately not the whole log: a progress bar can be a
  hundred thousand lines, and dumping it would destroy the context window
  compaction exists to conserve. If several fired at once, report the earliest
  and leave the rest in the queue. On `timeout_ms` expiry, return the current
  state of the set: crush's and zero's rule that a blocked read never loses the
  run.
- **Not waiting** (decision 6): firings accumulate and drain **at the next
  tool-loop boundary as a single appended user message** via `QueueMessage`. One
  message for the whole batch, N digests inside it, bounded per digest
  (qwen's `MAX_NOTIFICATION_OUTPUT_TAIL_BYTES = 8 192` is the right size class)
  and bounded in total, with a count of what was coalesced away. If the model
  never waits and 20 firings pile up, that is one turn, not twenty.
- **The hard invariant:** nothing in this design may write to the transcript
  except at the tool-loop boundary. A wake is one appended message, like every
  other observation, never an edit to history — the same cache-prefix discipline
  `docs/compaction-research.md` §4 treats as the first principle, and the
  reason a jobs feature can be shipped next to compaction without undoing it.
- `job_monitor` and `jobs` are how the model *describes* what it wants
  to be told about; the tool descriptions should say, in little-coder's words,
  *"declare what is worth interrupting you for; you will be told automatically
  when it happens, so do NOT poll the job in a loop."*

### Extensibility: what makes this work for subagents later

The manager and the engine speak to an interface, and `kind='bash'` is one
implementation of it:

```
ID() Kind() Label() State() StartedAt() EndedAt() ExitStatus()   // ExitStatus is nullable
Output() (path, reader over the append-only file)                 // ReadView-shaped
Kill(grace) Wait(<-chan firing)
```

- Monitors attach to a **job id**, never to a pid, and the scanner reads
  **files**, not `/proc`. A subagent whose "output" is its own transcript file
  and whose "exit" is its loop returning therefore needs no new monitor, wait,
  or injection machinery.
- `exit_code` is nullable and the digest degrades gracefully: `reason
  subagent_completed`, no exit-code line. Making that legal on day one is
  cheaper than retrofitting it.
- `parent_id` plus a `ListByParent`-style query (zero `manager.go:525`) so
  `jobs` can render a tree and so **reaping a parent reaps its children** — the
  one subagent behaviour that would otherwise leak a process per child.
- Process particulars live **inside the kind's own stopper**, not on the generic
  job — and for the bash kind they are not pids at all, because the run is
  mini's own interpreter on a dedicated context and `&` children are sessions
  the exec handler already owns. What a kind owes the generic path is
  `stop(grace)` and nothing else. Note the consequence for `jobs`: it must not
  promise a pid for a `bash` run, because there isn't one, and a model told
  otherwise will `kill` mini.
- What deliberately does **not** generalise: the `tcno` join. Only `kind='bash'`
  display resolves a job back to a tool call, and no generic path may do it.

### Process discipline and lifetime

1. **Own the context, keep the interpreter.** A job is the same
   `mvdan.cc/sh` runner that answers every foreground call — `env.Local`, driven
   on a `context.WithCancel` the manager holds instead of the tool call's. Forking
   `/bin/bash` for background calls was the obvious design and it is wrong:
   the prompts teach mini's bash grammar, the macOS system bash is 3.2, and a
   loop or an expansion that behaves differently only once backgrounded reads
   to the model as the harness lying. The manager-owned context is what §2.1
   actually requires, since `&` cannot escape the call's context (background
   statements run in-process on it, `interp/runner.go:307-328`), and "no
   deadline" is a property of the environment, so `env.Local` gained
   `Timeout < 0` rather than the job path growing its own runner.
2. **Kill by cancelling; the group dies with it.** Cancellation reaches
   `processGroupExecHandler`, which already SIGINTs, waits `env.KillGrace`,
   then SIGKILLs **the group of the command in flight**, because every external
   command it starts is its own session leader (`env/exec_unix.go:46`). So the
   guarantee is per in-flight command rather than one job-wide pgid, and it
   covers what matters: the `python train.py` and the dataloader workers
   forked underneath it. PID-reuse cannot bite, because mini only ever signals
   a process whose start it is sitting on inside the handler — which is a
   stronger invariant than zero's `isRunningPID` check (`manager.go:325`)
   obtained without tracking a pid across restarts. The cost is stated in the
   sweep below: after a crash, nothing is signalled at all.
3. **A job outlives a turn, never a session.** Written into the tool
   description the way omp documents `session.rs:44-56`; the documentation is
   part of the feature, because it is the only place the model can learn the
   rule.
4. **Reap on quit**: every live job of the session, on GUI quit and at
   `--print` end, with the count logged.
5. **A restart cannot resurrect a process.** On store open (`session.Open`,
   `session/store.go:94`) any row still `running` becomes `orphaned` with zero's
   warning surfaced to the human (`manager.go:475`, `LoadWarnings()` `:118`) —
   never silently "still running". Note that this is a Go sweep run at open, not
   a migration, and that the ids it collects are worth keeping in memory for the
   session so the human can be told once.
6. **Do not let a GUI-quit reap surprise a headless run**: see open question 4.

### What removing the ceiling costs

Decision 2 deletes mini's only historic guardrail, and the honest accounting is
worth keeping next to the design. What replaces the deadline: the fixed 30 s
foreground budget, so a runaway blocks nothing; the `MAX_JOBS` cap, so a runaway
cannot be multiplied; the monitor, so the model learns about a stuck job without
polling; the footer count and the `jobs` "why is it still around / how long
since output", so the **human** sees it; reap-on-exit and the `orphaned` sweep,
so it cannot outlive the session and cannot be hidden after a crash.

What is left uncovered is a job that runs forever, stays healthy, never exits,
and never matches: nothing in the settled design notices. little-coder's
`silence` rule (fires only for a job that produced output first, once per quiet
period) is the cheap alarm for exactly this, and it is deferred only because it
was not in the spec — it is the first thing to add back if abandoned jobs turn
out to be the failure mode in practice rather than the one the survey feared.

### Deliberately not built

stdin injection (`job_send`) until the GUI can show what a job is waiting for —
it turns jobs into interactive sessions and the harness into a terminal
multiplexer; delivery lanes beyond `nextTurn` (`steer` does not exist and
`laneDelivery`'s enum can wait for a second lane, little-coder's
`jobs.ts:244-252`); promote-by-abort-*reason* inside the interpreter (qwen's
`promoteAbortController`) when a select-on-threshold gets there from outside in
half the code; a standalone `Monitor`/watch-for-condition tool divorced from
jobs (qwen `monitor.ts`), since "watch for a condition" is already expressible
as a backgrounded command plus a match rule and a second watching primitive
would only drift from the first; **`job_output` itself**, for all the reasons in
§"Why there is no `job_output` tool" — it is the one tool every surveyed harness
has and the one this design deletes; `silence`/stall rules (above); a parent
watchdog baked into the job (little-coder `wrapWithParentWatchdog`), because
mini is a foreground app that reaps on quit and sweeps `orphaned` on start,
which covers the crash case differently; job-to-job stdin pipes; and a
background-tasks dialog, since the footer count is 90 % of its value at 2 % of
its cost.

### Build order

1. **The registry.** ✅ *built* — `jobs` + `job_events` tables, the `Run`/
   `stopper` seam, the manager-owned context over `env.Local`, output to
   `job-<id>.log` in the session's own result directory, the state machine, the
   `running ⇒ orphaned` sweep at open, and reap-on-quit. Foundation everything
   else reads, and safe to ship half-finished precisely because the sweep and
   the reap are live before any job can be started.
2. **Promotion.** ✅ *built except the tools* — `bash` has `background` and
   optional unbounded `timeout_s`; the 30 s auto-background promotes rather than
   kills, and the tool description says so. The fast-failure grace needs no code
   here: a command that dies before the budget is simply `<-run.Done()` winning
   the select. `MAX_JOBS` is enforced at `Detach`. Still owed: `jobs` **with
   `output_path`, log size and last-write age**; `job_kill`. After this step a job is
   spawnable, listable, readable, and killable, the feature already pays for
   itself, and it changes nothing for a model that never hits the wall. There is
   deliberately no read-tool step: the first thing to instrument here is whether
   a model that has never seen jobs discovers `tail`/`sed -n` on the reported
   path without being told twice, because that is the bet this section argues
   for. `docs/compaction-research.md` §4's spill pattern is the evidence it is a
   safe bet, not a hope.
3. **Monitors.** The per-job monitor with the always-armed exit rule,
   `job_monitor` add/remove/list, the new-bytes scan, the throttles, and the
   **batch queue drained at the tool-loop boundary** as one appended message
   (decision 6). Push before wait: this is what saves turns.
4. **`job_wait`.** First-fire-wins over a set, deadline answers with state
   (decision 5), and the at-most-once split against the batch queue.
5. **GUI.** Footer count off `Snapshot`/`runStatsText` (`agent/agent.go:253`,
   `ui/blocks.go:126`) — `2 jobs` is the whole change — then a job card carrying
   the digest a wake produced.
6. **Subagents**, when they arrive, as a second `kind` behind the same
   interface. If that step needs a new monitor, a new injection path, or a
   kind-specific output reader, steps 1–5 were built wrong.

### What is built

Step 1 is in the tree:

- `job` — `Manager` (cap, live set, `Sweep`, `KillAll`), `Run`
  (`State/Alive/Elapsed/Wait/Kill/Done/Row/OutputPath`), `Spec`, and
  `local.go`'s interpreter-backed `Start` and outcome watcher.
- `session/jobs.go` + two tables in `session/store.go` — the registry, the
  first-writer-wins `FinishJob`, the sweep that returns its warnings, and the
  `job_events` firings the monitor engine will write.
- `env` — `KillGrace` exported so a job and a foreground command share one
  number, and `Timeout < 0` meaning no deadline.
- `cmd/mini` — one manager per run over the session's own `env.Local`, the
  sweep with the warnings surfaced, and the reap folded into the cleanup path
  both front ends already share.
- Step 2, **minus the two tools**: `bash` has `timeout_s` (uncapped, parsed
  permissively) and `background: true`, the 30 s auto-background is live, and the
  handoff names the job id, its path, and a `tail -n 100` recipe. Still owed:
  `jobs` and `job_kill`, which need the parser to dispatch a second tool name and
  the confirm gate to cover it.

  Three things the wiring settled that the spec left open:

  - **Promotion requires the run to outlive the wait from the start.** Every
    foreground command now launches through the job manager and is *waited on*
    with a budget; when the budget expires the wait ends and the run does not.
    A design that ran the command normally and promoted it afterwards would be
    promoting a process `env` had already signalled, which is the mistake the
    shape of `env/exec_unix.go` makes unforgivable.
  - **Open question 1 is answered: the call row stays `started`.** The call did
    not complete, it handed off, and the store already models an unfinished call
    (that is the cancellation case). Closing the row with a return code nobody
    measured would put a number in the ledger that the job later contradicts.
  - `bash` also gained `description`, a few words from the model that becomes the
    job's label - the thing `jobs` prints and a wake will quote, which is what
    turns "j3" into "quantise 70b" in anything read later.
  - **A promoted run keeps the call's file**, because the row stores the path it
    has been writing to rather than a new one. So the transcript's `tcr_path` for
    a long command *is* the live job log, `append-at-EOF` keeps every stored
    offset valid, and the model is never told about a second path. Rows are
    created lazily by `Detach`, which is also what keeps the registry about jobs
    instead of a row per `ls` and the cap honest.

  Known gap: *why* a run became a job (backgrounded vs outran its budget) lives
  in the live handle and the log, not the row. The `jobs` listing will read it
  from the handle; persisting it means a new column, and the schema has no
  version tracking yet (`session/store.go:26-29` defers `PRAGMA user_version` to
  the first breaking change). Adding a column to a table already shipped to a
  live database is exactly that first breaking change, so the decision is
  recorded rather than taken quietly.

  Two decisions fell out of wiring `timeout_s` and are worth keeping. The
  per-call budget travels **on the context**, because `Environment.Execute` has
  no timeout argument and a container environment should not have to imitate one
  it cannot implement; a caller deadline now beats `Local.Timeout` in both
  directions, so a model can ask for an hour or for one second. And an expired
  deadline is an **observation, not an interruption** — Execute's
  `ctx.Err() != nil` branch used to swallow it, which would have reported a
  model-set timeout as a Ctrl-C that never happened and discarded the partial
  output that makes a timed-out build worth reading about.

Nothing starts a job yet, which is the point of building the registry first:
the guarantees that are easy to get wrong — ownership after a crash, reaping,
one interpreter, append-only output — are already enforced before the model can
ask for a job. Step 2 (promotion) is where the tool surface appears.

### Open questions to settle before step 1

1. **Does the promoted call's tool result stay open?** The transcript row for
   the `bash` call has to be finalized with the promotion message, and the job's
   eventual completion arrives later as a wake. That is correct for the cache
   prefix, but it means the last *synchronous* observation of a long command is
   "moved to background" — so `jobs` must be discoverable from the summary text
   if compaction eats the wake (durable-state list above).
2. **`job_wait` return shape when several jobs fire together** — earliest only
   plus "3 more pending", or all of them up to the byte cap? Earliest-only is
   the stated default here; it is also the one that keeps the digest bounded.
3. ~~Confirm mode and `all`.~~ **Settled: no prompt, for `all` included.** This
   doc originally said a kill is still a shell side effect and should inherit the
   gate, by way of zero's `TaskStop`. That is wrong, and the mistake is
   instructive: zero's tool stops a specialist it launches *from the tool*, so
   the prompt is where the launch is authorised. Here the launch was already
   authorised when `bash` ran, and `job_kill` only removes work in progress. The
   blast radius is also bounded by construction - it can address jobs in this
   session with a live handle, and nothing else. So `all: true` is the safe
   emergency exit rather than the wildcard that needed guarding.
4. **Headless `--print` with live jobs at the end**: reap (current plan, and
   consistent with "never a session") or gain a `--wait-for-jobs` flag that
   holds the run open until every monitor has fired? A soak test launched with
   `--print` and reaped on exit is the way this feature becomes annoying.
5. **Where `line_no` points.** Absolute line in the job's file is the answer
   this doc assumes, since it is the one coordinate `sed -n 'N,Mp'` and
   `grep -n` can act on directly — with no reader tool of our own to translate
   an offset, a wake that reported only a byte offset would hand the model a
   number it cannot use. `byte_offset` rides along in `job_events` for the
   monitor's own bookkeeping.
