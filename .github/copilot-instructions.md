# Copilot Instructions — wspulse/server

## Project Overview

wspulse/server is a **minimal, production-ready WebSocket server library** for Go. It manages concurrent connections, room-based broadcasting, session resumption, and heartbeat. Module path: `github.com/wspulse/server`. Package name: `server`. Depends on `github.com/wspulse/core` for shared types (`Frame`, `Codec`).

## Architecture

- **`server.go`** — `Server` interface (public API: `Send`, `Broadcast`, `Kick`, `GetConnections`, `Close`) and `NewServer` constructor. Implements `http.Handler`.
- **`hub.go`** — Central single-threaded event loop. Manages all sessions and routes messages via channels. No `net/http` imports.
- **`session.go`** — `Connection` interface and the unexported `session` struct. Per-connection `readPump` + `writePump` goroutine pair; ping/pong heartbeat; backpressure drop.
- **`options.go`** — `ServerOption` functional options, `ConnectFunc` type, and all `WithXxx` option builders.
- **`resume.go`** — Ring buffer for buffering frames during temporary disconnects (session resumption).
- **`errors.go`** — Server-only sentinel errors: `ErrConnectionNotFound`, `ErrDuplicateConnectionID`, `ErrServerClosed`.
- **`doc/protocol.md`** — Wire protocol specification.
- **`doc/internals.md`** — Internal architecture documentation.

## Development Workflow

```bash
make fmt        # format (gofmt + goimports)
make lint       # vet + golangci-lint
make test       # race detector, count=3
make check      # fmt + lint + test (pre-commit gate)
make bench      # benchmarks with memory stats
make test-cover # coverage report → coverage.html
make tidy       # tidy module dependencies
```

## Conventions

- **Go style**: `gofmt`/`goimports`, snake_case filenames, GoDoc on all public symbols, `if err != nil` error handling (no runtime panics; setup-time programmer-error panics are allowed — see **Panic policy** critical rule), secrets from env vars only.
- **Naming**:
  - **Interface names** must use full words — no abbreviations. Write `Connection`, not `Conn`; `Configuration`, not `Cfg`; `Manager`, not `Mgr`.
  - **Variable and parameter names** follow standard Go style: single-letter or short receivers (`r` for `*Router`, `c` for `*Context`), idiomatic short names for local scope (`conn`, `fn`, `err`, `ok`, `n`, `i`, `v`), and descriptive names for package-level identifiers.
- **Markdown**: no emojis in documentation files.
- **Git**:
  - Follow the commit message rules in [commit-message-instructions.md](instructions/commit-message-instructions.md).
  - All commit messages in English.
  - Each commit must represent exactly one logical change.
  - Before every commit, run `make check` (runs fmt → lint → test in order).
  - **Branch strategy**: never push directly to `develop` or `main`.
    - `feature/<name>` — new feature
    - `refactor/<name>` — restructure without behaviour change
    - `bugfix/<name>` — bug fix
    - `fix/<name>` — quick fix (e.g. config, docs, CI)
    - CI triggers on all four branch prefixes and on PRs targeting `main`/`develop`. Tags do **not** trigger CI (the tag is created after CI already passed). Open a PR into `develop`; `develop` requires status checks to pass.
- **Tests**: co-located with source (`_test.go`). Cover happy path and at least one error path. Required for new public functions.
  - **Test-first for bug fixes**: **mandatory** — see Critical Rule 7 for the required step-by-step procedure. Do not touch production code without a prior failing test.
  - **Benchmarks**: changes to ring buffer, broadcast fan-out, or frame encoding must include a benchmark. Verify with `make bench`.
- **API compatibility**:
  - Exported symbols are a public contract. Changing or removing any exported identifier is a breaking change requiring a major version bump.
  - Adding a method to an exported interface breaks all external implementations — treat it as a breaking change.
  - Mark deprecated symbols with `// Deprecated: use Xxx instead.` before removal.
- **Error format**: wrap errors as `fmt.Errorf("wspulse: <context>: %w", err)`; define sentinel errors as `errors.New("wspulse: <description>")`.
- **Dependency policy**: prefer stdlib; justify any new external dependency explicitly in the PR description.

## Critical Rules

1. **Read before write** — always read the target file, `doc/protocol.md`, and `doc/internals.md` fully before editing.
2. **Minimal changes** — one concern per edit; no drive-by refactors.
3. **No hardcoded secrets** — all configuration via environment variables.
4. **Hub serialization** — all session state mutations must go through the hub's event loop. Never mutate session state from outside the hub goroutine.
5. **Goroutine lifecycle** — every goroutine launched must have an explicit, documented exit condition. `Close()` must not leak goroutines. Use `go.uber.org/goleak` in `TestMain` to catch leaks during testing.
6. **No breaking changes without version bump** — never rename, remove, or change the signature of an exported symbol without bumping the major version. When unsure, add alongside the old symbol and deprecate.
7. **STOP — test first, fix second** — when a bug is discovered or reported, do NOT touch production code until a failing test exists. Follow this exact sequence without skipping or reordering:
    1. Write a failing test that reproduces the bug.
    2. Run the test and confirm it **fails** (proving the test actually catches the bug).
    3. Fix the production code.
    4. Run the test again and confirm it **passes**.
    5. Run `make check` to verify nothing else broke.
    6. If you are about to edit production code and no failing test exists yet — stop and go back to step 1.
8. **STOP — before every commit, verify this checklist:**
    1. Run `make check` (fmt → lint → test) and confirm it passes. Skip if the commit contains only non-code changes (e.g. documentation, comments, Markdown).
    2. Run GitHub Copilot code review (`github.copilot.chat.review.changes`) on the working-tree diff and resolve every comment before proceeding.
    3. Commit message follows [commit-message-instructions.md](instructions/commit-message-instructions.md): correct type, subject ≤ 50 chars, numbered body items stating reason → change.
    4. This commit contains exactly one logical change — no unrelated modifications.
    5. If any item fails — fix it before committing.
9. **Accuracy** — if you have questions or need clarification, ask the user. Do not make assumptions without confirming.
10. **Language consistency** — when the user writes in Traditional Chinese, respond in Traditional Chinese; otherwise respond in English.
11. **Panic policy — fail early, never at steady-state runtime** — Enforce errors at the earliest possible phase:
    1. Prefer compile-time enforcement via the type system.
    2. **Setup-time programmer errors** (nil handler, empty event name, duplicate registration, invalid option): `panic`. These indicate a caller logic bug; crashing at startup is correct — the process should never start accepting traffic with a misconfigured router or server.
    3. **Steady-state runtime** (`Dispatch`, `Send`, `Close`, reconnect loops, and any code that runs after startup completes): return `error`, never `panic`.

## Session Protocol

> Files under `doc/local/` are git-ignored and must **never** be committed.
> This applies to both plan files and `doc/local/ai-learning.md`.

- **At the start of every session**: check whether `doc/local/plan/` contains
  an in-progress plan for the current task, and read `doc/local/ai-learning.md`
  (if it exists) to recall past mistakes and techniques before writing any code.
- **Plan mode**: when implementing a new feature or multi-file fix, save a plan
  to `doc/local/plan/<feature-name>.md` before starting. Keep it updated with
  completed steps and any plan changes throughout the session.
- **AI learning log**: at the end of a session where mistakes were made or
  reusable techniques were discovered, append a short entry to
  `doc/local/ai-learning.md`. Entry format:
  `Date` / `Issue or Learning` / `Root Cause` / `Prevention Rule`.
  Append only — never overwrite existing entries.
