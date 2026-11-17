# Parallel Sessions Research

Status: **built through §7 step 6.** The prerequisite (steps 1-5) and feature 1 -
subagents - are in the tree and tested; what remains is step 7, the multi-session
UI, and the confirmation bubbling it carries (the one thing that makes spawning
from a `confirm`-mode parent refuse rather than over-grant, §5.5). §8 is the
record of what landed and where. §3.7 and decision 13 settle the last structural
seam - how a session asks a person something when it is not in a position to use
its own prompt path. The first draft of this document was written
from mini's own source alone, before the survey in §3, and the survey disagreed
with it in eight places. Those are
recorded in §3.5 rather than quietly edited out, because the disagreements are
the useful part. Twenty rulings followed, in five rounds.

Three overrode the survey's own recommendations - no nesting by tool
invisibility, one global concurrency knob, no workspace isolation - and §3.6
keeps that dissent on the record next to each. Two overrode recommendations this
document had itself just made: §5.11's cascade-and-collect-everything removal,
and §5.8's cost model. One retracted a design already written: §9.9's ruling that
panics stay loud and fatal killed §5's session-boundary containment, which the
first draft had copied from zero. And the last round closed §9.10 by declaring
the question malformed - a subagent is simply a job that backgrounds itself once
its session exists, so no top-level loop behavior changes and §5.12's principle
lands entirely inside the agent kind. **Step 6 is now specified end to end**, and
§7's steps 1-4 are built.

mini has always had exactly one of everything per process: one session, one
`env.Local`, one job registry, one loop, one window. No check anywhere enforces
that; it is a property of `buildRun` (`cmd/mini/main.go:419`), which assembles
all six things in one function and returns one `miniRun`, and of `run`, which
drives exactly one front end from it. That property is what two features need
to lose:

1. **Subagents.** A top-level session spawns an *agent job*: a nested session
   created with a prompt that says exactly what to do, whose final step's text
   is the job's report.
2. **Independent top-level sessions.** Several repositories, several complete
   agents, one `mini` process and one UI.

Both are the same capability wearing different clothes, so the prerequisite is
stated once and built once: **nothing in the core may assume it is the only
running session, and session assembly must be callable more than once.**

---

## 1. Deployment facts that decide it

Three facts about how mini is actually run constrain every choice below, and
they are not true of most harnesses surveyed:

- **One GPU.** sushi is a local server on `127.0.0.1:1234` behind one box
  (`model/sushi.go:23`). Parallel agents do not add throughput; they divide it.
  Anything that "just runs N of them concurrently" is a latency story, not a
  capacity story, and the interesting failure is four long-context generations
  interleaving on one model.
- **Long-running work is the point.** The workload is hours-long training and
  quantising runs (docs/jobs-research.md §1), which is why jobs deliver results
  by push at step boundaries instead of costing turns to poll. A subagent is the
  same shape of object: it is slow, it is fire-and-forget, and its interesting
  moment is the end.
- **The transcript is the product.** Every turn is durable in SQLite with its
  output spilled to disk, and `mini -r` continues a conversation. A nested
  session that is *not* durable and addressable is worth much less, because
  "what did that subagent actually do" has to be answerable after the fact.

## 2. The design space

Ten decisions, chosen separately and combining freely:

1. **Process topology.** N sessions in one process (zero's ACP agent,
   `internal/acp/agent.go:86`); one process per session coordinated by a host
   (oh-my-pi's robomp spawns `omp --mode rpc` per issue worktree,
   `python/robomp/AGENTS.md:16`); or N whole runs in one process's threads with
   no shared object at all (mini-swe-agent's benchmark runner,
   `ThreadPoolExecutor(max_workers=workers)` over `process_instance`,
   `src/minisweagent/run/benchmarks/swebench.py:209,257`). Threads-for-batch is
   what mini already descends from, and it is the honest cheap answer for
   feature 2's *independent* case; in-process is the only answer for
   subagents, which must share a job registry and a UI.
2. **Session identity and lineage.** Nothing (mini today), an in-memory parent
   pointer (crush's `dispatchedAgent.ParentSessionID`,
   `internal/agent/agent_dispatch.go:149`), or durable columns for parent,
   root, kind and depth (zero, `internal/sessions/exec_session.go:139-146`).
3. **Is a child session resumable by a human?** zero says no, loudly: a child
   loaded for viewing is `readOnly` because "a prompt would continue the sub-run
   as a standalone conversation, which every other resume path refuses"
   (`internal/acp/agent.go:105-113`).
4. **Where authority comes from.** zero's rule is the clearest: `MemberSpec`'s
   `PermissionMode`/`Model` "carry the orchestrator's policy so the launcher can
   never grant a member more authority than its parent"
   (`internal/swarm/member.go:11-13`). crush adds an access tier and hides the
   tool for a read-only parent (`readOnlyParent`, `resolveSubAgent`,
   `agent_dispatch.go:383-402`); qwen hides it *and* keeps a runtime backstop
   using the same predicate, "so the two layers cannot drift"
   (`tools/agent/agent.ts:2795-2805`).
5. **How a runaway tree is stopped.** Strip the spawn tool from children
   (fragile: it breaks prompt-cache parity, which is exactly why qwen abandoned
   it, `fork-subagent.ts:47-56`), mark the async frame and refuse at dispatch
   (qwen's `AsyncLocalStorage` marker), or bound the depth with a number
   (qwen's `DEFAULT_MAX_SUBAGENT_DEPTH = 5`, hard ceiling 100,
   `config/config.ts:1932-1934`). qwen's refusal is worth quoting because it
   tells the model what to do instead: *"Complete this task directly with your
   own tools instead of spawning another sub-agent."* (`agent.ts:2814-2820`).
6. **Concurrency bound and default.** A global cap on background agents
   (qwen 10, `background-tasks.ts:54`), a per-model cap with slot reservations
   and wait queues *on top* of it (`background-tasks.ts:663-672`), a team-size
   cap (zero 8, `team.go:17`), or a *total* budget for the whole tree divided
   into lead/wave/reserve and executed **serially by default** (little-coder,
   `.pi/extensions/deep-research/budget.ts:1-24`).
7. **Cost rollup.** crush adds a child's spend to the parent incrementally,
   tracking what has already been rolled up because the child's own total is
   cumulative (`rolledUpCost`, `agent_dispatch.go:173-176,586-591`).
8. **Workspace isolation.** omp builds a whole copy-on-write PAL for this
   (`crates/pi-iso/src/lib.rs:1-19`: APFS `clonefile`, Linux overlayfs, Windows
   ProjFS, and `git worktree` as the portable fallback), and robomp states the
   policy flatly: "The container itself is the isolation boundary; per-issue
   isolation is the git worktree" (`AGENTS.md:114`). zero, crush and qwen share
   the tree and instead gate children toward read-only work.
9. **The report contract.** The child's last message (mini's plan), or its last
   message with the scratchpad removed first - qwen strips `<analysis>` blocks
   and keeps `<summary>`, because "Keeps successful subagent scratchpad tags out
   of the parent model context" (`agents/subagent-result.ts:142-154`), and a
   forked worker's output is otherwise the model thinking out loud.
