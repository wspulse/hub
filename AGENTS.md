# AGENTS.md — wspulse/server

This file is the entry point for all AI coding agents (GitHub Copilot, Codex,
Cursor, Claude, etc.). Full working rules are in
`.github/copilot-instructions.md` — read it completely before
making any changes.

---

## Quick Reference

**Module**: `github.com/wspulse/server` | **Package**: `server`

**Key files**:

- `server.go` — public `Hub` interface + `NewHub`
- `heart.go` — single-goroutine event loop (all state mutations here)
- `session.go` — `Connection` interface, `readPump` / `writePump`, resume logic
- `options.go` — `HubOption` builders
- `resume.go` — ring buffer for session resumption
- `errors.go` — sentinel errors
- `doc/internals.md` — internal architecture
- Wire protocol spec: [`.github/doc/protocol.md`](https://github.com/wspulse/.github/blob/main/doc/protocol.md)

**Pre-commit gate**: `make check` (fmt → lint → test)

---

## Non-negotiable Rules

1. **Read before write** — read the target file + `doc/internals.md` before any edit.
   For wire protocol details see the [centralized protocol spec](https://github.com/wspulse/.github/blob/main/doc/protocol.md).
2. **Heart serialization** — all session state mutations go through the heart's
   event loop. Never mutate session state from outside the heart goroutine.
3. **Goroutine lifecycle** — every goroutine must have an explicit exit
   condition. `Close()` must not leak goroutines.
4. **No breaking changes without version bump.**
5. **No hardcoded secrets.**
6. **Minimal changes** — one concern per edit; no drive-by refactors.

---

## Session Protocol

> `doc/local/` is git-ignored. Never commit files under it.

- **Start of session**: read `doc/local/ai-learning.md` in full (create with header if missing) and check `doc/local/plan/` for any in-progress plan.
- **Feature work**: save plan to `doc/local/plan/<feature-name>.md` before starting.
- **End of session**: append at least one entry to `doc/local/ai-learning.md` — **mandatory even if no mistakes were made**. An empty file proves the session protocol was ignored.
  Format: `Date` / `Issue or Learning` / `Root Cause` / `Prevention Rule`.
