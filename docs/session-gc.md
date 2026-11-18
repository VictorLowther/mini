# Session collection

The retention policy, its knobs, and the boundaries it will not cross.

## Policy

| | |
|---|---|
| Unit of collection | a session **tree**: a root session plus its subagents |
| Grouping | by **project** - the git root when there is one, else the working directory (`projectOf`) |
| Exempt | the `keep` most recently used trees per project, always |
| Collected | anything else after `idle_days` without activity |
| Subagents | swept with the parent, never judged on their own |
| Knobs | global, in the sqlite `settings` table |
| Defaults | `keep = 3`, `idle_days = 15` |

Implemented in `session/gc.go` (`PlanGC` / `RunGC`), wired into `host.New` beside
the lineage and stray-result sweeps, and exposed as `mini sessions gc`.

## Why a tree and not a row

`updated_at` on the root says nothing about a subagent that is still working.
Recency for a tree is the **maximum over its members**, so an idle parent with an
active child survives, which is the case that would otherwise delete history from
under a running conversation. Collection then calls `Store.Delete` on the root
only - `Delete` already collects the subtree and removes every member's result
directory - so "subagents go with the parent" is structural rather than a rule
someone has to remember in two places. A child id is never handed to `Delete` by
GC, and `mini sessions rm <child-prefix>` remains the only way to collect one
child alone.

Result directories are named per **session**, not per tree, so a tree's footprint
is the sum over its members. Getting that wrong reports the root's files only and
understates what collection reclaims.

## Where the numbers live

`settings`, not the config file: `gc.keep_per_project` and `gc.idle_days`. The
config file is run configuration that scripts and `-c` overrides depend on; these
are interface state that should outlive a run and change without a rebuild, which
is the same reason the thinking toggle and window size live there. Reading
tolerates absence and garbage: anything unparseable, zero, or negative falls back
to the default, because the destructive reading of a typo - `idle_days = 0`
meaning "sweep everything" - is the worse one. `SetGCConfig` refuses
non-positive values outright rather than storing a landmine.

## What protects a live session

A tree is skipped if any member row carries a non-zero `opened_by` whose pid is
**alive** (`host.ProcessAlive`, signal 0, EPERM counted as alive). That reuses the
same owner stamp A-6 added, and covers three cases at once: another window's
session, another process's resumed session, and the session that is running right
now - this process's own pid is alive, so a collection pass cannot eat its own
transcript. A crashed process's stale stamp reports dead and does not preserve a
tree forever.

The CLI deliberately uses the store directly instead of `host.New`, because `New`
runs a collection pass: `mini sessions gc -n` would otherwise delete on the way in
to show you the plan.

## Preview first

`-keep` and `-idle-days` override the numbers **for that run only**; `-save` is
what persists them, and `-n` deletes nothing. Without that split, "what would this
collect at 1 day?" would silently rewrite the global policy, and the curious
experiment is always the aggressive one.

## Deliberately not done: shrinking the file

`auto_vacuum` is 0 on existing databases. Collection returns pages to the
freelist and the file does **not** shrink - it stops growing, because subsequent
sessions reuse the freed pages. That is the honest limit of what landed here.

Fixing it properly means `PRAGMA auto_vacuum = INCREMENTAL` followed by a full
`VACUUM`, because the mode cannot be switched any other way. That rewrites the
whole database (43 MB on the developer's store, seconds of blocking I/O) and would
have to happen once, at an upgrade, without being able to ask first - so it is a
decision rather than an omission. If it is wanted, do it in a named migration with
a progress line, not at startup.

## Boundaries of this policy

Old history is deleted, permanently, with its raw command output. Nothing is
archived or exported first: an automatic export before delete would create files
whose lifetime nobody owns, which is the class of orphan this feature exists to
clean up. `mini sessions export` remains the way to keep something, and the
`keep` exemption is the way to say "the last N in this project are mine".