10. **Lifetime across a restart.** Sweep to a terminal state and say so (mini's
    jobs), re-parent the work onto a fresh member (zero's `AdoptOrphans`,
    `lifecycle.go:377`), or reconstruct the child's *report* from the parent's
    transcript so the parent stops waiting on a report that can never arrive
    (crush's `reconcileDispatches`, `agent_dispatch_status.go:196-227`).

Those combine into four recognisable shapes: **batch** (mini-swe-agent: N
independent runs, no shared object), **roster** (crush, qwen: children are
background tasks of one parent session with a UI to watch them), **tree**
(zero: durable parent/root/depth lineage, members with mailboxes, an RPC server
holding N sessions), and **farm** (omp: process per isolated worktree, patched
back). mini lands on **roster over a two-level tree**, sitting on the promoted
registry: the object a subagent is *is* the object `job` already models, feature
2 needs the durable lineage only the store can give, and §5.6 caps the tree at
parent and child so the lineage is a pair of columns rather than a recursion.

## 3. Per-harness profiles

### 3.1 zero — a session tree with durable lineage (Go)

The closest existing answer to both features at once, and the most careful
about what must be persisted.

**N sessions in one process.** `internal/acp/agent.go:86` holds
`sessions map[string]*acpSession` behind `agentMu sync.Mutex`, and each session
carries its own `turnMu`: "concurrent session/prompt calls run one at a time so
they can't interleave history or clobber the single cancel slot"
(`agent.go:98-101`). `session/cancel` deliberately does *not* take `turnMu`, so
cancelling a turn stays responsive while a turn is held (`agent.go:459-462`).

**Lineage.** A child session row gets `SessionKindChild`, `ParentSessionID`,
`RootSessionID` (firstNonEmpty of the parent's root or its own id), `Depth`,
`AgentName`, `TaskID` and `SpawnedFromEventID` = the calling tool-use id
(`internal/sessions/exec_session.go:131-146`). The root is denormalised so
"everything under this run" is one indexed predicate rather than a walk. The
parent link is surfaced to the human as `parent=<id>` in
`mini sessions`' counterpart (`internal/cli/sessions.go:534`), and redacted in
search like any other identifier (`internal/search/search.go:267`).

**The launcher seam.** `MemberLauncher` turns a `MemberSpec` into a
`MemberHandle{ID, Wait}`; production wires two implementations (a direct
executor, and a daemon pool) and tests inject a fake, "which means the swarm
core has no compile-time dependency on either heavy subsystem"
(`member.go:41-46`). `FuncLauncher` runs the work in a goroutine and recovers:
"A panic in a member's Run must fail only that member, never crash the
orchestrator process" (`member.go:70-78`).

**Policy.** `MemberSpec` is fully resolved before it crosses the seam -
`Model string // resolved (never the "inherit" sentinel)` (`member.go:23`) - so
no launcher can guess differently about what "inherit" meant. Permission mode
and model are copied down from the orchestrator for the reason in §2.4.

**Shutdown, carefully.** Lifecycle work is admitted through a counted barrier:
`beginLifecycleAdmission` hands back a release func Close waits for, and the
comment records the deadlock that motivated the shape - "Close needs the write
lock before it can cancel, so a launcher that blocks in Launch until its context
is cancelled would deadlock... Holding the [admission lock across Launch]"
(`lifecycle.go:38-49`). Waits on children are bounded by
`defaultHandoffStopTimeout = 30 * time.Second` (`team.go:22`), used as
"without letting a cancellation-insensitive member wedge a model tool forever",
and when a cancel and a completion become ready together the completed barrier
wins, because "a stopped source can still commit safely"
(`lifecycle.go:345-374`).

**Leases.** Where mini's store note says "No flock/lease machinery"
(`session/store.go:192`), zero does hold one, and its rationale transfers
almost verbatim: a session holds no lock between writes, so "nothing told
`zero sessions prune` in another terminal that a TUI, an exec run or an editor
still had the session open, and an idle open session looked exactly like an
abandoned one" (`internal/sessions/lease.go:23-28`). Hold "never waits and never
fails its caller" - the cost of a missing lease is that prune cannot see you,
which only matters for reads.

### 3.2 crush — detached subagents as background tasks (Go, same interpreter)

crush's `agent_dispatch` tool is mini's feature 1 almost exactly, and it was
built by somebody with the same instinct about jobs: "launches a subagent that
runs detached from this tool call. The tool returns as soon as the subagent
starts; the result is delivered later as a message into the parent session"
(`internal/agent/agent_dispatch.go:378-380`).

- **Detachment is `context.WithCancel(context.Background())`** on the claim
  (`agent_dispatch.go:207`), the same line mini's `Manager.background()`
  (`job/local.go:518`) reaches, for the same reason: "a subagent outlives the
  turn that started it and is stopped only through stop".
- **Subagents get the bash promotion rule.** `backgroundAfter` is "how long a
  blocking subagent runs before it is moved to the background. It mirrors the
  bash tool's threshold" (`coordinator.go:184-187`). One mechanism, two
  consumers - exactly docs/jobs-research.md §4's argument.
- **Generations.** `gen` numbers runs so "a run that is slow to unwind cannot
  mark a newer run as finished" (`agent_dispatch.go:157-160`), which matters
  because crush's subagents are *reopenable* - `agent_send` resumes a stopped
  one with its work in context.
- **Label claims are race-closed on purpose.** "Checking the label and
  registering the entry happen under one lock, so two parallel dispatches cannot
  both take it" (`agent_dispatch.go:433-437`), and naming happens *last*:
  "Named last, so a refused dispatch does not use up a first name"
  (`agent_dispatch.go:431`).
- **Cost rollup** to the parent, incrementally (`rolledUpCost`, §2.7).
- **Restart reconciliation** reads the parent's own messages for dispatches that
  never reported and writes a synthetic failed report, because "without this the
  parent would wait forever on a report that is never coming". It runs once per
  session per process, **only while the session is idle**, "so the report lands
  between turns rather than in the middle of one"
  (`agent_dispatch_status.go:196-227`).
- **Busy accounting is a refcount, not a flag**: `subSessions map[string]int`
  exists "so the busy check sees subagents the main agent is not running itself"
  (`coordinator.go:191-194`), and `settleMu` serializes transcript repair "so
  two readers cannot both settle one orphaned call" (`coordinator.go:196-197`).
- **UI.** The report renders as a *tool item* on the same gutter as tool calls,
  because "a subagent finishing is the same kind of event as a tool returning,
  and the transcript stays legible when every non-prose block shares one shape"
  (`internal/ui/chat/subagent.go:22-27`), and its id carries an
  `atomic.Int64` sequence "even when two subagents share a label across
  sessions" (`subagent.go:17-19`).
- **Model sharing.** One `models` value serves every agent, `rebuildMu`
  serializes rebuilds, `awsRefresh` is a `singleflight.Group` that "shares one
  AWS SSO login among requests that need it", and per-dispatch model overrides
  are cached in `overrideModels` (`coordinator.go:160-177`).

### 3.3 qwen-code — caps, slots, and depth that fails closed (TS)

The most paranoid (i.e. correct) treatment of the nesting question.

- Two-tier concurrency: 10 background agents globally
  (`background-tasks.ts:54`) **and** a per-model cap, both checked by
  `assertCanStartBackgroundAgent(model)` (`:649-672`), with reservations and
  separate background/foreground wait queues and an occupancy getter "so a
  caller can render (`claimed/cap`) rather than only queue depth when a launch is
  blocked behind a running agent, which is the common case the queue-depth text
  alone cannot describe" (`:829-838`). Terminal entries are retained and
  bounded: `MAX_RETAINED_TERMINAL_AGENTS = 32` (`:152`).
- The refusal names the remedy - "maximum concurrent background agents (10)
  reached. Stop an existing agent first." (`:657-661`) - which is independently
  the rule mini's `ErrCap` states (docs/jobs-research.md §4, "the error that
  carries it has to name the remedy").
- Depth: `DEFAULT_MAX_SUBAGENT_DEPTH = 5`, ceiling 100 (`config.ts:1932-1934`),
  and a **corrupt persisted depth fails closed to the limit** rather than open
  (`agents/agent-transcript.ts:378-410`), with the depth restored on resume
  "so a restart-recovered agent retains the nesting budget it had when it was
  created" (`background-agent-resume.ts:638,1488`).
- Forks never spawn, enforced at runtime via an `AsyncLocalStorage` marker
  because tool-stripping would break byte-identical cache parity with the
  parent (`fork-subagent.ts:44-58`), and a detached fork is turn-capped at 200
  because "an unbounded reasoning loop burns tokens silently" (`:41-43`).
- Approvals from a detached child **bubble** to the parent's UI, because "a
  detached fork has no inline UI, so 'default' would auto-deny them"
  (`fork-subagent.ts:34-36`), and on cancel "Reject parked approvals BEFORE
  aborting. Order matters" (`background-tasks.ts:1119-1127`).
- Its registry entry is `kind: 'agent'` (`background-tasks.ts:303`) alongside
  shells and monitors, with `notified` and `outputOffset` for push delivery -
  the same shape as `jobs` + `job_events.delivered_at`.

### 3.4 oh-my-pi — isolation as a platform concern (Rust)

omp does not solve parallel sessions by sharing a process; it solves them by
making the *filesystem* cheap to duplicate. `pi-iso` is a cross-platform PAL
giving "a writable 'merged' view of a read-only 'lower' tree without paying for
a deep copy", with `clonefile(2)` on macOS, kernel overlay (then fuse-overlayfs)
on Linux, ProjFS on Windows, and `git worktree`/recursive copy as the portable
fallback; every backend can also surface the workload's changes as a
`git diff` "byte-identical to what `git apply` consumes downstream"
(`crates/pi-iso/src/lib.rs:1-19`). robomp applies it as one worktree per issue
with a persistent `session_dir` per workspace and tools "scoped to the worktree"
(`AGENTS.md:15-17`).

Its **Agent Hub** (`docs/agent-hub.md`) is the most developed answer to the
fan-in problem feature 2 has: one roster for the subagents of the current
session with status/identity/**parent**/unread count, resolved model, age, task,
cost, active time, request and tool-call counts and tokens; a header
aggregating across agents; `t` toggling a flat roster and a **parent/child
tree**; an inspector with parent and child lineage, output and patch paths and
isolated-worktree branch metadata; steering, revive and kill; `parked` as a
status for children recovered from persisted artifacts on resume; and missing
metrics rendered as `usage —` "rather than an estimate".

### 3.5 Corrections to the first draft

The draft of §5 was written before this survey. Eight things it said are wrong
or missing, and they are the reason the survey was worth doing:

1. It treated the model client as a non-issue ("a throughput question with no
   measurement behind it yet"). qwen built **per-model slot accounting with
   reservations and queues**, and crush shares one model registry with
   singleflight on auth work. On a one-GPU box this is the actual constraint on
   how many sessions may be *querying* at once, and it is a prerequisite
   because a subagent multiplies queries without adding capacity.
2. It had no answer to "what working directory does a subagent get". omp's
   existence proves the question is not hypothetical.
3. It had no recursion rule. Every surveyed harness with subagents has one, and
   the interesting variance is *where* it is enforced (tool list, async frame,
   runtime predicate, persisted depth that fails closed).
4. It had no cost rollup, which makes mini's `cost_limit` decorative the moment
   a subagent exists.
5. It claimed the concurrency cap was settled by `DefaultMaxJobs = 8`. That
   number is right for jobs and wrong for agents: little-coder's is a *total
   tree budget* divided across waves and **serial by default**, and qwen's cap
   is two-level. A model that wants 12 parallel subagents on one GPU wants them
   queued, not refused, and certainly not concurrent.
6. It said nothing about turn serialization, and mini's `Loop.Running()` is
   **advisory only** - the UI checks it to grey out buttons
   (`ui/fyneui.go:560`), but `Loop.Run` itself happily lets a second caller in,
   where both write `l.Messages` and fight over `l.cancelTurn`. zero's
   `turnMu` and crush's `subSessions` refcount exist for exactly this.
7. It proposed nothing about resume safety, and mini has a live bug waiting:
   `findResumable` with no prefix takes `store.List(project, false, 1)` - the
   newest session in the project by `updated_at` (`cmd/mini/main.go:691`, ordered `updated_at DESC` at `session/store.go:568`).
   A finished subagent updates more recently than the parent that spawned it in
   that same directory, so `mini -r` would resume **the subagent's transcript
   instead of the human's**. zero's `IsResumableKind` refusal is the fix, and it
   requires the lineage column §5 adds.
8. It was written as though the design were settled and shipped ("Status:
   implemented"). It is not.

### 3.6 Rulings against the survey

Three of §5's decisions go against what the surveyed harnesses converge on. They
were ruled after this document's design draft, and the dissent is kept intact
here rather than summarized away, because the survey's reasons are good reasons
and the next reader is owed them.

1. **Nesting.** The unanimous surveyed answer is a depth number, enforced twice
   so the layers cannot drift, over a persisted value that fails closed: qwen's
   `DEFAULT_MAX_SUBAGENT_DEPTH = 5` with a hard ceiling of 100
   (`config/config.ts:1932-1934`), the runtime backstop mirroring the tool-hiding
   predicate (`tools/agent/agent.ts:2795-2805`), and a corrupt stored depth
   treated as *at* the limit (`agents/agent-transcript.ts:378-410`).
   **Ruled:** children never spawn, enforced by not telling them the tool exists
   - in the request body and in the prompt's own tools listing (§5.6).
   **What the ruling buys:** the tree is two levels by construction, so `depth`,
   its persistence, its restore-on-resume and its fail-closed parsing all leave
   the schema (§5.3). **What it costs:** the model cannot be told "no" and learn
   to stop, because it never asked; if it *infers* the capability from training
   data it will hallucinate a call rather than hit a documented refusal. The
   mitigating fact is that this path already exists and is tested: the parser's
   name check (`model/actions.go:101-105`) turns an unadvertised tool into
   `Unknown tool 'x'.`, which routes through the same
   `format_error_template` the harness uses for malformed arguments, and
   `actions_test.go:108` asserts it against a foreign harness's tool name
   (`shell_start`). So the failure is legible to the model, not a mystery -
   which is the property §5.6's third consumer has to preserve rather than
   accidentally break.

2. **Concurrency.** The surveyed answer is layered: a global cap *and* a per-model
   cap with reservations and separate wait queues (`background-tasks.ts:649-672`),
   occupancy rendered as `claimed/cap` because queue depth alone cannot describe
   the common blocked case (`:829-838`), a shared model registry with
   `singleflight` on the auth work behind it (`coordinator.go:165-177`), or a cap
   reinterpreted as a *total tree budget* divided into lead/waves/reserve and run
   serially by default (`budget.ts:1-24`).
   **Ruled:** one global knob - maximum in-flight chat completion requests - and
   no other limits (§5.7). **What the ruling buys:** one number a user can reason
   about, no per-parent accounting to get wrong, and no cap error a model can
   misread as a task failure. **What it costs:** a saturated box cannot be
   attributed - the human sees four sessions crawl and cannot tell which one
   spawned the twelfth subagent, and the parent that over-fans-out is punished by
   its own latency rather than refused. **Accepted:** starvation is a fact of
   local-model life, and the answer ruled is human pause/kill controls per
   session (§7 step 7) rather than a fairness policy - which is the better
   instrument anyway, since a reserved slot is GPU capacity held by a session
   that may not be using it (§5.7).

3. **Workspace isolation.** omp treats it as a platform concern worth a
   cross-backend PAL, and robomp names the worktree the isolation boundary
   (§3.4). **Ruled:** none for now (§5.9). **What it costs:** parallel writers in
   one tree corrupt each other's view of the repository. The ruling is defensible
   because feature 2 does not touch it and read-mostly subagents mostly do not
   either - but it is deferred, not resolved, and §5.9 says so at the point where
   someone will decide to spawn a writing subagent.

### 3.7 Asking the human from outside the loop

The seam §9.4's queue and §5.5's bubbling both lead to: a session needs something
from a person, and it is not in a position to use its normal prompt path - it is a
child, or a background job, or a resumed run nobody has typed at yet. Four
harnesses have solved this and they split into two camps.

**qwen-code: bridge it to the nearest ancestor that can answer.** An approval
request is an event carrying `respond` - literally a callback the requester
closes over (`AgentApprovalRequestEvent`, `background-tasks.ts:240-258`) - plus a
`subagentId` that says which descendant is waiting. The comment on that field is
the load-bearing one: the bridge caller **declares** it, because "the runtime id
and the registry agentId use different suffixes, so comparing them cannot
distinguish own from nested." Attribution cannot be derived; it has to be carried.

The interesting part is what the callback *isn't*. The machinery is in answering
"who can hear this?": `findBackgroundedAncestorAgentId` walks the lineage links to
the nearest backgrounded, still-running ancestor (`agent.ts:501-520`), because "a
nested foreground agent has no inline UI. Its scheduler's TOOL_WAITING_APPROVAL
fires on the invocation's own emitter, which nothing bridges - the call would wait
forever." Approvals get parked "exactly where the ancestor's own approvals go," so
one dialog answers a whole tree. Three refinements, each a scar:

- **No ancestor, no bridge.** A top-level foreground launch "keeps the inline
  confirmation path unchanged" - the fallback is a different mechanism, not a hang.
- **Double answers are deduped** by a per-call `responded` guard, because a dialog
  and an inline path can both be live.
- **The bridge is skipped when the ancestor auto-denies**
  (`getShouldAvoidPermissionPrompts`),
  since under that ancestor "no TOOL_WAITING_APPROVAL can fire and the subscription
  would be dead."
- Cycle protection comes from a visited set rather than a hop budget, because depth
  is configurable up to 100.

**oh-my-pi: do not bridge. Refuse, and make the spawn the boundary.** Flatly the
other answer (`docs/approval-mode.md`): "Subagents run headless with
`tools.approvalMode: yolo` so ordinary tier-based prompts do not stall them. **The
parent `task` approval is the authorization boundary.** Per-tool policies still use
the normal resolver: user `deny` remains absolute, and any call that resolves to
`prompt` rejects because a headless subagent cannot satisfy it" (:216). And the
general rule behind it: "Without an interactive UI (print mode, headless
subagents, the Cursor exec bridge) the call is refused, like any other prompt with
nobody to answer it" (:63).

omp also puts a **floor under yolo inheritance**: stealing the user's foreground
must be asked for "in every mode", and the mode that makes subagents headless
deliberately cannot grant it, "because the default `yolo` would otherwise let every
headless subagent take the user's foreground" (:130). Headlessness is a convenience
for ordinary prompts, not a licence.

**zero: inherit, never widen, and resolve at the seam.** A member's
`PermissionMode` is empty to mean "inherit the orchestrator's mode (never
widened)" (`swarm/definition.go:110`), and the spec that crosses the launcher seam
carries a resolved model "never the `inherit` sentinel" (`member.go:23`) - the same
resolve-before-crossing discipline §5.1 cites for authority. Its subagent prompt is
also the clearest statement of §9.7's identity paragraph in the survey: "You start
with zero context - the briefing below is all you know."

**crush** sidesteps the question by construction: subagents are dispatched into a
read-only or full access tier chosen at spawn time (`readOnlyParent`,
`resolveSubagent`, `agent_dispatch.go:383-402`), so the capability that would need
asking for is largely not offered, and what is offered needs no prompt. That is
omp's posture with the tier chosen per dispatch instead of per mode.

## 4. What mini already proved

Worth recording because it is surprising and because it decided the shape of the
work: the loop, the interpreter, the registry and the store were already
concurrency-clean. A probe running four sessions at once in one process -
different working directories, one shared `*session.Store`, each with its own
`Loop`, `env.Local` and `job.Manager`, writing files into its own repository and
reading them back - passes under `-race` with no changes to any of those
packages. Three sessions each starting a real background job, and each woken by
its own job's ending and only its own, likewise.

The reasons are structural rather than lucky:

- `agent.Loop` keeps its state behind one `sync.Mutex` on the value and takes
  its collaborators (`Cfg`, `Model`, `Env`, `Recorder`, `Jobs`) as fields rather
  than reaching for anything global. `agent` does not import `session`, which
  the `Recorder` comment already names as the reason storage can be dropped or
  replaced - the same seam zero uses for `MemberLauncher`.
- `env.Local` is stateless per call: a fresh `interp.Runner` every `Execute`
  with `interp.Dir(dir)` from the argument (`env/local.go:157`), so `cd` in one
  session cannot move another, and mini never calls `os.Chdir` anywhere.
- `job.Manager` is one per session and every registry query is scoped by
  `session_id`, right down to the composite `PRIMARY KEY (session_id, id)` on
  `jobs` that the first live run of two sessions forced into existence when it
  collided on two first jobs both numbered `j1`.
- `session.Open` was already written for many writers - WAL,
  `busy_timeout(30000)`, `_txlock=immediate` (`store.go:155-157`) - and the one
  transaction in the package (`turns.go:184`) does no I/O inside itself, so
  `SetMaxOpenConns(1)` cannot interleave with a goroutine waiting on anything.

So this was never "make the core thread-safe". It is "remove the six
one-per-process assumptions in the wiring", plus the policy the core cannot
invent for itself: what lineage means, which sessions may be resumed, whose
authority a child carries, and how many requests may be in flight at once.

---

## 5. The design: a Process, and N sessions inside it

The split follows the existing grain of the code rather than inventing a layer.
Things that are genuinely singular go in `host.Process`; everything that is
per-conversation goes in `host.Session`, built by `Process.Open`.

```
host.Process                one per mini process
  Store                     the shared SQLite handle
  InitEnv                   the global .env, applied exactly once
  Gate                      the bound on in-flight chat completions (§5.7)
  TrajDir                   where trajectory files go by default
  Open(Spec) -> *Session    callable any number of times, concurrently
  Sessions()                the live set, for a UI to enumerate
  Close()                   reap every manager's jobs, then close the store

host.Session                one per conversation - top-level, resumed, or subagent
  Cfg, Model, Env, Sess, Jobs, Loop   the six things miniRun already held
  Run(ctx, task)            synchronous, single-flight; panics are fatal
  Go(ctx, task)             asynchronous, for anything needing a job lifecycle
  Wait()                    the result Go's run produced
  CancelTurn / Quit / Close this session only, never a sibling
```

### The fourteen settled decisions

**1. Assembly takes a merged config, not flags.** `Spec.Config` is
`map[string]any`, already resolved. `cmd/mini` keeps owning flags, `-c` specs
and the wizard, because those are how a *human* says what they want; the host
owns what happens with the answer. This is what makes a subagent possible at all
- it has no argv - and it is zero's `MemberSpec` discipline: **resolve before the
seam, never pass the word "inherit" through it** (`member.go:23`).

**2. `host` does not own the front end.** `Run` is synchronous because both
front ends call `Loop.Run` inline today and stream into it via `Hooks`; `Go`
exists for callers that need a job-shaped lifecycle. Choosing one would have
meant rewriting either the GUI or the loop's contract, and the contract is what
the tests are built on.

**3. Lineage is durable.** The `sessions` table gains `parent_id`, `root_id` and
`kind` (`top` / `subagent`), matching zero's shape (§3.1) because every later
question - what may be resumed, whose cost is this, what does the UI nest, what
does `mini sessions` list - is one indexed predicate with those columns and a
transcript walk without them. `ADD COLUMN ... NOT NULL DEFAULT` is not a
breaking change, so it does not yet force the `PRAGMA user_version` decision
`session/store.go:34` defers. `jobs` already has `parent_id` and `tcno`; the
child session gets the same pair of links, so a job and a session can find each
other from either end - which is crush's "sub-session ID derives from the bare
tool call ID so the UI can walk back from a child-session event to this tool's
chat item" (`agent_dispatch.go:426-429`) done with two columns instead of an id
convention.

`root_id` is the **top-level ancestor's own id, and a top-level session is its
own root** - zero's `firstNonEmpty(parent.RootSessionID, parent.SessionID)`
(`internal/sessions/exec_session.go:142`). That is the difference between
"everything in this run" being one indexed `WHERE root_id = ?` and a recursive
walk with an `OR id = ?` clause that somebody eventually forgets, and it is what
makes §5.11's subtree deletion a single query.

There is deliberately **no `depth` column.** zero and qwen both persist it
because their trees nest to arbitrary depth; §5.6 makes mini's exactly two
levels by construction, so `kind` already answers every question depth would
have been asked - and a column nobody can set to a second value is a column
that only drifts.

**4. A child session is not resumable by a human.** `store.List`, `Find` and
`findResumable` default to `kind = 'top'`; `mini sessions` shows `parent=` for
children the way zero does, and opening one explicitly is a *view*, never a
continuation. §3.5.7 is the bug this prevents, and it is preventable only by
this decision: `updated_at` ordering cannot distinguish a subagent from the
conversation the human meant.

**5. Authority never widens downward.** A subagent inherits the parent's
`mode`, `whitelist_actions` and config ceiling; `Open` refuses a spec that asks
for more. Because `confirm` requires a human, and a detached child has no UI of
its own, the child's confirmations **bubble to the parent's front end** through
the existing `Hooks.BeforeExecute`, addressed by label - qwen's
`approvalMode: 'bubble'` (`fork-subagent.ts:34-36`) and the reason it exists
("a detached fork has no inline UI, so 'default' would auto-deny them"). With no
human attached, the child gets the same answer an unattended parent gets today:
refused, with the reason. No new capability, no widening, no silent auto-approve.

**Bubbling is only safe if the prompt says who is asking, and the name is the
parent's to give.** The spawning tool therefore takes a short descriptive
**name** as a parameter - chosen by the model, saved in the database, and used
for every human-facing interaction with that subagent. Session UUIDs are right
for the store and useless in a dialog: a human deciding whether to allow
`rm -rf build/` in the next three seconds cannot compare
`01a1e247-7bf6-7bce-9241-dd22ca0dc497` against anything. So `ui.Question` gains
an origin carrying both - the id for any action the answer triggers, the name
for the one line the human reads - and the confirm card in both front ends
renders it (`ui/ui.go:24-38` has `Kind`, `Title`, `Actions`, `Cfg` and nothing
else today; the GUI's confirm panel paints exactly `q.Title` plus the command
lines, `ui/fyneview.go:1389`). Without that field, bubbling is a regression in
the only property `confirm` mode exists to provide: the human approving a
subagent's command while reading it as their own.

The name lands in the **existing `title` column**, which needs no schema change
to hold it: `title` is already written first-write-wins, backfilled from the
task only `while sessions.title is still empty`
(`session/turns.go:142,155` - `CASE WHEN title = '' THEN ? ELSE title END`).
An explicitly named child is therefore immune to the backfill by construction,
and the one column that already means "what a human would call this session" is
doing double duty. It is also what `mini sessions` already prints
(`cmd/mini/sessions.go:95`), so the name reaches the CLI listing for free.

**The name is unique within its top-level session**, and it is held for as long
as the child is not done - which §5.10 defines as *after* the child's own jobs
end, not merely after its loop submits - §5.12 is what makes a waiting child a
not-done child. Holding the name costs the parent a label; it never costs the
parent its turn, because a waiting child does not block the conversation that
made it. The parent goes on working and simply finds the wake has not come yet,
which is the truth of the situation rather than a delay of it. So the namespace
is one `root_id`, the tree is flat by construction, and a name frees up when the
work it describes has actually left the GPU rather than when the model stopped
talking about it.
`Open` refuses a collision with the remedy in the message - crush's exact
wording, "pick another label or stop it first" (`agent_dispatch.go:440`) - and
the same rule mini's job cap already follows (`job/job.go:44-45`).

The liveness half of that rule is what makes it workable rather than
exasperating. A parent retrying a failed subagent wants the name back, and gets
it; a parent that backgrounded a build and gave it a name cannot start a second
run under that same name while the first is still burning, which is exactly the
ambiguity the name exists to prevent - two live things called "quantise 70b" in
one prompt to a human being asked to approve something.

The job id stays the machine handle and the name the human one; only the
first has to be unique for all time, and only the second has to be
*understandable* in three seconds.

**6. Subagents cannot spawn subagents, and the mechanism is visibility rather
than a depth number.** The spawn tool is not advertised to a child at all: it is
absent from the `tools` array its requests carry, and absent from the tools
listing its system prompt renders. A tool a model was never told about is not a
tool it has to be refused, so there is no retry loop against a denial it could
not have anticipated.

The consequence for the plumbing is the part worth writing down, and it is
bigger than "generate the prompt": **the tool list is written in three places
today, and all three have to derive from one per-session set.**

1. The request body: `model/sushi.go:306` splices
   `[]any{BASH_TOOL, JOBS_TOOL, JOB_KILL_TOOL}` into every query, so the
   advertised set is a property of the client rather than of the session.
2. The prompt: `config/configs/mini.yaml` names tools in prose - line 60 tells
   the model about `job_kill`, and `bash` appears seven times in the
   `system_template` body - so hiding a tool from the array leaves it in the
   instructions, which is the half the ruling specifically forbids.
3. The parser: `model/actions.go:101-105` validates the incoming tool name
   against a hardcoded `case ToolBash, ToolJobs, ToolJobKill:` and reports
   anything else as `Unknown tool '<name>'.`

Point 3 is the one that turns a tidiness argument into a correctness one: an
advertised tool the parser does not know is a tool that cannot be used, so
adding a spawn tool for top-level sessions without touching the switch would
produce a session that was told to call something and then refused for calling
it. Conversely a hidden-but-still-parseable tool is exactly the inconsistency
the third consumer exists to prevent. One set, three renderings.

The codebase has already settled this argument once for a number:
`Loop.TemplateVars` interpolates `view_lines`/`view_chars` into the prompt
rather than retyping them, on the grounds that "a number written down twice is a
number that drifts" (`agent/agent.go:942`) - after the shipped prompt advertised
a 200-line reading habit through a 100-line window. Same failure, same fix, and
the subagent rule is what makes it necessary rather than merely desirable.

This supersedes the depth bound the survey recommended (§3.3, §3.6): with no
child spawning, the tree is two levels by construction.

**7. Concurrency is one global knob: the number of in-flight chat completion
requests.** No per-parent cap on subagents, no per-model slot accounting, no
queue-depth reporting, no divided tree budget. The bound is process-wide and
lives in the model layer, admitting `Query` and `Summarize` alike, so a
subagent's request competes for the same slots as its parent's and no caller can
forget to consult it. That unskippability is the argument for the model layer,
and it settles what §9 used to leave open.

There is exactly one choke point to put it in, which is convenient: every
`chat/completions` POST goes through `Sushi.attempt` (`model/sushi.go:350`),
reached from `retryStream` (`model/sushi.go:258`), which is the only thing both `Query`
(`:165`) and `Summarize` (`:214`) call. So the compaction side-query is bounded
by the same knob as a turn without either path knowing about the other, which is
what makes "one global knob" a true statement rather than a hope.

And the gate belongs at `attempt`, **not** at `retryStream`. Retries back off
exponentially with a 4s floor and a 60s cap (`:119`, documented at `:72`), so a
slot taken for the duration of the retry loop would be held through minutes of
sleeping -
bounding how long a request may *wait to exist* rather than how much work the
GPU is doing, and starving sessions behind it for no reason. One slot per HTTP
attempt means a stalled request releases its place while it backs off, which is
the only reading of "in-flight requests" that costs what it says.

**A slot is held for the entire request and response, streaming included.** The
knob therefore bounds GPU occupancy, which is the resource that actually hurts,
and not the number of sockets in flight - and the honest name for it is
something like `model.max_concurrent_requests` with the stream semantics stated
in the config comment, because a reader who sees "requests" may otherwise expect
a request to be cheap. The consequence is real: a child running a 40-minute
reasoning trace holds its slot for 40 minutes, and everything queued behind it
waits that long. That is accepted for the same reason starvation
is: the mitigation is the human pausing or killing a session, not releasing the
slot early and letting the box oversubscribe itself. It stays
consistent with the gate sitting at `attempt` rather than `retryStream`: one
slot spans one HTTP request and the stream it opens, and is handed back while
the client sleeps through a backoff before trying again.

The bound **blocks rather than refuses.** A refusal inside a turn arrives at the
model as an error it did not cause and cannot fix, whereas waiting costs only
wall time on a box where the work genuinely is queued. Slots are held for the
duration of a *stream*, which on a local server is minutes per request, so this
knob is a statement about GPU occupancy and not about connection count - which
is also why one number is honest here where the survey found two-tier caps
necessary (qwen's global 10 *and* a per-model cap, `background-tasks.ts:649-672`):
mini serves one model from one box, so the two collapse.

Two consequences worth stating rather than discovering. Agent-kind jobs are
exempt from `job.DefaultMaxJobs`, because that cap exists to stop a model
"multiplying its own mistakes" with GPU-heavy background runs
(`job/job.go:42-45`) and an agent job is not a run in that sense - it holds no
pid and needs no stopper (`job/job.go:9-10`). And with no per-child limit, the
only thing a parent can exhaust is the shared bound its own work competes
within, so an over-eager model degrades its own conversation instead of being
refused. That is the intended failure mode; §3.6.2 records what it costs.

**The bound is one number, default 4, and it is stored rather than only
configured.** Persisting it in the settings table - the same place the thinking
toggle lives - is the right home because it is machine state about this box
rather than run configuration about this invocation: scripts and `-c` overrides
must keep the config file's meaning, and nobody wants `mini -p` quietly
rewriting their config (the reasoning already written down at `ui/prefs.go:12-20`
for the two existing keys). Precedence follows specificity, mirroring how
`model.DiscoverContextWindow` resolves a window and logs which of the three
answered: an explicit config value for this run wins, then the stored preference,
then the default - and the key is deliberately *absent* from the embedded
`mini.yaml`, because a shipped default sitting in the file would outrank the
stored choice forever and the setting would be a write-only column. A stored
value that does not parse, or is not positive, is not an opinion: it falls
through rather than closing the gate.

Holding the slot inside `Sushi.attempt` turned out to be free of the awkwardness
this section anticipated. `attempt` reads the entire SSE body and closes it
before returning (`model/sushi.go:374`), so acquire-and-release around that
call *is* the whole request and response, stream included, with nothing to track
past the function boundary.

**Starvation is accepted, on purpose.** A fan-out can therefore fill the gate
with a parent's own children and leave the parent queued behind them - including
behind the step that would deliver their reports. Nothing deadlocks, because
children terminate and release, but a session can stall for a long time. That
is ruled a fact of life for local models rather than a problem to solve with
fairness accounting, and the mitigation is placed where it belongs: the human,
with **pause and kill controls per session**, which is feature 2's UI work
(§7 step 7) and is a better instrument than any reservation policy - a
reserved slot per session would let a stalled session hold GPU capacity it is
not using, which is the opposite of what the knob is for. Two things this
ruling still owes, so that waiting is at least legible: the gate's wait must be
selectable on `ctx` (a quit that hangs behind a queued request is a bug, and
zero documents the same trap, `lifecycle.go:38-49`), and a parked request
must be surfaced, because "a stalled window with no explanation is
indistinguishable from a crash" (`cmd/mini/main.go:348-350`). This draft proposed
reusing the existing `Model.OnRetry` seam for it; that turned out to be a bad
idea worth recording, because `OnRetry`'s signature - attempt, max, error, wait -
describes a request that *failed and is being tried again*, and a slot wait is
neither a failure nor a retry. It has its own hook now, `Sushi.OnGateWait`,
carrying occupancy rather than an error.

**8. Cost rolls up for accounting, and the two numbers are different.** A
subagent's spend is reported as part of its parent's total, and it does **not**
count against the parent's `cost_limit` or `step_limit`: those limits bound the
session's own conversation, and a fan-out must not be able to stop the parent
mid-task on money the parent never directly spent. The split is only honest if
the UI keeps them apart too - a footer reading `$4.10` while the limit is `3`
and the session's own spend is `$0.40` is a lie of the same size in the other
direction, so both numbers are shown, labelled, wherever the total appears.

This is where crush's design and mini's part company, and mini gets the better
of it. crush *mutates* the parent's number and so needs `rolledUpCost` to
remember how much it has already folded in, because "the child's own total is
cumulative across every turn" (`agent_dispatch.go:173-176`) - a per-run
incremental accumulator whose whole purpose is avoiding a double count.
mini's store can make the tree total a **derived query** instead: with
`root_id` on every row, inclusive cost is
`SELECT sum(cost), sum(n_calls) FROM sessions WHERE root_id = ?`, indexed by the
column §5.3 already adds. Nothing is accumulated, so there is nothing to
double-count, nothing to persist on the parent, no ordering to get wrong between
a child finishing and the parent's ledger being written, and a crash between the
two leaves an answer that is still correct. It also sidesteps a hazard the
naive version walks straight into: `Loop.Cost` and `Loop.NCalls` are plain
fields written only from the agent goroutine, and `Snapshot`'s own comment says
reading them from elsewhere "races with the agent goroutine"
(`agent/agent.go:807,1019,1150,1179`). A rollup that added into `l.Cost` from
a child's watcher would be a data race the parallel tests here would catch -
and the derived query never touches them.

One consequence to keep visible rather than discover: because limits count own
spend only, `cost_limit` bounds a *session*, not a *run*. A model spending $40
through twelve children will not be stopped by a parent configured with `3`.
That is the ruling, and the honest place to say so is the limit's own
refusal message.

**9. No workspace isolation, by ruling.** A subagent runs in its parent's
`env.Local`, in the same directory, against the same files.

The survey's strongest counter-example is omp, which built an entire
copy-on-write PAL for this (`crates/pi-iso/src/lib.rs:1-19`: APFS `clonefile`,
Linux overlayfs, Windows ProjFS, `git worktree` as the portable fallback) and
still settles on "per-issue isolation is the git worktree" (`AGENTS.md:114`) -
and omp is running many agents *writing the same tree*, which is precisely the
case this ruling leaves unprotected. What makes it affordable is that zero,
crush and qwen all share the tree too, and the reason it is tolerable for them
is that their subagents are read-mostly: crush gates dispatches toward a
read-only access tier rather than isolating them (`agent_dispatch.go:383-402`).

So the risk is named rather than removed: two concurrent sessions, or a parent
and a child, editing one checkout see each other's uncommitted changes, and
`git status` in one is not a statement about that one. Feature 2 is unaffected -
different repositories are isolated by definition, which is the asymmetry that
makes this the right thing to defer: the prerequisite does not depend on it, and
only some future, write-heavy use of feature 1 will.

**10. Endings are reported, including the ones nobody witnessed.** A bash job's
end is a process exit; an agent job's end is a loop returning, so it needs no
stopper and `job`'s registry is already written to tolerate a kind with none
(`job/job.go:416-10`). On restart, `Sweep` marks it and the host writes the
firing *itself*, with text that says what happened and what is still recoverable
- the shape crush settled on: *"Interrupted: mini exited while this subagent was
running. Its session is kept; ... resumes it with its work in context."*
(`agent_dispatch_status.go:218-225`). Delivery runs at open, once per session,
and only while idle, "so the report lands between turns rather than in the
middle of one" (`agent_dispatch_status.go:200-202`). mini's `job_events` with
`delivered_at IS NULL` already makes this durable, where crush has to
reconstruct it from the parent's messages - a genuine advantage of the registry
being the source of truth - and the reason §4's "the store, not the transcript"
rule is load-bearing rather than stylistic.

**The report is delivered when the child submits, even if the child left jobs
burning.** The row stays `running`, because a process this session started is
still on the box and saying otherwise would be a lie - but nothing waits on it,
and the parent is told immediately, with the residue named: *submitted; it left
two jobs running, `j3` (training, log at ...) and `j7` (server, ...).*

An earlier draft of this section gated delivery on the child's job count
reaching zero, and that was the wrong instinct: it made the harness absorb, with
an automatic wait, a mistake that belongs to the agent. mini already assigns the
duty. The bash tool's own description ends *"end it yourself with `job_kill`
when you are done with it. A job outlives a turn, never a session."*
(`model/model.go:224`). An agent that started a watcher and yielded its
result without ending or declaring it has forgotten it was still doing
something, and what a forgotten instruction needs is visibility and a human, not
a mechanism that quietly extends the deadline - especially when the forgotten
job may be a server that never exits, which makes "eventually" a rhetorical
term rather than a promise. Blocking a parent on its child's oversight converts
one agent's mistake into another agent's stall.

**There is one event, not two.** An earlier draft of this section had the agent
job deliver early and finish late, and that treated a submission as an ending,
which §5.12 rules out: a no-tool-call turn while a job may still report is not
the child having nothing left to do - the job's result is exactly what it is
waiting for, and when it lands the child has work again. So the report is the
step at which the child had nothing to do **and nothing that might still speak
up**, and the agent row goes terminal at that same moment. One event, and the
report cannot be older than the state that produced it.

The dev-server objection that motivated early delivery was sound but aimed at
the wrong mechanism. Waiting is not a hang, because the wait has an ending the
model does not have to guess at: `job_kill` is a terminal transition, so killing
the forgotten server writes a firing, the firing buys a step (`deliverOwed`,
`agent/agent.go:1111`), and the child wakes to find its own watcher gone and can
then genuinely finish. The escape hatch is the human the earlier ruling pointed
at (§5.7, §7 step 7), and reaching them costs a *step* rather than a rescue -
which works only because the thing being waited on is a row with an id in a
listing rather than a mystery process, the same reason §4 makes the registry the
source of truth.

What survives from that analysis is the plumbing: mini's state machine already
separates `finished` from `delivered` (`agent/agent.go:1127`), and `job_events`
already carries firings that are not exits - `monitor_id` and `reason`
(`session/jobs.go:399-400`), where `exit` is only the column's default. That is
precisely what lets a killed job wake a waiting child, so the two-stage
distinction was worth having even after the two-stage *delivery* was wrong.

The residue is also cheaper than it looks, which is why leaving it alone is
tenable. It costs **no gate slot** - slots belong to requests, and the child
stopped making them when it submitted (§5.7) - so a forgotten job does not
starve anybody's GPU time. It is a row in the registry, a held name (§5.5), a
running process, and an output file that keeps growing. The human's remedies are
the ones already ruled for starvation: the roster's per-session pause/kill
(§7 step 7) sees it, and closing the parent reaps it through the cascade
(§9.2). Nothing auto-kills a child's jobs at submission, because a child that
launches a training run and reports the job id is doing precisely what the
registry exists for, and **the harness cannot tell that case from
forgetfulness** - only the human, or the
report's own words, can. Which is why §9.7 rules that children are **guided to
have drained their jobs before reporting**, making the residue this section was
written to describe the exception rather than the expected shape of a subagent's
ending: the prompt does the work, and waiting is the backstop for the turns the
prompt did not reach.

So the duty belongs in the prompt rather than in the machinery: the `agent`
tool's directive to a child has to say that a job it starts is its to end or to
declare in its report, and the report format should have a place to declare it.
That is a prompt-contract requirement on step 6, not a new mechanism - and the
reason it is stated here rather than left for the prompt draft is that §5.6
makes the child's prompt a *generated* listing, so the sentence has to be added
deliberately or the child inherits a tools block and no obligation.

A child that is **killed** reaps its outstanding jobs first, through the
ordering in §9.2, so the forgotten case and the killed case differ in exactly one
respect: somebody chose one. The name frees when the row goes terminal, which the
child's own manager drives as its last job ends - `job.Run.Done()`
(`job/job.go:215`) and `Run.Wait(ctx)` (`:288`) are the watching primitives - and
since that watcher is now off the delivery path, the worst it can do by never
firing is hold a name, which a human can end.

**11. Removing a session removes its subtree, and garbage collection closes
what a crash leaves open.** This ruling went against this document's own
recommendation, which was `ON DELETE SET NULL` on `parent_id` so a parent's
deletion could not vaporise the audit trail of what its subagents did. The
counter-argument wins: a subagent transcript whose parent is gone is not
history anybody can read - it was one step inside a conversation that no longer
exists - and rows nothing can reach are the definition of the mess §5's lineage
columns were added to prevent. So `parent_id` carries `ON DELETE CASCADE`, and
the work moves to making the deletion *complete*.

The trap is that mini's transcript is not all in the database. `Store.Delete`
cascades the rows and then removes `os.RemoveAll(tcrBase/<uuid>)` - for the id
it was handed (`session/store.go:717,733`). Result files live outside the
database and their path is *derived from the row's id*
(`Session.tcrDir()`, `session/tcr.go:12`), so a cascade that deletes twelve
child rows silently strands twelve directories that nothing can name again: the
rows are gone, and the path is computed from the row. That is a permanent leak
of exactly the most expensive bytes mini keeps - every command's full output,
plus the background jobs' logs, since `JobPath` puts those in the same
directory (`session/jobs.go:156-158`).

So `Delete` collects its descendants **before** it deletes anything, and removes
each one's files and rows, then the parent's. `root_id` is what makes that one
query rather than a recursive walk, and it is why §5.3's top-level sessions
store their own id there: with the tree two levels deep,
`WHERE root_id = ?` is the whole subtree including the root.

Orphans still happen - a parent deleted by an older build, a crash between the
directory removal and the row delete, a hand-edited database - so a sweep
removes child sessions whose `parent_id` references a session that is not
there, along with their files. It runs where `job.Manager.Sweep` already runs,
at open and before anything can start (`job/job.go:420`), and it **reports what
it removed** rather than tidying quietly, which is the stance the job sweep
already takes about the thing mini cannot prove.

Two consequences, both ruled. `mini sessions rm` is now a much bigger hammer -
it deletes every subagent's transcript and output at once - so it should count
and name what it is about to remove before doing it. And **a child is removable
only with its parent, and only with its parent**: no `--children` flag, no
prefix that resolves to a subagent. The reasoning cuts in both directions, which
is why the answer is not a flag. Removing a child *before* its parent destroys
data the parent may still use - the transcript a later turn refers back to, the
output file a report points at, the child a follow-up would resume - and
removing one *after* is just garbage, which is precisely what the sweep already
collects. A knob that can only either lose data or tidy up is not a knob.

§5.4's `kind = 'top'` filter is the whole enforcement, since `rm` resolves
through `Store.Find` like `-r` does (`cmd/mini/sessions.go:53-68`): one filter,
two commands that must not be able to name a subagent. That is worth noting
because the resume restriction and the removal restriction look like separate
rules and are the same column doing the same job - subagent sessions are
history, not endpoints.
**12. A session is never finished.** It reaches a state where there is nothing
left to do *without a further user message*, and that is a different state from
being finished.

mini already says the first half out loud and then contradicts it eight lines
later. At the exit boundary the loop notes that "Ending a turn is the model
saying *it* has nothing to do, not that nothing remains to be done"
(`agent/agent.go:1105-1109`) - exactly right - and then: a *finished* undelivered
job buys a step (`:1111`), while a job that is merely still running gets a log
line and the run breaks anyway (`:1114-1118`). So today a session with a build
in flight reports `Submitted`, exits 0 (`cmd/mini/main.go:535`), and writes a
trajectory saying the task is done.

The rule: **a response without a tool call indicates nothing-left-to-do only when
no background job may still deliver.** If one may, there is work remaining, and
it is that job's result that will drive it. The state after a turn is therefore
two states, not one:

| | waiting on a human | waiting on a job |
|---|---|---|
| what will resume it | a message | a firing |
| who can end it | the human, by typing | the human or the model, by `job_kill` |
| is there work left | no | **yes** |
| should `exit_status` claim completion | no | no |

Calling the second column `Submitted` is the same lie as leaving a `running` row
after a crash - and §4 built the whole job sweep on the argument that a record
saying work has stopped when it has only paused is worse than no record. It also
re-arms the exact confusion that made `job_wait` worth deleting
(docs/jobs-research.md §4): a model that has been promised its background
results arrive unprompted will read a transcript saying "submitted" instead of a
registry saying "three still running", and answer a question its own unfinished
jobs were about to answer.

Three consequences land elsewhere in this document rather than here: §5.10's
report is the quiescent step rather than an early delivery, §5.5's name is held
across waiting because a waiting child is not done, and §9.9's table has a
waiting row that is explicitly not an ending. What it opened - whether a waiting
session holds `Run` open - turned out to be a malformed question about two
different things at once, and §9.10 closes it by leaving the top-level loop alone
and testing quiescence in the agent row.

**The scope of the change is the subagent, not the loop.** That paragraph
originally demanded the loop's exit path be rewritten to match the model, down to
a `Waiting` status replacing `Submitted` whenever jobs may still deliver. §9.10
rules against it: nothing here changes how a top-level session operates, so
`Submitted`, `deliverOwed`'s comment ("a job that is still *running* is not an
owed result at all... the run can end beside a server that never exits",
`agent/agent.go:1148`) and the "Agent is ready" prompt all stay exactly as
they are.

That is coherent rather than a concession, and it is worth seeing why. The
existing comment is true *of the human's session*: a person can walk away, the
registry keeps the result, and the next run takes it - the session is not
finished either, it is simply waiting on a human and on a job at once, and the
human is the one who decides. What the model forbids is a claim of completion,
and for a top-level run `Submitted` has always meant "this turn-series has
nothing more to do", which is exactly what the loop returns. A **subagent** is
the case where a no-tool-call turn would be read as completion by *somebody
else* - the parent, collecting a report - so that is where the claim has to be
tested, and §5.10 tests it in the row. The principle lands entirely inside the
agent kind, and the loop never learns about it.

**13. Asking the human is a callback the session owns, and its absence is a
refusal rather than a wait.** Settled from the survey in §3.7 and the maintainer's
leaning toward exactly the mechanism qwen turns out to use.

Every session carries an `Asker`: one function it calls when it needs a person and
cannot use its ordinary prompt path - a subagent's confirmation, a resumed run's
question, a limit reached while nobody is watching. The child constructs the
question, hands it over, and waits for the answer; that is the callback, and it is
all the seam is. What the survey adds is the four things that have to be decided
*around* it, none of which are the callback itself:

- **Attribution is carried, never derived.** qwen's `subagentId` exists because
  ids from different layers cannot be compared to tell own work from nested
  ("comparing them cannot distinguish own from nested; the bridge caller declares
  it"). So `ui.Question` gains an origin set by the asker, naming the session that
  is waiting - which is §5.5's field and §9.4's queue finally having a producer.
- **Who can hear it.** In qwen that costs a lineage walk, a visited set and a
  "nearest backgrounded ancestor" rule because its tree nests to 100. mini's tree
  is two levels by construction (§5.6, backstopped by `Create`), so the answer is
  one hop: **ask my parent's asker, or there is none.** The machinery the survey
  found is real and mini gets to skip almost all of it - which is the strongest
  argument for the simple shape rather than against it.
- **No asker means refuse, not wait.** Both omp (:63, :216) and qwen's
  no-ancestor case land here, and it is the rule the design already uses for
  `job_send` on a bash job: the request fails with a reason the model can act on.
  A callback with no implementation hanging forever is precisely qwen's "the call
  would wait forever" failure, arrived at by a different road.
- **Double answers are guarded.** qwen needs a per-call `responded` flag because a
  dialog and an inline path can both be live. mini has one path and a queue that
  hands out one turn at a time (§9.4), so this is already paid for - but it should
  stay in mind the day a second answering surface appears.

**Headlessness is not a licence, and there is a floor under inheritance.** omp
makes subagents yolo so ordinary prompts do not stall them, keeps `deny` absolute,
and refuses to let that convenience grant the capability that would take the
user's foreground "in every mode". mini's version of that line is worth naming now
even though it has nothing to put behind it yet: an unattended child may not gain
anything the human never approved, and `job_kill` with `all: true` is already
treated this way - the harness refuses to guess at a sweeping action and makes the
model ask for it explicitly. If step 6 grows a capability whose blast radius spans
sessions, that is where the floor goes.

**This retires the confirm-mode spawn guard, and that is its point.** The guard
added in §8 - refuse to spawn from a `confirm` parent because the child would run
unattended - was the right call while bubbling did not exist, and it was always
meant to be temporary: `TestConfirmModeParentCannotSpawnYet` skips itself when it
becomes obsolete. Once a child's questions reach a human, omp's posture is the
better one: the child runs, ordinary prompts bubble, anything that cannot reach a
person refuses with a reason, and **the delegation itself is the thing the human
approves** - which is a decision they can actually make, taken once, at the point
where they can see the task, rather than one they are asked to make blind per
command from a transcript they cannot see.

**14. No turn or time budgets.** They are a relic of the one-shot harness this
port came from: mini-swe-agent solved one benchmark instance, printed a trajectory
and exited, so bounding its steps and its clock was the only way to stop an
autonomous run going forever. mini is an interactive product with a human watching,
a persistent transcript, `jobs` reporting elapsed times, `job_kill`, and a window
that can be closed - so those numbers stopped being a guardrail and became a
second, invisible author of the report. "The subagent ended on its step limit" is
an answer that came from a config file nobody meant as a task specification.

**Landed for everything, at the maintainer's second look.** The first pass
cleared a child's step, cost and wall budgets in `host.Open`, which fixed the
symptom only for the sessions this document happened to be about. The ruling
then widened: no budgets for anything - they are a relic of the one-shot
harness, and mini runs local models, so the only real cost is electricity and
the only honest brake on a runaway is a person deciding to stop it. What went:
the three `Config` fields and their config keys, `checkLimits` and its call,
`OnLimits` from `agent.Hooks` and all four implementations, the limits dialog in
both front ends, `AskLimits` as a live prompt kind, `limitsBanner`,
`ParseLimitLine`, and `cmd/mini`'s `-l/--cost-limit` flag. The trigger had been - a child's spend
belongs to its session and is not reset by another run of it, so re-driving a
helper could stop it partway through its second pass on the strength of its first -
and both alternatives were worse: a fresh ledger per revival is no ledger at all
(anything can be finished in n+1 passes for the price of n), and raising the
parent's number on a child's behalf is the human's decision, not the harness's.
`TestSubagentOutlivesItsParentsBudget` asserts the behaviour, and replaced a test
that asserted the opposite - whose failure was itself the proof, since the report
it captured was the child's real submission rather than a budget notice.

**Two things worth how they were handled.** `OnLimits` was deleted from the
`Hooks` interface rather than left uncallable, because an interface method
nothing can invoke is still a method four implementations have to answer for -
`ui.Interactive`, the batch front end, `host.bubblingHooks` and a test fake - and
the GUI's whole dialog subsystem turned out to exist only for the limits prompt.
That made the removal larger than the field list suggested and is the right size:
a abstraction with one client, whose client is gone, is not an abstraction, it is
a level of indirection to nothing. `LimitsExceeded`, `TimeExceeded` and
`AskLimits` were retired rather than deleted, because old transcripts carry them
as data and a resumed run should still be able to say what ended it - deleting
those would erase history to remove a feature.

Asked directly, the answer on money was that there is no money: `cost_limit`
went with the others. It had the strongest claim to being different in kind - not
a guess about duration but a ceiling on spend, liftable mid-flight by a human -
and on a local box it was a limit against a number that is always zero, which is
worse than no limit because it looks like protection. **What bounds a runaway
instead**: `job_kill`, `Quit`, closing the process
and its subtree (§9.2), and the in-flight-request bound (§5.7), which is the only
cost that actually matters on one GPU. A runaway is visible in `jobs` with a name,
an elapsed time and an output path - a thing a human can act on, where a silent
budget cut is a thing that merely stops.

**Two wiring gaps found by asking "is it actually wired in?" rather than by
reading the implementation notes.** Both were in the spawn path, and both were
the kind that a green test suite would never show because nothing had been
written to look at them:

- **Delegation was not gated.** `NeedsConfirmation` had grown to cover bash and
  the three job tools and had simply never been told about the fourth thing that
  starts work. So a `confirm`-mode human would have approved the parent's
  commands while an agent quietly spawned whole sessions beside them. That
  matters more than an ordinary missed gate: §3.7's omp posture rests on "the
  parent `task` approval is the authorization boundary", and a boundary nobody is
  prompted at is not a boundary. `job_send` stays ungated on the existing
  job_kill reasoning - it can begin nothing.
- **A running subagent looked like a finished call.** The spawn observation
  returned `ReturnCode: 0`, and `ui/blocks.go:88` keys its live card on
  `obs.Pending` - so the card closed immediately, reporting a delegation that had
  produced no output as a successful one, and the running child became
  invisible in the one place the human looks. The handoff path already carries a
  comment explaining why `Pending` rather than `0` - "a 0 here is what made a
  handed-off call look like a succeeded one in every front end that reads it" -
  and the spawn path had not been given the same treatment it warned about.

`TestNeedsConfirmationCoversDelegation` pins the gate across all five tools,
including the bare unlabelled action that defaults to bash.
`TestKillStopsALiveSubagent` is the one that the budget removal makes
load-bearing: with no ceiling left, `job_kill` and `Quit` are the only brakes, so
stopping a mid-turn child has to work, settle its row as `killed`, leave its loop
not running, and hand the parent a summary with how far it got rather than a
blank.

What stays in every reading is the **accounting**: `Cost`, `NCalls`, the tree
total, the card headers, the trajectory. Those answer "what did this cost", which
is a different question from "when should we stop you", and §5.8's rollup depends
on the first staying accurate even as the second goes away.

### Report contract

The report is the child's **submission at quiescence**: its `exit` message
already carries the final text, because mini's prompt contract makes a turn with
no tool call the submission - and per §5.12 that turn only counts as a
submission when nothing the child started may still report. The text is not the
first no-tool-call step of a child with work in flight; it is the one after the
last firing lands. No new mechanism, and
nothing to strip - mini keeps reasoning in a separate `reasoning` field, so the
`<analysis>` scrubbing qwen needs (`subagent-result.ts:142-154`) is not a
problem here, which is worth recording as luck rather than design. A child that
ends any other way (limits, format errors, cancellation) produces a
failure summary naming the state, never an empty report - while a panic is not a
report at all, because the process dies by ruling (§9.9), and both cases follow
mini's existing truncate-and-spill rule: cap the body, point at the child's
output path for the rest.

**15. Sessions get tabs, one per session, top-level or subagent.** *Ruled by the
maintainer, and it overrides §3.7's recommendation and step 7's first draft.*
Each tab wraps the whole current UI for its session - its own transcript, input,
status and permission prompt - and the strip is the window's content.

The recommendation this replaces was "a roster and a parent/child tree, not
browser tabs", drawn from omp-Agent-Hub. That advice is right for what it was
measured against: a roster optimizes for *scanning many* sessions, which is what
a farm of short batch runs needs. mini's realistic workload is the other case - a
handful of long-lived sessions, each in its own repository, where the human
*works in* one at a time and wants the others to keep running beside it. For
that, a tab per session is the better shape, and the roster's real virtue -
seeing every session's state at once - moves into the tab labels rather than
disappearing. What the roster still owes, and tabs must keep, is listed in step 7.

Three earlier rulings change shape because of this, and each is recorded with
what happens to it rather than quietly rewritten:

- **§9.4's one-at-a-time queue becomes per-tab.** It exists so two questions
  cannot interleave in one shared input area. Tabs remove the shared input area,
  so serializing unrelated repositories behind one prompt would be a regression
  bought at the wrong price. Attribution moves from a string to the tab itself;
  `Question.Origin` and `displayTitle` stay, because print mode and a resumed
  transcript have no tabs to attribute through.
- **§5.5's bubbling becomes routing.** A child's confirmation no longer travels
  to whoever gates its parent; it appears where the child is, and
  `host.bubblingHooks` shrinks to "ask this session's own front end".
- **Decision 13's unattended child needs re-reading.** It ruled a child of an
  unattended parent unattended, "because there is nobody to bubble to". With a
  tab, a child *is* attendable - the human can click it - so a child of an
  attended parent should get the parent's real hooks. That changes what
  `SpawnReply.Unattended` means, so it needs re-ruling before it is coded, not
  discovered afterwards in a diff.

### Which Fyne container: the check, and why it is DocTabs

Verified against the vendored source (Fyne 2.8.2, `fyne.io/fyne/v2/container`),
not against documentation. The mutation API is identical in both - `Append`,
`Remove`, `RemoveIndex`, `Select`, `SelectIndex`, `Selected`, `SelectedIndex`,
`SetItems`, `SetTabLocation`, and `Enable`/`Disable` by item or index - so the
choice is entirely about interaction, which is what this feature is.

| | `AppTabs` | `DocTabs` |
|---|---|---|
| `CreateTab func() *TabItem`, a native "+" | absent | `doctabs.go:26`, built at `:303`, hidden when nil (`:466`) |
| `CloseIntercept func(*TabItem)` | absent | `doctabs.go:27`, consulted at `:179` |
| `OnClosed func(*TabItem)` | absent | `doctabs.go:28` |
| per-tab close icon | never wired (`onClosed` unset) | `doctabs.go:333`; hover on desktop, always on mobile (`tabs.go:751`) |
| scrollable strip | no scroller at all (0 references) | yes (15 references, `r.scroller.Content`) |
| "all tabs" jump menu | absent | `buildAllTabsButton`, `:279-300` |
| `OnUnselected` | yes | `doctabs.go:30` |

`AppTabs`' struct carries `OnChanged` (deprecated), `OnSelected`, `OnUnselected`
and a `popUpMenu` field it never populates. It is a section switcher for a fixed,
small set of destinations - exactly the assumption mini does not have: these tabs
come and go, are opened by the human and by a spawn, and can outnumber the strip.

Four consequences worth writing down, because three are bugs waiting:

1. **`CloseIntercept` suppresses removal entirely.** `close()` at
   `doctabs.go:178` reads `if f := t.CloseIntercept; f != nil { f(item) } else {
   t.Remove(item); t.OnClosed(item) }`. The moment mini sets an intercept - which
   it must, to put §5.13's quit guard per tab - the container removes nothing and
   fires nothing, and the intercept owns both the confirmation *and* the
   teardown. Set the guard, forget the removal, and every close silently does
   nothing, which is indistinguishable from a frozen UI. So all removals funnel
   through one `closeTab`.
2. **The jump menu is text-only.** `doctabs.go:285` carries an upstream FIXME:
   "MenuItem doesn't support icons (#1752)". With `TabItem` limited to
   `{Text, Icon fyne.Resource, Content}` and no slot for a widget, status has to
   be encoded in `Text` whatever the container. Settling that now avoids
   designing a glyph system the toolkit cannot carry.
3. **Neither container reorders.** No `Move`/`Insert` in either method set, so
   "children beside their parent" is insertion order plus `SetItems` - which
   `Refresh()`es without re-resolving `current` by identity. Any reorder needs an
   explicit `Select` afterwards, or the highlighted tab can point at a different
   session than the content being shown.
4. **`CreateTab` is synchronous and returns the item**, but opening a session is
   not: it needs a working directory (mini has no picker of any kind) and
   `proc.Open` does store writes plus a context-window discovery. None of that
   belongs on the canvas thread inside a widget callback, against the `GUI.post`
   discipline the whole view obeys. So `CreateTab` returns a *picker* tab whose
   `Content` is a small form, and the session opens from the pump after the human
   commits.

## 6. Cross-cutting components, by value-per-line for mini

1. **Lineage columns + resume filter + recursive removal** - a few lines of
   schema and four queries. Closes a data-loss-class bug (§3.5.7) *before*
   subagents exist, because subagents are what make child sessions possible, and
   the same `root_id` that answers "may this be resumed" is what makes subtree
   deletion one query instead of a walk (§5.11).
2. **Turn serialization (single-flight)** - one mutex and one refusal in
   `Loop.Run`; without it, two callers corrupt a transcript irrecoverably.
3. **Not panic containment - and that is a ruling, not an oversight.** The
   survey's Go harness recovers at precisely this seam ("A panic in a member's
   Run must fail only that member, never crash the orchestrator process",
   `member.go:70-78`) and the first draft of §5 copied it. Ruled against: under
   private active development a panic is a bug caught red-handed, and turning one
   into a tidy `failed` row is the harness declining to report its own defect.
   The process dies, loudly, with the stack. What step 3 owes instead is
   **attribution, not survival** - the `session=` log attribute (§7 step 2) makes
   the crash name the session that caused it - and §9.12 asks the one scope
   question left, about the `recover` that already exists in the runtime path.
4. **Per-session trajectory path** - prevents sibling runs overwriting each
   other's records; `defaultOutputPath` is one function.
5. **`Process.Open` extracted from `buildRun`** - the actual prerequisite;
   everything else is already callable concurrently.
6. **Model query gate** - the survey's most valuable import (§3.5.1), and
   ~20 lines with a buffered channel, because mini can queue instead of refuse.
7. **Cost rollup** - small, and required for the limit to mean anything.
8. **Authority checks in `Open`** - cheap policy at one seam. (The depth bound
   that used to be listed here is gone: §5.6 makes children unable to spawn, and
   `Create` refuses a grandchild, so there is no depth to bound.)
9. **Owed-report delivery at open** - mostly already built as `job_events`.
10. **A per-session tool set with three renderings** - request array, prompt
    prose and parser acceptance all derived from one set (§5.6). This is the
    mechanism ruling 1 depends on: an invisible tool needs a generated listing
    or the invisibility leaks through the prose, and an advertised tool the
    parser does not accept is a tool that cannot be used.

Deliberately not built: **any workspace isolation** (ruled out, §3.6.3),
**per-model or per-parent concurrency limits** (ruled out in favour of the single
global knob, §3.6.2), **a persisted nesting depth** (unnecessary once children
cannot spawn, §5.3), zero's lease files (mini's prune is manual and its
`session.lock` equivalent is a single SQLite writer already), and qwen's
slot-reservation protocol (the queue makes it unnecessary while delivery is
push).

## 7. Build order

1. `sessions` lineage columns (`parent_id` with `ON DELETE CASCADE`, `root_id`
   defaulting to the row's own id, `kind`); `List`/`Find`/`findResumable`
   restricted to `kind = 'top'`; `mini sessions` shows the parent; recursive
   subtree removal in `Store.Delete` and the open-time orphan sweep with their
   directories (§5.11). **Tested against the resume bug and the directory leak
   first** - they are the two items here that can bite a user today, and the
   leak is the one that never stops costing.
2. `Loop` single-flight turn mutex (refuse, and name `QueueMessage` as the
   supported path, §9's settled note); `runCtx` under `l.mu`; per-session
   trajectory path; global env applied once per process; a `session=` attr on
   every record a session logs.
3. **Built.** `host` package, including §5.6's per-session tool set: `Process`,
   `Spec`, `Open` (no `recover` at the session boundary - panics stay fatal by
   ruling, §6 item 3),
   `Run`/`Go`/`Wait`/`Quit`, sweeps that `New` cannot skip, the derived
   tree-total cost query (§5.8), and the model gate at `Sushi.attempt` - one slot
   per request and its whole streamed response, a ctx-selectable wait, reported
   through the new `Sushi.OnGateWait` hook (§5.7). Startup
   model-info lookup cached per process.
4. **Built.** `cmd/mini` calls the host instead of assembling inline, with
   `run()`'s contract unchanged - the existing end-to-end tests were left
   untouched precisely so they could say whether that was true.
5. Tests: the four-session and three-jobs probes, promoted; two sessions
   resuming the same project; queued
   (not refused) queries under a saturated gate; a parent deletion that leaves
   no directory behind; an orphaned child swept at open.
6. **Built; bubbling deferred to step 7** (§8). Feature 1: `KindAgent`; `Spec.Config`
  derived from the
   parent - constructed inside the tool call as a fresh copy, so nothing is
   inherited by accident (§9.8); two tools, `agent` to spawn and `job_send` to
   speak to a child whether it is running or finished (§9.3, §9.5, §9.11), the
   latter restoring a finished child's transcript before delivering to it; the §9.2
   cascade with its sweep ordering and its single batched unwind; the §9.3
   revive-as-a-new-run, with the resumed child's tool set resolved from its own
   kind; the agent row's quiescence test, held in the job wrapper and **not** in
   the loop - `Loop.Run` still returns exactly as it does for a top-level
   session, and `deliverOwed` and `exit_status` are untouched (§5.10, §9.10); the
   §9.7 report paragraph in the child's prompt; `ui.Question`'s
   origin field, rendered by both front ends (§5.5); and the per-session tool set
   with the system prompt's tools listing generated from it (§5.6). The
   paragraph §9.7 rules - drain your background jobs, take the wake, then report
   - belongs inside that generated child prompt rather than beside it.

   Two prerequisites move *earlier* than this step. The question-queue fix
   (§9.4) belongs with step 2, because the pending-slot clobber is a live bug
   whatever feature 1 decides and its symptom - a wedged agent goroutine with
   somebody else's prompt on screen - is the worst kind to debug later. And the
   tool-set templatization (§5.6, §9.6) belongs with step 3, because with a
   single tool set it is a no-op for every existing session, which makes it
   behaviour-neutral to build before there is a second one to differ for.

   **The origin field is not optional in this step** - bubbling without it puts
   an unattributed permission prompt in front of a human, and §9.4 makes
   unattributed questions the normal case rather than the corner.

7. Then feature 2's UI half, **rewritten as tabs** - see decision 15. The first
   draft of this step said "a session strip modelled on the Hub's roster/tree
   rather than browser tabs"; the maintainer overrode it, and the reasoning that
   made the roster the survey's answer is kept in decision 15 rather than
   deleted, because it still describes what the strip has to show. Increments,
   each shippable:

   1. `DocTabs` around the existing view, one tab, no new behaviour. **Done.**
   2. **Built (§8.3).** Extract the per-session state `GUI` currently holds
      singularly - above all
      `stream streamState`, which decides which transcript streamed text lands
      in, plus `pending`, `lastQ`, `contextNote` - into a `tab`, and move the ask
      queue with it. The risky step: leave `streamState` shared and a background
      session writes into the transcript the human is reading. It landed as
      `shell` (the window) plus a `GUI` that *is* one tab, because `ui.UI` was
      already the per-session contract; same seam, one fifth of the renaming.
   3. `CreateTab` to open a top-level session, with a working-directory picker,
      because a tab whose repository you cannot choose is a tab you cannot use.
   4. **Built (§8.4).** A child-created callback on `Process`, so a spawn opens its
      tab. Polling `Children()` would work and is wrong: it makes the UI guess.
      This is also where decision 13 was re-ruled (attended-ness follows the front
      end), where `Session.unattended` went, and where `Spawn`'s hook-assignment
      race - and the comment that denied it - were fixed.
   5. Status and attention markers in the label, `⌥→`/`⌥←`, and the per-tab quit
      guard through `CloseIntercept` - which per decision 15 must own the removal
      and the cascade itself.
   6. Split the slash commands into tab-level and window-level, and re-issue
      §5.13's batched cancel across N sessions instead of one `runCtx`.

   Still owed to the roster, and tabs must not lose it: per-session **pause and
   kill** (§5.7's starvation mitigation is what made that ruling affordable), and
   a view of a session's **outstanding jobs** endable independently of the
   session that started them. Without the second, §5.10's deliberate "no
   auto-kill" residue is invisible, and an invisible residue is not a judgement -
   it is a leak.

## 8. What is built

### 8.0 The tab strip exists, with one tab (step 7 increment 1)

`ui/fyneui.go` now builds `container.NewDocTabs` around the existing `guiView` at
the single place the window's content is set, with the tab labelled by model name
and nothing else moved. The choice of container, and the four source-level
traps that follow from it, are decision 15.

Two deliberate omissions, both because the later steps own them:

- **`CreateTab` is left nil.** The "+" would open a session, and a session needs
  a working directory the UI has no way to ask for yet (increment 3).
- **`CloseIntercept` is set to a refusal, not a teardown.** DocTabs puts an × on
  every tab whether or not anything handles it, and `close()` runs the intercept
  *instead of* removing the item - so with no intercept a mis-click would delete
  the only tab and leave an empty window holding a live session, which is a worse
  way to lose work than anything mini had before. The intercept notes that in the
  transcript and leaves. When increment 5 lands the per-tab quit guard, this is
  the one function that changes, and it must then own both the removal and the
  `proc.Close` cascade - decision 15, consequence 1.

Two flakes surfaced while landing this, one real and one not:

- **`host`'s `TestSpawnRunsAChildAndReportsToItsParent` failed about one full-suite
  run in two**, at clean `59e37ca` with no UI change involved, as `deterministic
  model ran out of outputs after 3 calls`. Not a product bug: the parent's script
  had exactly three outputs, and whether the child's report is owed *before* the
  parent submits decides how many turns the first run spends - so `runChildTurn`
  sometimes needed a call the script no longer had. Fixed by giving the script
  slack for both orderings, with the reason in the test. The assertion was always
  about the child reporting and the row settling, never about turn count.
### 8.1 Copying out of the cards, and the test that could not see it

Card text is now selectable, and it took two attempts. The first swapped
`monoBlock` from `RichText` to a selectable `Label`, and its test asserted that
`monoBlock` returns a selectable `Label` - and passed, while the user still could
not copy anything, because the transcript cards do not go through `monoBlock`.
They go through `transcriptGrid`, which built its own `RichText` via `monoRT`.
`RichText` has no selection whatsoever in Fyne 2.8: `widget.selectable` is
embedded by `Label` and `Entry` only. So the fix had to move `monoRT` itself, and
the test had to move with it to the widget the user actually clicks.

The lesson is not "test the real path", which everyone already knows, but that a
test which asserts *a helper returns what the helper was changed to return* is
not a test of the feature. The replacement drives a mouse-down and a drag across
the card body and then asks the clipboard what arrived, which is the only version
of this that can fail for the reason it just failed for. Two incidental findings
came with it: the drag has to carry the *moved* position, because Fyne's
selection tracks the pointer and ignores the delta, and an unsized text widget
collapses every position onto one character, so a selection test needs the widget
laid out or it reports the feature broken for the wrong reason.

The second pass made **every** card body selectable, which the first did not:
`blockCards` routes a styled turn, thinking, a user or system message, a note, an
error and the compaction summary through `mdBody`, and `mdBody` was markdown all
the way down. It now asks a per-block question instead - `mdOrLabel` renders the
block as a selectable `Label` unless the text carries markdown that would visibly
change the rendering, in which case it keeps `RichText`. The predicate errs
toward formatting (an asterisk or an underscore anywhere sends the block to
markdown, even where the markdown would have rendered the same), because the two
ways to get it wrong are not equal: guessing "plain" silently drops a heading's
formatting and reads as a broken renderer, while guessing "markdown" costs one
styled paragraph its drag-select.

Why not select over the markdown widget directly, since selection is *built* on
one? `widget.selectable` holds a `*RichText` provider and drives it through
unexported methods - `row`, `rows`, `rowLength`, `rowAlignOffset`, `charMinSize` -
so nothing outside package `widget` can attach it to a `RichText` of its own. The
reachable move is to give the text to the widget that already embeds the
selection, and reserve the markdown path for the blocks that need it.

Still not selectable, both on purpose: the `exit` row, which paints two colours
in one line and is a status marker rather than output, and inline-styled prose.

### 8.2 Fyne 2.9's `RichText.Selectable` deletes the whole trade-off (dev pin)

Both halves of 8.1's compromise are gone: every card body is a `RichText` again,
*and* every card body is selectable. Fyne put selection into `RichText` itself -
`Selectable`, `SelectedText()`, `ClearSelection()`, "Since: 2.9" (upstream
issue [#5946](https://github.com/fyne-io/fyne/issues/5946), closed as completed
2026-10-09; landed by commit `2e33fd8a` "Add selection API for RichText"). The
last *release* is v2.8.2 and does not have it, so `go.mod` now pins the develop
pseudo-version `v2.8.3-0.20261115175655-cfb984d35559`. **When 2.9.0 ships, replace
the pin with the release**; until then, treat any Fyne-side regression as ours to
bisect, because the pin moves under us only when we bump it deliberately.

What that bought, measured rather than assumed: a drag across a markdown
`RichText` yields the *rendered* text - `the **build** passed` copies as
`the build passed`, backtick and asterisk markers left behind - and a drag across
blocks reconstructs structure (`first paragraph line\n• item one\n• item two`).
So `mdOrLabel`, `mdShowsFormatting` and `selectableLabel` are deleted, the
formatting-vs-clipboard predicate is gone, and card bodies are one widget kind
again. `exitRT` selects across its two colours, which the Label route could not
draw at all.

The survey behind the decision (15 text-oriented apps from apps.fyne.io, read from
source): **zero** use `Label.Selectable`; every app that renders styled text either
adds a whole-text Copy button and keeps a raw string beside the widget (Janice,
EcNotes, Fyclip - which is also the maintainers' own advice) or hand-rolls
selection over custom painting (Gaze ~2k LOC of caret/anchor/row-rect code;
fyne-io/terminal inverts per-cell `TextGrid` styles; TetherSSH needs a transparent
`TextGrid` plus a pixel-snapped background layer and still documents gaps; DasherG
abandoned its selection highlight as unworkable). `TextGrid` has no selection in
2.8.2 *or* 2.9, so the one-widget-per-block mono body stays a `RichText`.

**The focus trap is not a Fyne bug we can pin away - it is the model.** Mouse-down
in any selectable text widget (`Label`, `RichText`, `Entry`) moves canvas focus to
the internal `*widget.focusSelectable`, whose `TypedKey`/`TypedRune` are no-ops, and
the highlight rectangles are drawn *only while that widget holds focus* (measured:
one visible rectangle while selected, zero after refocusing the input). So
"the selection stays visible" and "the input is focused" cannot both hold, and
`chatInput`'s reclaim cannot fire because `Focused()` is not nil. Left alone, that
means select a card and then every keystroke is swallowed.

The answer is a menu item, not a handler: glfw dispatches a shortcut to the focused
widget and **returns**, so `Canvas.AddShortcut` is unreachable while any text widget
holds focus; only main-menu shortcuts fire *before* the focused widget
(`internal/driver/glfw/window.go`, `triggerMainMenuShortcut`). Hence `Edit ▸ Copy`
carrying `&fyne.ShortcutCopy{}`, whose action asks the *focused object* - which is
`widget.focusSelectable`, not the RichText above it - for `SelectedText()`, puts it on
the app clipboard, and returns the caret to the input: select, ⌘C, type is one
continuous gesture. Two consequences of that focus model are accepted rather than
fought:

- the highlight disappears when ⌘C hands the focus back, so ⌘C *is* the "copied"
  feedback. Fyne draws selection rectangles only on the focused widget, so "still
  visible" and "still typeable" cannot both be had;
- the copy must go through `app.Clipboard()`, the same handle `widget.selectable`
  uses. `window.Clipboard()` is a different object - per-window on Unix, and a
  single-use stub in the test driver, where writing it is silently invisible.

`ClearSelection()` is deliberately not called: it lives on the RichText, while the
focused object is its internal selection, and nothing about that object is
reachable from outside package `widget`. Right-click Copy on a card is Fyne's own.

- **`TestGUITrajectoryMatchesPrintMode` intermittently deadlocks, and the first
  write-up of this got the attribution wrong.** It was recorded above as
  link/CGO contention, because it appeared in full-suite runs while the same
  package finished in 2.7s alone. It is not contention. It reproduces about once
  in three at `go test ./ui/ ./agent/ ./host/ -count=1`, it hangs in that same
  test for the whole package deadline, and it **reproduces at `59e37ca`** in a
  fresh worktree - before the tab strip and before the running card, so neither
  caused it and neither can be blamed for it.

  What is known: `waitFor` gives up after 5s and `t.Fatalf`es, so the minutes of
  silence are not polling. The stall is the test parked on `<-done` with the run
  goroutine still inside `GUI.Ask` - a question that will never be answered
  because the only thing that answers it is the test, and the test has stopped
  pumping to wait. `TestGUITrajectoryMatchesPrintMode` is the one GUI test that
  ends by typing `/quit` *while a question is still pending*, and its own comment
  calls the preceding `p.enter()` "no-op on every frontend", so the resolution
  depends on which path the submit takes.

  Not fixed here, deliberately: it is pre-existing, it is in the quit-answer
  path rather than in anything this increment touched, and a blind change to how
  a slash command interacts with a pending prompt is exactly the sort of edit
  that turns a one-in-three hang into a one-in-one. It is the first thing to
  take up, and the reproduction command above is the whole cost of entry.

§7 steps 1 and 2 - the prerequisite's schema and loop-safety half - are in the
tree, with the survey and the rulings behind them.

**Step 1: lineage, and removal that is actually complete** (`session/store.go`,
`session/lineage_test.go`, `cmd/mini/sessions.go`)

- `sessions` carries `parent_id` (`REFERENCES ... ON DELETE CASCADE`, nullable
  because SQLite exempts NULL from FK enforcement where `''` would be an
  unresolvable id), `root_id`, and `kind`. `Create` resolves the defaults at the
  seam: a top-level session is its own root, a child inherits its parent's, and
  an unknown parent or a grandchild is refused - the store's own backstop for the
  two-level tree, so the rule in §5.6 does not depend on every caller having read
  §5.6.
- `Find` and `List` filter to `kind = 'top'`, which is the entire enforcement of
  §5.4 and §5.11 at once: `-r` and `mini sessions rm` both resolve through
  `Find`, so one predicate stops a subagent shadowing a human's conversation on
  resume and stops it being named by `rm`. `Children` is the only way to a
  subagent row. `TreeTotals` is §5.8's derived inclusive cost - a query, never a
  write into somebody else's ledger.
- `Delete` collects the subtree **before** it deletes, which is the fix for the
  leak §5.11 describes, and removes every collected session's directory. Written
  by `parent_id` rather than `root_id`, because siblings share a root and would
  otherwise be taken along.
- `migrateSessionLineage` ALTERs a pre-lineage database into shape and backfills
  `root_id = id`, and runs after the schema exec rather than before - a fresh
  database has one spelling of the table and a legacy one gets patched, with no
  divergence between them. The two new indexes live in the migration for the same
  reason: an index over a column the legacy table lacks fails the whole exec,
  which the first version of this code did and the migration test caught.
- `SweepLineage` and `SweepStrayResults` run at open, before any session exists,
  and are wired into `buildRun` - not into `mini sessions`, which promises a
  listing. They report what they removed rather than tidying quietly.

Two mutation checks were run against these tests, because a bug-guard that does
not fail when the guard is removed is decoration: deleting the `kind = 'top'`
predicate from `Find` makes `mini sessions rm` happily destroy a subagent's row
and files, and deleting the single-flight test below's guard lets the second run
into the history.

**Step 2: one run at a time, and no process-wide surprises**
(`agent/agent.go`, `cmd/mini/main.go`, `agent/parallel_test.go`)

- `Loop.Run` is **single-flight**: `beginRun` claims the loop under `l.mu` - the
  check and the set in one critical section, because checking `Running()` and
  then setting it is the race, not a narrower version of it - and a second
  caller gets `ErrRunInProgress`, which names `QueueMessage` as the thing the
  caller almost certainly wanted. It refuses rather than blocks, which is the
  opposite of zero's `turnMu` and correct here for one reason: mini has a
  steering channel and ACP does not.
- `runCtx` under `l.mu`, for the same §3.5 reason.
- The trajectory is named after the session under the state directory unless
  `-o` says otherwise, so a second session can no longer overwrite the first's
  record and leave a filename that misdescribes it.
- `loadGlobalEnv` runs once per process behind a `sync.Once`, so a second
  session's startup cannot mutate the environment a first session's shells are
  snapshotting.
- Every record a session logs carries `session=<id>`, including its job
  registry's, which is what keeps a four-session log readable later rather than
  a single interleaved stream.

**The tests that hold the whole design up** (`agent/parallel_test.go`): four
sessions in one process with one store, each writing and reading its own
directory; three sessions each woken by its own background job's ending and by
no sibling's. These are the evidence for §4 and the tripwire for the assumption
§5 rests on - the day the core grows a process-global, one of them fails and
names the assumption that died.

**The model gate** (`model/gate.go`, `model/sushi.go`, `model/gate_test.go`) -
§7 step 3's one blocking piece, now built:

- `Gate` is a **resizable, ctx-cancellable** bound, deliberately not a buffered
  channel: a channel semaphore cannot shrink when a human lowers the limit, and
  its wait cannot be interrupted by a quit - which would have made `mini`
  unquittable while any request was queued. The implementation is a counter plus
  a broadcast token that is replaced on every release, so a spurious wakeup is
  just another trip round the loop.
- It is taken inside `Sushi.attempt`, which reads the whole stream before
  returning, so a slot means one complete request-and-response.
- `DefaultGate` is what a `Sushi` with no `Gate` uses, so the way to escape the
  bound is not "forget to wire one in" - the property that made the model layer
  the right home (§5.7).
- `SetLimit` wakes waiters when raised and never preempts a request already
  running when lowered; `Stats` reports busy/waiting/limit for the UI's
  occupancy line, since `claimed/cap` is what qwen found a user actually needs.
- Resolution and persistence in `applyConcurrency` (§5.7), logged with a
  `source` field the way the context window is.

`TestSushiCapsConcurrentRequests` proves it from the outside: a server that
counts its own concurrent requests, eight queries, a gate of two, and a peak the
server observed - so releasing early, holding through a backoff, or missing a
path all show up there rather than in production.

**Step 2 is complete**, including the §9.4 question queue described there - the
two front ends now take turns, which was a live hang wearing a feature-1 label.

**The startup model-info lookup is cached** (`model/info.go`,
`model/info_test.go`) - §7 step 3's second entry. Six sessions opening at once
now ask `GET /v1/models` once rather than six, and a failed lookup is
deliberately not cached, because a server still loading is not a server with no
opinion. The reason this is worth the code rather than a shrug: that lookup is
not a chat completion, so it never reaches `Sushi.attempt` and is therefore
**invisible to the request gate** - a subagent fan-out would have fired N
unbudgeted requests at a box that was already busy. Accidentally bypassing your
own bound is the specific way a well-intentioned limit fails.

**The interpreter panic is now loud** (`env/local.go`, `env/local_test.go`), per
§9.12's ruling that the recover stands but must not be quiet. Checking that
ruling against the code found `env` had **no logger at all**: the recover built a
`Result` carrying `exception_type: Panic` and discarded the stack, so no record
of an upstream fault outlived the process. `Local` now takes a `Logger`, the
recover calls `recoveredPanic`, and that logs at error level with the panic
value, the cwd, the untruncated command, and `debug.Stack()`; `buildRun` hands it
the session's logger so the record carries `session=<id>`. The command still
fails as before - catching it was never the question.

**Steps 3 and 4: `host`, and the CLI on top of it** (`host/host.go`,
`host/session.go`, `host/host_test.go`, `cmd/mini/main.go`).

`host.Process` owns the store, the live sessions, the context every job inherits,
and the global environment behind a `sync.Once`. `Process.Open(Spec)` assembles
one session - config, model client, interpreter, durable row, registry, loop -
from a merged map and a directory, with no flags, no terminal and no human, which
is the property both features need and `buildRun` could never supply.
`Session.Run` is synchronous for the front ends that stream into it;
`Go`/`Wait` are the job-shaped lifecycle a subagent will use; `Quit` ends one
session and its jobs and nobody else's.

Four things in it are rulings rather than incidental choices:

- `New` runs both garbage-collection sweeps, and cannot be asked to skip them -
  the same reasoning that put the gate in the model client, since a skippable
  cleanup fails by leaking quietly forever. A failed sweep is logged and never
  stops the process: refusing to run because garbage would not collect trades a
  leak for an outage.
- `Spec.Workdir`/`Project` are passed in rather than computed. `projectOf` is a
  rule about checkouts and symlinks - the CLI's idea of identity - not a property
  of running a conversation, so the host is handed the answer.
- `trajectoryPath` gives a **subagent its own file whatever its spec carries**
  (§9.8). The first version honored an explicit path for any kind and
  `TestTrajectoryPathsDoNotCollide` failed on it immediately, which is the case
  for writing the test before the wiring: this leak is invisible until a run's
  record has been overwritten by its own child.
- No `recover` anywhere. §9.9's ruling is stated at the top of the package file
  with its reason, because the recovery this package *does not* contain is the one
  zero writes at exactly this seam, and a thoughtful reader would otherwise add it
  as an improvement.

`buildRun` is now the CLI half only - global env, first-run setup, flag-derived
config, the store, project identity, resume resolution - with one `Open` for the
six objects it used to build by hand. **Its end-to-end tests were not changed,
which is the only real evidence that the rewire preserved behavior.** Verified
against the offline server: a headless run and a resume land exactly as before,
and a seeded orphan child plus a stray result directory are both collected *and*
named in the startup log.

**§5.6's per-session tool set** (`model/tools.go`, `model/actions.go`,
`model/sushi.go`, `agent/agent.go`, `config/configs/mini.yaml`,
`host/host.go`) - which was the last prerequisite item, and it turned out to be
the one the survey's §3.6.1 warning about layers drifting was really about.

One value, `model.ToolSet`, now feeds all three renderings: the request array
(`Sushi.tools().schemas()`), the parser's acceptance
(`parseToolCallActions(..., allowed, ...)`, replacing the hardcoded
`case ToolBash, ToolJobs, ToolJobKill`), and the system prompt's `## Your tools`
block, generated per session from `Loop.tools().Listing()`. `host.Open` picks the
set from the session's kind - top-level gets `TopLevelTools()`, a subagent
`SubagentTools()` - and assigns the same value to the client and the loop, so the
array and the instructions cannot disagree. `ToolAgent` is declared now with a
comment explaining why it precedes its own implementation: the kind split has to
exist before the spawn tool does, or the first commit that adds spawning ships
with subagents able to spawn and a test suite that never had a way to notice.

The prose that depends on tools is gated rather than deleted: the background-jobs
bullet in the Command Execution Rules is wrapped in `{{if .has_jobs}}`, and the
prompt variables (`tool_list`, `has_jobs`, `has_agent`, `subagent`) come from the
set, with `subagent` *derived* from the absence of the spawn tool rather than
stored beside it - visibility is the mechanism, and one fact should not need to
be kept true in two places.

Three tests hold it: `TestParseHonoursTheAdvertisedSet` checks both directions of
the array/parser split and that the zero set means "default", not "nothing";
`TestToolSetRenderingsAgree` asserts every listed name is accepted and every
accepted name is listed; `TestPromptListingFollowsTheToolSet` renders the shipped
template and proves the gate is live by handing it a bash-only set and requiring
the jobs prose to disappear. `tpl`'s `TestShippedTemplatesRender` - the golden
guard on the shipped prompt - failed on the new variables the moment they were
added, which is exactly what it is for, and it now declares them with the same
literal-value reasoning it already documented for `view_lines`.

**Feature 1: subagents** (`job/local.go`, `job/job.go`, `model/tools.go`,
`agent/agent.go`, `host/spawn.go`, plus their tests).

An `agent` tool call opens a child session, registers it as an **agent-kind job
of the parent's registry**, and returns the job id immediately; the child runs its
own conversation in its own row, and its report arrives as a background result at
the parent's next step boundary. Everything new sits in four places, and the
deliberate absence of places is the design:

- **`job.Spec.Run`** is the kind seam - a caller-supplied body the manager puts on
  the same context, output file, state machine, firing and cap as a bash run. The
  stopper for a kind that is not a process is the cancellation, with no
  SIGINT-to-SIGKILL window because there is nothing to signal: the package doc's
  "a kind whose ending is not a process exit simply has none," made real in about
  forty lines. `funcRun.hurry()` keeps §9.2's batched unwind intact - a session
  with six live jobs is torn down in the time one takes.
- **Quiescence lives in the job wrapper** (`childRunner`), not the loop. A child's
  `Loop.Run` still ends beside a running job exactly as a top-level session's
  does; the wrapper asks the child's registry whether anything is still owed, and
  if so waits on `Manager.OnQuiescent` - an edge-triggered notification, fired on
  the watcher's goroutine after the row is recorded - then re-drives the child
  with `Loop.Resume(live history)`, which is the resume path, reused. §9.10's
  four-way select is a real select over wake, context and a watchdog, and the
  watchdog exists so a forgotten server cannot park a goroutine forever; the
  report is whatever the child's eventual quiescent turn says.
- **The report body rides the wake.** `describeEndedJob` reads an agent row's file
  and inlines it up to the observation window, path for the rest - same
  truncate-and-spill rule as every other result. A bash job's ending is a pointer
  because a build log is not something to paste; a subagent's ending *is* the
  payload, and making the parent spend a turn fetching it hands back exactly the
  cost the child was meant to save.
- **A watchdog is not a timer.** Found by re-reading this code, not by a test, and
  worth the space because it is the kind of bug no suite catches: the wrapper's
  30-second belt returned "re-drive now" on every tick, so a child waiting on an
  hour-long training run would have been woken with **one model query every thirty
  seconds, forever** - the polling that §5 and the child's own prompt spend real
  effort forbidding, smuggled back in as a safety measure. The tests passed because
  their children finish in milliseconds and never reach a tick. It now re-checks
  the registry and keeps waiting unless something actually changed. The lesson is
  the general one: bounding a wait and clocking a wait are different acts, and the
  difference only shows up at timescales a test does not run at.
- **Two tools, both job-addressed.** `agent` spawns; `job_send` delivers text to a
  running child at its next boundary (the path typed input takes) or restores a
  finished one and then delivers - §9.3's revival as one mechanism seen twice. It
  refuses anything that is not an agent job with the flat `Cannot send to job`,
  which is right for a capability that does not exist rather than a policy the
  model could argue with. `jobs` and `job_kill` needed no changes, which was the
  §9.5 bet.

The two-level tree is now enforced three times over, deliberately: the tool is not
advertised to a child (§5.6, and its prompt says nothing about it - not even "do
not spawn", since a negation is still a mention), `session.Create` refuses a
grandchild, and `Spawn` refuses from a non-top-level session. Three layers that
agree cannot drift, which is the lesson §3.3 took from qwen.

**One deferral, since retired.** Confirmation bubbling - `ui.Question`'s origin
field, and a child's prompts reaching the parent's human - landed in the next
commit, and it deleted the guard described here rather than weakening it. What
this section shipped was `Spawn` refusing a parent that was neither yolo nor
already unattended, because a child running with no `Hooks` meant no prompts at
all, and a `confirm`-mode parent that spawned one handed a second agent more
permission than the human had granted the conversation. That was the correct call
against the code of the moment, and the guard's own test was written to skip
itself the day it went stale - which is the day the follow-up landed, so the
refusal went out with its test asserting the better behaviour instead.

A child's `BeforeExecute` now forwards to its parent's hooks, so the human who
gates the parent gates the child. Its name reaches that prompt through the loop's
own template variables (`asker`), which `ui.Question.Origin` carries and both
front ends render - attribution declared by the asker, exactly as §3.7 learned it
must be, rather than derived from ids that cannot be compared across layers.
Everything else a hook can be asked deliberately does **not** bubble: whether the
run is over, whether its limits may rise, what the human would like typed next -
those are questions about the conversation the parent's human is watching, and
answering them for a child would have them deciding about a run they were never
shown. A parent with no hooks spawns an unattended child whose prompts then
*reject* rather than hang, which is omp's posture from §3.7 and the case the guard
used to refuse outright.

**§9.3's revival also landed**, found by reading the send path rather than by a
failure: a send to a *finished* child queued a message behind a wrapper
goroutine that no longer existed. No error, no crash - a dead letter, and a parent
waiting on a second report nothing was left alive to write, which is the worst
shape a bug can take in a system whose whole promise is that results arrive
unprompted. A child now has three states a send can meet, and they are
distinguished by a flag rather than inferred, because a non-terminal row means
either "parked, somebody will re-drive this" or "mid-turn" and only the first wants
a nudge: running → queue it, parked → queue and wake, finished → **start a new
agent row over the same transcript** - a revival, not a reopening, since the state
machine has no legal trip from `delivered` back to running and inventing one would
cost the structural termination the jobs design leans on.

Two findings from making that work, both kept because they change behaviour
rather than cosmetics:

- **The lookup was asking memory.** `Manager.Get` sees live handles, so a
  finished job cannot be found by id at all - the send resolved to "unknown job"
  for exactly the case it exists to serve. Sent to the store instead, which is
  what the package doc already says is the source of truth about what a job *is*;
  a handle is only the mechanism for signalling one.
- **A child's budget is per session, not per run, and revivals spend it.** The
  cost limit belongs to the config and the spend to the loop, and `Run` does not
  reset the ledger, so a child re-driven for a second pass is refused partway
  through on the strength of what its first pass already cost. That is the right
  rule - a subagent that could clear its own ledger by being re-asked has no
  ledger - but it is invisible until someone hits it, so `withCost` says so at its
  definition, and the spawned report below now names the spend out loud.

**§9.9's table became code**, because the revival test tripped over the empty
cell: a child stopped by its cost limit ended with `LimitsExceeded` and no
submission, and the wrapper wrote "(the subagent ended without a report)". That is
a summary of nothing - it cannot distinguish a helper that ran out of money from
one that ran out of turns from one that had nothing to say - so an ending that did
not submit now reports which ending it was, how many calls and what spend it took
to get there, and where the transcript is. A parent that knows its child ran out
of budget re-delegates more narrowly; one that knows it ran out of *turns* knows
the task was too vague.

Three tests: the gate forwards with the right name attached while the parent's
run-shaping hooks stay untouched; an unattended parent gives its child no route to
a human who does not exist; and two queued asks do not arrive wearing each
other's labels, which is the attribution failure that would be found by a human
approving the wrong thing rather than by a suite.

Agent jobs are exempt from `DefaultMaxJobs` at **both** cap sites - `Detach`'s row
count and `register`'s live-handle count. The second one is the instructive bug:
exempting only the first produced a refusal that named the cap for a job the
comment three lines up says must not be capped, which is what two places encoding
one rule always gets you.

**Prerequisite complete.** §7 steps 1-4 and §5.6 are built and tested; ten
packages pass under `-race`, twice. What is left is the two features: step 6's spawning
and `job_send`, and step 7's UI. A second session can be opened, run, enumerated,
reaped and given its own tool set today; it cannot yet be *reached* by another
session, and `agent` is not yet in either set - by construction, with the
comment above saying so.

### A known flake, pre-existing

`job`'s `TestCapRefusesAndNamesTheRemedy` fails roughly once in fifty full-suite
runs, always under load, always with "cap did not release after the reap". It was
chased while landing this work and cleared of suspicion by the commit series
itself: it reproduced on a commit that touches no `job` file at all, so it
predates the kind seam, the cap exemption and `noteQuiescent` - and the last of
those returns before its extra store read whenever no hook is set, which is the
case in every `job` test.

The mechanism the evidence points at is the one the test exercises hardest: its
command forks two `sleep`s, and `KillAll` waits a bounded `unwindAllowance` per
run. On a saturated box that window can legitimately expire with a row still
being written, and the next `Start` then sees one live job. That is the
documented design - "a kill that is working is never reported as one that
failed" (`job/job.go:57-62`) - rather than a bug in it, so the fix belongs in the
test: await quiescence instead of trusting the reap's deadline. Noted here so the
next person who meets it does not spend an afternoon suspecting their own
concurrency work.

### 8.3 Step 7.2: the window became a `shell`, and a `GUI` became one session

The plan said "extract the per-session state `GUI` currently holds singularly into
a `tab`". It landed the other way round, for one reason: `ui.UI`
(`Reasoning`/`Content`/`Message`/`Ask`/`Interactive`) plus the optional `Issuer`
*already is* the per-session contract, and `GUI` already held exactly the
per-session state behind it - so making `GUI` mean "one tab" and moving the
window's own belongings into a new `shell` type cuts the same seam with a fraction
of the renaming, and every existing `g.<field>` call site stayed valid. What the
plan asked for is true either way: no per-session field is reachable from two
sessions, and the ask queue moved with the session.

`ui/shell.go` is the window: `app`, `window`, the `DocTabs` strip, the menu bar,
the sticky thinking preference and window size, the one `app.Run()` and the one
per-second tick. `ui/fyneui.go`'s `GUI` is one session: `cfg`, `loop`, `stream`,
`pending`, `lastQ`, `asks`, `inflight`, `runDone`, `cancelRun`, `task`, `resume`,
`sessionID`, its own mutex, and its own `guiView`. `NewGUI(o)` still returns a
`*GUI` with a window around it, which is why `cmd/mini` and all 1673 lines of GUI
tests needed no restructuring.

Three collisions the step existed to kill, each now covered by a test in
`ui/shell_test.go` that two sessions in one window run:

- **streaming.** One shared `streamState` interleaved two turns and painted the
  result into whichever tab repainted first. Each `GUI` owns its buffers, and its
  `Loop.Handler` points at *its* `Content`/`Reasoning`.
- **questions.** One `pending` slot and one `askQueue` meant a keystroke at one
  session could answer another's `AskConfirm` - §5.5's safety property, one shared
  field away from being void - and an unrelated repository's confirmation queued
  behind a prompt nobody was looking at. Per-session queue (decision 15's
  "one-at-a-time becomes per-tab"), per-session panel, per-session `Origin`-free
  attribution: the tab *is* the attribution now.
- **window chrome.** A background session's tick retitled the window with its mode
  and its input could reclaim the caret. The title and the enabled state of
  `Run ▸ Cancel turn` are applied by the visible session only, and what was last
  applied is remembered on the shell (`lastTitle`), not on the view - a tab that
  set the string while hidden would count it as applied and never show it when the
  human finally selected it.

Two findings about Fyne itself, both verified against source:

- **`DocTabs.OnSelected` is what moves the keyboard.** Selecting a tab by click
  selects the *content* and nothing else, so the newly visible transcript has no
  focus owner (and the outgoing one's deferred `FocusLost` reclaim is exactly the
  wrong thing to let fire). `shell` wires `OnSelected` to
  focus-the-now-visible-input + refresh-its-status. `Select` on the already-current
  item is a no-op that does *not* fire the callback, so `selectTab` repeats the
  work rather than relying on it.
- **`FocusManager.Focus` (internal/app/focus_manager.go) walks the complete object
  tree and returns success *without switching focus* when an ancestor is hidden.**
  So the toolkit already refuses to focus a hidden tab's input, which the focus
  tests in `shell_test.go` cannot see - the assertion there is a net, not a proof.
  The `isCurrent` clause in `chatInput.FocusLost` stays for the gap the toolkit
  does not cover: a deselected tab is not hidden *instantly*, and through DocTabs'
  transition the outgoing view is still visible and still focusable.

Not in this step, still owned by later increments: `CreateTab` and the
working-directory picker (7.3), the child-created callback (7.4), the attention
marker and `⌥→`/`⌥←` and the real `CloseIntercept` teardown (7.5), and splitting
the slash commands into tab-level and window-level with the batched cancel across
N sessions (7.6). `/quit` is still window-level: it cancels every session's run in
flight, which is what it always did with one.

### 8.4 Step 7.4: a spawn opens a tab, and the child's prompts go to the child

Decision 15's "bubbling becomes routing" is wired, and with it the re-ruling that
decision 15 demanded of decision 13: **attended-ness follows the front end, not the
parent.** A child session is attended when the process can give it a window, and
`host.Process.OnChild func(*Session) error` is how the GUI says so: `cmd/mini`
installs it, `GUI.OpenSession` builds the child's front end, and the child's
confirmation prompts, transcript, input and tabs are its own from its first step.
Where no `OnChild` is installed - print mode, a batch run, every existing `host`
test - the child still bubbles to its parent's human through `bubblingHooks`, which
is unchanged behaviour rather than a fallback nobody meant to keep working.

**The ordering was the finding, and the old comment was false.** `Session.Spawn`
used to call `Jobs.Start` and *then* assign `child.Loop.Origin` and
`child.Loop.Hooks`, with a comment asserting "Nothing has run yet at this point -
the job below is what starts the loop". The job below was the one started above:
`Jobs.Start` launches the run goroutine, and `agent.Loop` reads `Hooks` and
`Origin` with nothing guarding them, so the child could win the race and run its
first steps with **no confirmation gate at all** - §5.5's authority widening,
reached by timing rather than by design, and invisible in a test that never loses
the race. Everything the child needs before it can be asked to run is now bound
before `Jobs.Start`, and `OnChild` fires inside that window; three assertions in
`TestOnChildAttendsASpawnedSession` pin it (origin already set, hooks still nil,
session not yet running), each of which fails when the assignment moves back down.

Two more decisions fell out of the same seam:

- **`Session.unattended` is deleted.** It was written for every subagent and read
  by nothing, while its comment claimed the inheritance rule this step replaces.
  Attended-ness is now a property of the routing that exists, not a flag nobody
  consults.
- **A child that cannot be given a front end does not run.** `OnChild` returns an
  error, `Spawn` closes the half-built child and hands the reason back to the model
  as a refused tool call - decision 13's "no asker means refuse", applied to a front
  end that failed to open rather than one absent by configuration
  (`TestSpawnRefusesAChildItCannotAttend`).

`GUI.OpenSession` is callable from any goroutine, because that is what a spawn is:
the session's state, its streaming handler and its hooks are bound synchronously on
the caller's goroutine, and only the view and the tab item are posted to the canvas.
So there is a real window - a few milliseconds, one drained queue - in which the
child is attendable but has no tab yet, and that ordering is asserted rather than
assumed (`TestGUIChildSessionIsAttendedInItsOwnTab`, which calls `OpenSession` from
another goroutine and checks the hooks landed before the widgets did).

The strip's label is the only surface a background session has: `TabItem` carries
text and an icon and no widget, and DocTabs' jump menu is text-only upstream
(consequence 2), so a prompt waiting in an unselected tab puts **⚠** on its label,
raised when the question is published and cleared when it is answered or when the
human selects the tab. Nothing else will tell a person that a child is parked
waiting on them - which is what makes routing affordable over bubbling in the first
place.

## 9. Open questions

Organised by whether implementation can start without the answer, because that
is the only distinction that matters at this point.

Numbering runs continuously across the drafts of this document rather than
restarting: §9.2 and §9.3 are cited from §5 and §7, and a renumbered reference
that silently means a *different* question is worse than a visible gap in the
list. Retired questions keep their slot and say they are retired.

### Blocking steps 1-5, the prerequisite

Nothing. The last open item was the gate's default - **4**, stored and
configurable (§5.7) - and that step is built: `model.Gate`, taken inside
`Sushi.attempt`, with `model.DefaultGate` as the opt-out-free fallback (§5.7).

### Step 6 (feature 1)

Twelve numbered questions, ordered by how much of step 6 each one decides rather
than by difficulty. **Eight are answered** - §9.2 through §9.9, in the round-up
below, which is where the reasoning stays. **Nothing remains open.** §9.10,
§9.11 and §9.12 were answered in the fifth round - the first by ruling the
question malformed, the second by naming the tool `job_send`, the third by
keeping the one `recover` and making it loud, which turned out to be a code
change rather than a design one. §9.1 is a retired placeholder kept so the
numbering does not move. **Step 6 is specified.**

### The fourth round's answers

Eight rulings, each consistent with the analysis it closes; two of them changed
what step 6 contains. The items keep their full reasoning below.

- **§9.2 - the cascade goes all the way down.** Quitting or abandoning a parent
  session does the same to everything related to it: its own jobs, its child
  sessions, and those children's jobs. The four details listed there are the
  implementation - children-before-parents, one batched unwind, the children's
  registries swept first, and the count reported to the human.
- **§9.3 and §9.5 - `job_send` is in scope, so step 6 is two tools and not one.**
  Reviving a child that already reported is not a special operation at all:
  **restore it like any other session, then send it a message.** That collapses
  reopen-versus-new-job, because there is nothing to reopen - the transcript is
  the durable thing, and a send is the only way to make it live again. Two
  mechanical consequences:
  - A send reaching a child **with no live run** must start one, so the tool
    needs `host`'s resume path (`Loop.Resume` with the stored transcript
    authoritative, §9.3) as well as `QueueMessage`. One tool, two targets: a
    running child gets the text at its next boundary; a finished one is restored
    and then driven.
  - A second report is a **second wake**, and one row cannot carry it - the state
    machine has no legal `delivered -> running` transition, and inventing one
    forfeits the structural termination the jobs research leans on ("jobs only
    finish a finite number of times", docs/jobs-research.md §4). So **one new
    agent row per send to a finished child**, same `title`, and a handoff that
    says *resumed* rather than *started*. The parent's spend still rolls into the
    tree total by `root_id`, which is exactly what §5.8's derived query is for:
    five revivals of one named subagent answer "what did this cost" with a single
    indexed sum and no accumulator to get wrong.
- **§9.4 - permissions queue, asked and answered one at a time, for now.** The
  FIFO of option (a). The pending-slot clobber still gets fixed early (§7 step
  2), because a queue is only well-defined once an `Ask` cannot be silently
  dropped. "For now" is honest: three waiting children means the third waits,
  which is correct but will want the roster's per-session surface later.
- **§9.5 - the send tool is job-addressed.** The ruling's own words are "a tool
  to send text to a running job", which for an agent injects a user message at
  the next appropriate boundary - **the path user input already takes**
  (`QueueMessage`, `agent/agent.go:807`, drained at `:1120`), and omp's
  semantics exactly: "Steering uses the normal prompt path, so the message and
  response are written to the subagent's persisted session history"
  (`docs/agent-hub.md:80`). Standard jobs accepting input the same way is
  deferred, which is why the tool should be named and shaped for jobs generally
  - §9.11.
- **§9.6 - the cache cost is real but marginal.** Freshly spawned agents barely
  share a cache worth speaking of with anything, so nothing is designed around
  it. What survives from that item is §5.6's *visibility* requirement, which
  stands on its own: the child's listing is generated so a tool it was never
  told about cannot appear in its instructions, not for cache shape.
- **§9.7 - the report is the final LLM output when there are no background turns
  left to trigger further turns**, which is §5.12's quiescence named directly.
  And children are **guided to have all their background jobs finished before
  issuing it.** So the duty stays in the prompt, where §5.10 said it belonged,
  and waiting is the backstop for when guidance is not enough. This mostly
  *removes* §9.7's question: a child that drained its jobs has nothing to
  declare, and a child that did not is waiting rather than reporting. The
  paragraph step 6 has to write is therefore about *how* to finish - take the
  wake, act on the result, then report - rather than a manifest of outstanding
  work, which would be a form that only ever gets filled in when something
  already went wrong.
- **§9.8 - the child's config is constructed as part of the `agent` tool call,
  and it must be a copy.** Both halves matter, and the first is stronger than any
  allowlist: if nothing is inherited, there is no map to subtract from, so the
  `output_path` and resume-id leaks cannot occur by omission. The copy is not
  pedantry - `Loop.Run` writes through `l.Cfg` when limits get raised at the
  `AskLimits` boundary, so a shared pointer would let a child change its
  parent's step limit mid-run. `ConfigFromMap` per session, already the practice
  in `agent/parallel_test.go`, becomes the rule in `host`.
- **§9.9 - panics stay loud and fatal**, which retracted a design already
  written: see the rewrite there, §6 item 3, and §9.12 for the scope question.

#### 9.1 - 9.3

**1. (retired) The gate's default** - answered: 4, stored and configurable
(§5.7), and the gate is built.

2. **Does ending a parent cascade into the child's registry?** *Ruled: yes.*
   A child's own end
   cascades cleanly (§5.10): killing the child runs the child's `KillAll`. But
   when the *parent's* session closes, its `KillAll` reaches the agent-kind job,
   and stopping that job has to stop the child loop *and* the jobs the child
   started - three levels of "a job outlives a turn, never a session" where each
   level's session is a different session.

   The data model is already there and the traversal is not. `Spec.ParentID` is
   documented as linking a run "to the job that spawned it, for trees and for
   reaping a parent's children" (`job/local.go:52`), and it is persisted on
   every row (`session/jobs.go:80,109,183`) - but nothing anywhere queries by
   it, and no reaper follows the edge. So this is a question about ownership of a
   walk, not about a missing column, and the walk has two halves that cannot use
   the same mechanism:

   - *A child's own jobs* live under the child's `session_id`, so they are one
     call away inside the child's own manager. Trivial.
   - *The child's manager itself* is in a different session, and the database
     cannot bridge that honestly: after a restart a child's `running` row is a
     claim about a process this process does not own, which is exactly the
     reasoning behind §5.11's sweep-instead-of-signal rule. So the parent can only
     reach children it started **in this process**, which means the edge has to be
     held in memory - `host` keeping its children, the way crush's coordinator
     keeps `dispatched` keyed by parent session and label (`coordinator.go:178`).

   **Suggested:** `host.Session` owns an in-memory list of the child sessions it
   opened. `Quit` on a parent cancels each child's context, which makes each
   child reap its *own* registry through code that already exists, and only then
   reaps the parent's rows - children-before-parents, the order that cannot leave
   a running process whose session is already closed. Four details fall out of
   that and all of them are load-bearing:

   - **Sweep ordering on restart.** The child's jobs must be swept before the
     parent's agent rows test for quiescence (§5.10, §5.12), because the agent
     job's terminal state is gated on its child's live-job count. Sweep the
     parent first and it decides "still waiting on two jobs" from rows that are
     about to be marked orphaned, then waits on a number that will never move
     again. The store's sweeps and the manager's sweeps need an explicit order,
     and the order wants a comment saying why.
   - **Do not serialize grace windows.** `unwindAllowance` is per run
     (`job/job.go:57-62`): two grace periods plus slack, sized for one SIGINT
     chain. A cascade that awaits children one at a time would take N x that at
     quit, and a quit that takes nine seconds looks like a hung one. Cancel all,
     then wait once, with a single deadline for the batch.
   - **`jobs` must not over-promise.** A child's jobs are invisible to the
     parent's `jobs` call, because that lists the parent's session. Either the
     listing grows a per-row "2 of its own jobs still running" (which needs the
     memory edge above), or §5.10's wait for quiescence is opaque to the model.
     Recommend the count, not the ids: it answers "why is this one still
     running" - which after §5.12 is a question the parent will ask more often,
     because "still running" is now the honest answer rather than an anomaly
     beside a `Submitted` row - without a second listing the model has to
     chase.
   - **Report it truthfully.** The human should see "stopped 3 subagents and 5 of
     their jobs", not a silence, for the same reason the sweep warns: mini cannot
     prove anything about what it did not start.

   **Blocks:** `host.Session.Close`'s shape, the sweep ordering, `Quit`'s
   reporting, and whether `jobs` grows a column.

3. *Ruled: implement `job_send`; revival is restore-plus-send.*
   Is a follow-up on a finished subagent a new run of the same job (crush's
   reopen, needing §3.2's generation counter) or a new job on the resumed child
   session?**

   The question underneath this one is sharper than it first looks, and step 6
   cannot start without it: **§5.4 forbids a human resuming a child, but a
   follow-up from its own parent has to be allowed** - otherwise the name, the
   transcript, and all of the child's work are wasted the moment it stops. So
   `kind` is being asked to answer two different questions and currently answers
   one: "may `-r` pick this up" (no) and "may the session that made it continue
   it" (yes).

   The clean resolution is that the *caller* differs, not the row. `Find` and
   `List` keep filtering to `kind = 'top'` for every human path; the host resumes
   a child by explicit id with no filter at all. That is precisely omp's split -
   the ambient session is the human's, while the Hub can steer, prompt, and
   **revive** a subagent, where "a killed agent comes back as a fresh instance
   with its full history" (`docs/agent-hub.md:59`).

   **Suggested: revive as a new run on the same transcript.** It needs no `gen`
   column and no second state machine, and crush's generation counter exists only
   because its subagent is one long-lived object being reopened, which mini has
   no reason to imitate. mini is unusually well placed for this: `Loop.Resume`
   already seeds a history from stored turns (`cmd/mini/main.go:518-520`) and
   `Run` then takes the stored transcript as authoritative - "the stored
   transcript already carries the system and instance messages, so Run must not
   render or append them again" (`agent/agent.go:881-884`). That is the entire
   revive path, already written for a different purpose.

   Two consequences to accept deliberately:

   - **A resumed child must get its tool set from its own kind, not its
     parent's.** `Run.Resume` takes the stored transcript as authoritative, so if
     the advertised `tools` array came from anything but the session's own kind,
     a resumed child could be given the spawn tool back - the exact drift §3.6.1
     warns about, arriving through the resume door rather than the creation door.
   - **Two rows for one named thing.** The parent may then see "quantise 70b"
     twice in `jobs`. That is fine if the second is visibly a continuation
     (`jobs` prints the parent's row for the first, and the label carries over),
     and confusing otherwise. Cheap fix: the follow-up's label gets the same name
     and the wake text says *resumed* rather than *started*.

   **Blocks:** whether `agent` grows a "continue this one" parameter, the shape
   of the child registry in `host`, and §5.5's name-hold rule (a name frees when
   the child is *done*, so a resumable child must be able to be not-done twice).

#### 9.4 - 9.9, found while expanding this section

4. **Both front ends assume exactly one question is outstanding, and feature 1
   breaks that in two different ways.** *Ruled: a queue, one at a time.*
   This is the one item here that is a
   **live bug today** rather than a design question, and it is the precondition
   for §5.5's bubbling.

   `GUI.Ask` overwrites `g.pending` with the new channel and stores the new
   question (`ui/fyneui.go:442-445`). Nothing rejects a second concurrent call.
   The first asker's channel becomes unreferenced - `resolve` only ever sends to
   whoever is in the slot now (`:474-480`) - so the first agent goroutine blocks
   in its `select` until its run context ends. Not a wrong answer: a silent
   hang, on the goroutine that owns a conversation, with the prompt showing
   somebody else's question.

   `Print.Ask` has the same assumption expressed differently: it writes
   `q.Title` straight to `p.Out` and reads from one shared stdin with no
   serialization (`ui/print.go:237-241`). Two concurrent asks produce interleaved
   prompts competing for one line reader.

   Neither is reachable while one session exists, which is why it has survived.
   Once a child's confirmation bubbles to the parent's front end, two goroutines
   can ask at the same instant as a matter of course.

   **Options:** (a) a per-front-end FIFO queue - one question visible at a time,
   each labeled with its §5.5 name, plus a "+N waiting" count, with the UI's
   `HasQuestion` becoming "N outstanding"; (b) decline to bubble and make
   children unattended, which is crush's read-only access tier
   (`agent_dispatch.go:383-402`) and qwen's reasoning for `approvalMode: 'bubble'`
   in the negative ("a detached fork has no inline UI, so 'default' would
   auto-deny them", `fork-subagent.ts:34-36`); (c) per-child question surfaces,
   which is a step-7 UI feature and cannot be built before this is understood.

   **Suggested:** (a), and **fix the clobber now, in step 2, with a test** - not
   during step 6. **Done, that way:** `ui/askqueue.go` is a cancellable FIFO
   ticket queue wired into both `GUI.Ask` and `Print.Ask`, and
   `ui/askqueue_test.go` asserts turn-taking, strict order, cancelling a *queued*
   waiter, cancelling the *holder* without wedging the queue, and the original
   two-asks clobber itself. A ticket queue rather than a `sync.Mutex` precisely
   so a waiter can be cancelled. Worth recording that the fix needed a test of
   its own to land: the first version of the GUI test deadlocked by calling `Ask`
   inline and then trying to `resolve` a question that had not returned yet -
   which is a fair illustration of how this bug hides.
   The reasoning is unglamorous and important: the bug is in code
   both features share, its symptom is a wedged goroutine weeks later in
   unrelated work, and a queue is the smallest change that makes the second
   `Ask` well-defined. Whatever mini decides about *displaying* children's
   prompts, "an `Ask` that is silently dropped" is never correct.

   Also decide while here: whose answer is it when a parent question and a child
   question are both pending and the human types? With a FIFO the answer is
   mechanical, which is the fourth reason §5.5's origin field cannot be deferred
   - an unattributed queue is how a human approves `rm -rf build/` believing they
   pressed enter on their own command.

5. **How many tools does feature 1 actually need?** *Ruled: two, spawn and
   send.* The
   surveyed norm is three or four: crush ships `agent_dispatch`, `agent_send`,
   `agent_status`, and `agent_stop` (`agent_dispatch.go:378,676,764`,
   `agent_dispatch_status.go`), qwen has `list-agents`/`task-create`/`task-stop`,
   omp has the Hub.

   mini already owns two of those four by construction. **`jobs` is
   `agent_status`**: it lists id, state, elapsed, exit, and the path output is
   accumulating at (`model/model.go:255`), and an agent-kind row has all of
   them, because §5 chose the registry as the object. **`job_kill` is
   `agent_stop`**: a kind whose ending is not a process exit simply has no
   stopper (`job/job.go:9-10`), so killing an agent job means cancelling its
   context, and the tool already takes an id and already needs no confirmation
   for exactly that reason (`model/model.go:274`).

   So feature 1 is one new tool, `agent`, and the only capability genuinely
   missing is `agent_send` - steering a running child. Recommend deferring it:
   mini has the mechanism (`Loop.QueueMessage`, `agent/agent.go:807`, which
   is exactly omp's "Steering uses the normal prompt path", `agent-hub.md:80`)
   but no way for the *model* to address a child, and adding one parameter later
   is not a redesign.

   **The cost of deferring it should be stated rather than discovered:** without
   send, a parent that notices a child going wrong can only kill and respawn it,
   losing the child's work. That is acceptable for read-mostly subagents - which
   is the same class §5.9 declines to isolate - and poor for long write-heavy
   ones. If feature 1 turns out to be used mostly for edits, this question comes
   back with an answer already written by omp and crush.

   Recording the tool count matters for the claim in docs/jobs-research.md §2
   that mini ends up with *fewer tools than every harness surveyed*: one spawn
   tool on top of three keeps that true, at four. It also keeps §5.6's generated
   listing short, which is the whole reason that listing is worth generating.

6. **Does making the spawn tool invisible cost the prompt cache?** *Ruled:
   marginally, and not worth worrying about.* §5.6 gives a
   child a different tool set, which means a different system prompt prefix,
   which means no warm cache to inherit from its parent. On a box where the
   compaction design goes out of its way to issue its summary as a
   "cache-reading side-query" (`config/configs/mini.yaml:2-8`), that is worth
   checking rather than waving.

   qwen is the cautionary precedent and it decided the opposite on purpose: a
   fork keeps the `agent` tool in its declarations "for byte-identical cache
   parity with the parent" (`fork-subagent.ts:47-48`) and refuses nested spawns
   at runtime instead.

   But the comparison does not transfer, and working out why is what settles it.
   A fork *inherits the parent's conversation*, so its prefix literally is the
   parent's and any divergence in it costs the hit. mini's subagent starts fresh
   - the ruling in the feature statement is "an initial prompt detailing exactly
   what the agent needs to do", not a copy of the parent's history - so it never
   shares the parent's prefix whatever the tool list says. What children *do*
   share with each other is one system prompt, so N concurrent children of one
   parent warm one prefix between them, and that sharing survives the ruling
   intact.

   **Recommendation: proceed as §5.6 rules.** One condition comes out of this
   analysis and it should be honored: the child's prompt must be *identical
   across children*, so the paragraph from §9.7 below is generated from one flag
   rather than assembled per spawn with anything task-specific in it. A prefix
   that varies with the task forfeits the only cache the design gets for free.

7. **What is a subagent's report, actually?** *Ruled: the final output at
   quiescence, and children are prompted to drain their jobs first.* §5.10 and
   the Report contract say
   *the submission text*, and §5.10 adds that the child must declare jobs it left
   running. Nobody has decided what the child is **told** to put in it, and the
   current prompt is written for a human reader: "make it a summary worth
   reading", "ready for the user's next prompt"
   (`config/configs/mini.yaml:34,54`). A subagent's reader is another model with
   no context at all, and what that reader needs is different in kind: which
   files it changed and where they are, which jobs it started and where their
   logs are, what it did *not* accomplish and why, the exact command that failed
   with its error.

   **Options:** (a) the same template plus a conditional paragraph gated on a
   `subagent` flag in `TemplateVars` - cheapest, and the flag is the same one
   §5.6's generated listing already requires, so it costs one boolean and one
   `{{if}}`; (b) a separate `agent.subagent_system_template` in config, which is
   more control and another thing to keep from drifting; (c) a structured report
   (JSON with fields), which is parseable but cuts against the prose-in-
   observation style every other mini tool result uses.

   **Suggested:** (a), plus telling the child its directive came from another
   agent rather than from the human - otherwise it addresses its summary to "you"
   and hedges with next steps it could have taken itself, which is the most
   common way a subagent report wastes its budget. Cap the body like any other
   tool result and append the child's transcript path, since §5.11 puts those
   files inside the parent's own subtree: the parent can already `bash` to read
   more, and that needs no new mechanism, only a path in the report.

   This one needs the maintainer rather than an analysis, because the report is
   where the prompt contract decides what a subagent is *for*, and prompts are
   the product here.

8. **Which config keys must never cross into a child?** *Ruled: the child's
   config is constructed in the tool call, and it must be a copy.* §5.7 and build
   step 4
   already establish the concrete trap: `agent.output_path` - from `-o` or from
   the per-session default - would otherwise be written by parent and child
   alike, which is the clobbering bug this document's §4 item 3 says it fixed,
   reintroduced through inheritance. `-r`'s resume identity must not cross either,
   and neither may `mode` or `whitelist_actions` ever widen (§5.5).

   **Suggested: an allowlist of what a spawn may override** - model name,
   thinking effort, step and cost limit, and the task itself - merged over the
   parent's merged config, rather than a denylist subtracted from it. The reason
   is the failure mode: a denylist that forgets one key fails quietly, by having
   two sessions write one file or two sessions share one resume id, and nothing
   raises an error to tell anybody. An allowlist that forgets one key fails
   loudly, by refusing a child the model asked for.

   Worth naming the trap in code where it is implemented, whatever the answer:
   `agent.Loop.Run` sets `l.Messages = nil` and re-renders templates from
   `l.Cfg`, so a child that inherits the parent's `*agent.Config` **pointer**
   rather than a copy would share mutable run state with its parent - the same
   class of mistake as sharing `Row.Title`. `ConfigFromMap` per session is
   already the habit in these tests (`agent/parallel_test.go`) and should become
   the rule in `host`.

9. **What does a child that did not finish report?** §5.10 promises never an
   empty report, so step 6 needs the mapping written down once, because several
   different endings produce four wakes and each has to say something actionable
   - and one row is not an ending at all, which is the reason to write the table
   rather than describe it:

   | how the child ended | job state | report |
   |---|---|---|
   | no tool call, but a job may still deliver | **not an ending** - stays `running` | none yet. This is waiting (§5.12); the report comes from the step after the last firing |
   | quiescent - no tool call, nothing that may deliver | terminal | the submission text, plus declared jobs |
   | parent quit | `killed` | "stopped by the parent session closing" |
   | child panicked | - there is no row: **the process dies, by design** (§9.9) | none, and correctly so |
   | child hit its own limits | `failed` | which limit and at what spend - the truth of where it stopped |
   | restart mid-run | `orphaned` | §5.10's interrupted text, naming the kept transcript |

   This question is **answered, and its answer removed a row from the design.**
   The draft listed a panicking child as `failed` with a contained summary,
   copying zero's launcher (`swarm/member.go:70-78`). Ruled: **any panic in mini
   is a bug that must be fixed, not ignored or papered over, and while the
   project is in private active development panics are loud and fatal
   throughout.** A recover at the session boundary would convert a defect into a
   plausible-looking job state, which is the one thing a registry designed to be
   the source of truth must not do - a `failed` row silently written by a panic
   handler tells the parent "the subagent did not work out", which is a
   *finding* about the model's attempt when the truth is that mini broke.
   Consequently: no `recover` is added in step 3, §6 item 3 became a
   don't-do-this entry, and the price is accepted knowingly - one session's panic
   ends every session in the process, and their jobs with it, which is what
   §9.2's cascade is for and why the crash must name its session in the log
   (§7 step 2) rather than merely happen.

   The posture is explicitly for-now. The condition for revisiting it is
   unattended or long-running use, where "the whole box's work died because one
   subagent tripped over a nil" stops being a useful failure and starts being
   the failure; that day wants a decision made in daylight, not a `recover`
   dropped in as a defensive reflex. §9.12 is the scope question it raises.

10. **Was a session's wait going to hold `Run` open or return with a state the
    front end re-drives?** *Ruled: the question was malformed, and the answer is
    that top-level sessions are not touched.*

    The word "run" was doing two jobs there - the loop's `Run` call, and the
    session's life - and the analysis below chose between blocking and
    returning as though a subagent had to pick one for the loop's sake. It does
    not. **A subagent is a job that backgrounds itself once its session exists**,
    and nothing in this design changes how a top-level session operates. So:

    - The child's `Loop.Run` behaves exactly as it does today. A turn with no
      tool call and jobs still running still breaks out of the loop after logging
      (`agent/agent.go:1130`); `deliverOwed` is not amended, `exit_status`
      gains nothing, and no exit-contract audit is needed. The three sub-
      decisions this item used to carry - a `Waiting` status, its exit code in
      `-p` mode, what `--exit-immediately` means, and rewording "Agent is ready"
      - are withdrawn with it, because all four were changes to the human's
      session wearing a subagent's clothes.
    - **Quiescence is a property of the agent row, tested by the job wrapper.**
      When the child's run returns, the wrapper asks the child's registry for its
      live job count: if it is zero, the child was quiescent, and this run's
      submission *is* the report, so the row goes terminal and the parent is
      woken; if it is not, the row stays `running` and nothing is reported yet,
      which is the §5.12 truth expressed in the one place that can test it
      without touching anybody else's semantics.
    - **Re-driving is not new machinery.** When one of the child's own jobs
      fires, the host starts the child's run again on the same session. That is
      precisely what mini already does for a human resuming a session whose
      result was "queued for the next run" (`agent/agent.go:1148`) - the
      stored transcript, `Loop.Resume`, and `deliverOwed`'s step-boundary append
      are all existing code, reached by a different caller. So the four-way wait
      select this item feared never has to be built: there is no wait, because
      nothing blocks.

    `job_send` falls out of the same shape rather than needing its own: a child
    with a live run gets `QueueMessage` at its next boundary, exactly like typed
    input; a child without one is started the way a re-drive starts it, with the
    message queued first. §9.3's two targets are one mechanism seen twice.

    What §5.12 still stands for, stated precisely so it is not mistaken for a
    withdrawn item: the *model* - a turn with no tool call is not an ending while
    something may still report - is what makes the row's test correct. What is
    out of scope is rewriting the top-level loop's labels to match the model.

11. **What is the send tool called, and what does it refuse?** *Ruled:
    **`job_send`**, and for now it sends to subagents only - every other job kind
    is refused with a `Cannot send to job` error.*

    The job-general name is right for the reason the item was raised: the
    argument is a job id taken from `jobs`, and `jobs` and `job_kill` already
    address both kinds, so `agent_send` would promise a restriction the registry
    does not have. The refusal is a flat "Cannot send to job", which is the
    correct shape for a capability that does not exist yet rather than a policy
    the model could argue with - there is no remedy to name, because there is
    nothing for the model to do differently except pick an agent job.

    Two things the refusal should not quietly grow into. First, mini's foreground
    shell uses `/dev/null` for stdin (`env/local.go:143,154`), so making a bash
    job *actually* accept input is an interpreter change, not a branch in the
    send tool - fine, since it is deferred, but the tool must keep saying so
    plainly rather than accepting text it will drop. Second, a send needs no
    confirmation gate: `jobs` and `job_kill` are already exempt because they can
    begin nothing (docs/jobs-research.md §4, axis 8), and a message is the same
    case - it can only redirect work the parent was already allowed to start.

12. **Does "panics are fatal throughout" include the `recover` that already
    exists?** *Ruled: the recover stands - it is not our bug - but it must be
    loud and logged, so the fault can be worked around or filed upstream.*


    **Implemented, and it was quieter than it looked.** Checking the ruling
    against the code found that the `env` package had no logger at all: the
    recover turned an interpreter panic into a `Result` with
    `exception_type: Panic` and nothing else (`env/local.go:94,176-180`), so the
    stack existed only in the value the model was handed and no record of it
    survived the process. That is precisely the silent absorption §9.9 rules out
    for mini's own panics, arriving by a different route.

    The change: `Local` gained a `Logger` (nil means `slog.Default()`), the
    recover now calls `recoveredPanic`, and that logs at **error** with the panic
    value, the working directory, the **untruncated command** - it is the repro,
    so a shortened copy cannot be replayed - and `debug.Stack()`. The command
    still fails as before, because recovering here is the point. `buildRun` sets
    `e.Logger` to the session's logger, so the record carries `session=<id>` and
    a crash-adjacent mystery has an owner. `env/local_test.go` asserts exactly
    one record at error level with all four attributes, because "logged" is a
    policy claim and a comment cannot hold one.

    For the record, how the question arose: §9.9 settles code step 3 has not
    written yet, but mini already catches exactly one panic: `env.Local.Execute`
    recovers around `interp.Run`
    and reports it as a failed command carrying `exception_type: Panic` in the
    observation (`env/local.go:176-180`), because the interpreter is third-party
    and can panic on exotic input.

    The two readings differ in consequence. "Any panic *in mini*" leaves that
    recover standing - it is not mini's bug, and a command that takes the process
    down over a malformed glob is worse than one that reports failure, which is
    the same reasoning behind `env`'s existing per-command isolation.
    "Loud and fatal *throughout*" could mean it goes too. If so, a third-party
    parser panic becomes a dead mini, and `env/local.go`'s comment about
    "exotic input" should be read as a promise nobody intends to keep.

    One sentence of ruling settles it, and it should come before step 3, because
    step 3 is exactly where a reader would otherwise infer the new posture and
    delete the existing guard by analogy - the one change in this document that
    could be made by someone faithfully following it.

### Settled, recorded so nobody relitigates them

The fourth round's answers live with the items they close (§9.2-§9.9) rather
than here, except where they removed work: no depth column, no panic
containment, no reopen state machine, and no report manifest.

`root_id` is the session's own id at the top of a tree (§5.3). A second `Run` on
a live loop is **refused**, not blocked, naming `QueueMessage` as the supported
path - mini has a steering channel and zero does not, which is why their answers
differ. Startup model-info lookup is cached per process, since it bypasses the
gate entirely (`model/info.go:246-252`). Every record a session logs carries
`session=<id>`, and a gate wait is surfaced through its own `Sushi.OnGateWait`
hook rather than `OnRetry`, whose signature means a failed request (§5.7). **Gate
slots span the full request and response, streaming included** (§5.7). Cost
limits bound own spend only, and the tree total is a derived query (§5.8).
**Starvation is accepted**, with per-session pause/kill as the remedy, which is
why that ruling was affordable (§5.7, §7 step 7). **Subagent names go in
`title`**, already first-write-wins (§5.5), **unique per top-level session and
held until the child's own jobs end** - enforced in `nameAvailable`, which keeps a
name held while the child runs *or* has jobs running (§5.5, §5.10). **A session is never
finished** - a turn with no tool call means nothing-left-to-do only when no job
may still deliver, so a child reports at quiescence and a forgotten job is
recorded as waiting with a human's way out of it, never as completion (§5.12).
**A child is removable only with its parent** - before is data loss, after is
garbage (§5.11). Permission
prompts bubble with the name attached, and the origin field is part of step 6
rather than a later hardening pass (§5.5).

One question from the earlier list dissolved rather than being answered: whether
a top-level session waits at submission for children that are still running.
mini already decided the general case - "the run can end beside a server that
never exits", with the result "queued for the next run"
(`agent/agent.go:1148`) - and an agent job is just a job, so a human who
walks away mid-fan-out is resumed into the reports rather than being held at the
prompt. Nothing new to build, and nothing to rule.
