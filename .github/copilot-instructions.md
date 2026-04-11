# Copilot Instructions — wspulse/hub

## Project Overview

wspulse/hub is a **minimal, production-ready WebSocket server library** for Go. It manages concurrent connections, room-based broadcasting, session resumption, and heartbeat. Module path: `github.com/wspulse/hub`. Package name: `wspulse`. Depends on `github.com/wspulse/core` for shared types (`Frame`, `Codec`).

## Architecture

- **`server.go`** — `Hub` interface (public API: `Send`, `Broadcast`, `Kick`, `GetConnections`, `Close`) and `NewHub` constructor. Implements `http.Handler`.
- **`heart.go`** — Central single-threaded event loop. Manages all sessions and routes messages via channels. No `net/http` imports.
- **`session.go`** — `Connection` interface and the unexported `session` struct. Per-connection `readPump` + `writePump` goroutine pair; ping/pong heartbeat; backpressure drop.
- **`options.go`** — `HubOption` functional options, `ConnectFunc` type, and all `WithXxx` option builders.
- **`resume.go`** — Ring buffer for buffering frames during temporary disconnects (session resumption).
- **`errors.go`** — Hub-only sentinel errors: `ErrConnectionNotFound`, `ErrDuplicateConnectionID`, `ErrHubClosed`.
- **`doc/internals.md`** — Internal architecture documentation.
- Wire protocol specification has moved to the [`.github` repo](https://github.com/wspulse/.github/blob/main/doc/protocol.md).

## Development Workflow

```bash
make fmt              # format (gofmt + goimports)
make lint             # vet + golangci-lint
make test             # all tests (unit + component) with race detector
make check            # fmt + lint + test (pre-commit gate)
make bench            # benchmarks with memory stats
make test-cover       # all tests with coverage report → coverage.html
make tidy             # tidy module dependencies
```

## Conventions

- **Go style**: `gofmt`/`goimports`, snake_case filenames, GoDoc on all public symbols, `if err != nil` error handling (no runtime panics; setup-time programmer-error panics are allowed — see **Panic policy** critical rule), secrets from env vars only. Package name is `wspulse` (not `server`) — import path `github.com/wspulse/hub` resolves as `wspulse.X` without an alias.
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
    - `feat/<name>` or `feature/<name>` — new feature
    - `refactor/<name>` — restructure without behaviour change
    - `bugfix/<name>` — bug fix
    - `fix/<name>` — quick fix (e.g. config, docs, CI)
    - `chore/<name>` — maintenance, CI/CD, dependencies, docs
    - CI triggers on all branch prefixes above and on PRs targeting `main`/`develop`. Tags do **not** trigger CI (the tag is created after CI already passed). Open a PR into `develop`; `develop` requires status checks to pass.
  - **Pull request description**: must follow the repo's `.github/PULL_REQUEST_TEMPLATE.md`. Fill in every section (Summary, Changes, Checklist). Do not invent custom formats.
- **Tests**: co-located with source (`_test.go`). Cover happy path and at least one error path. Required for new public functions.
  - **Component tests**: tests use mock transports (`mockTransport`) via `InjectTransport` — no real WebSocket connections needed. All tests run with `make test`.
  - **Test-first for bug fixes**: **mandatory** — see Critical Rule 7 for the required step-by-step procedure. Do not touch production code without a prior failing test.
  - **Benchmarks**: changes to ring buffer, broadcast fan-out, or frame encoding must include a benchmark. Verify with `make bench`.
- **API compatibility**:
  - Exported symbols are a public contract. Changing or removing any exported identifier is a breaking change requiring a major version bump.
  - Adding a method to an exported interface breaks all external implementations — treat it as a breaking change.
  - Mark deprecated symbols with `// Deprecated: use Xxx instead.` before removal.
- **Error format**: wrap errors as `fmt.Errorf("wspulse: <context>: %w", err)`; define sentinel errors as `errors.New("wspulse: <description>")`.
- **Dependency policy**: prefer stdlib; justify any new external dependency explicitly in the PR description.
- **File encoding**: all files must be UTF-8 without BOM. Do not use any other encoding.

## Feature Workflow

All new features and design changes follow this process — do not skip steps:

1. **Plan** — write idea to `doc/local/plan/<name>.md` (local only, git-ignored)
2. **Quick discussion** — feasibility + value check
3. **Go / No-go** — kill or proceed
4. **Layer check** — transport layer (wspulse implements) or application layer (write docs recipe instead)
5. **Issue** — repo-scoped work: open issue on this repo. Cross-repo/global work: open issue on [`wspulse/.github`](https://github.com/wspulse/.github). Include summary, scope, impact assessment, priority label + milestone
6. **Design discussion** — API surface, cross-SDK parity, contract/protocol updates, edge cases
7. **Task** — feature branch from `develop`, implement with tests, CHANGELOG entry, PR following template. **Repo-scoped**: link PR to the issue. **Global**: each PR mentions the global issue (e.g., `wspulse/.github#N`); after opening a PR, comment on the global issue with the PR link

## Release Workflow

Releases follow a branch-based process. A CI changelog-gate enforces CHANGELOG hygiene before merge to `main`.

1. **Feature work** lands on `develop` via PRs (each PR must include a CHANGELOG entry under `[Unreleased]`).
2. **Prepare** — create a `chore/release-vX.Y.Z` branch from `develop`:
   ```bash
   git checkout develop && git pull
   git checkout -b chore/release-vX.Y.Z
   ```
3. **Update CHANGELOG** — promote `[Unreleased]` to a concrete version and date. Remove the empty `## [Unreleased]` section entirely — the CI gate rejects it:
   ```markdown
   ## [X.Y.Z] - YYYY-MM-DD
   ```
   Also add the comparison link at the bottom:
   ```markdown
   [X.Y.Z]: https://github.com/wspulse/<repo>/compare/vPREV...vX.Y.Z
   ```
4. **Open PR** — `chore/release-vX.Y.Z` → `main`. The CI changelog-gate checks that the top `## [` entry in CHANGELOG.md is not `[Unreleased]`.
5. **After merge** — tag the release on `main`:
   ```bash
   git checkout main && git pull
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```
6. **Sync develop** — merge `main` back into `develop` to pick up the release commit.

## Critical Rules

1. **Read before write** — always read the target file and `doc/internals.md` fully before editing. For wire protocol details see the [centralized protocol spec](https://github.com/wspulse/.github/blob/main/doc/protocol.md).
2. **Minimal changes** — one concern per edit; no drive-by refactors.
3. **No hardcoded secrets** — all configuration via environment variables.
4. **Heart serialization** — all session state mutations must go through the heart's event loop. Never mutate session state from outside the heart goroutine.
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
    2. Commit message follows [commit-message-instructions.md](instructions/commit-message-instructions.md): correct type, subject ≤ 50 chars, numbered body items stating reason → change.
    3. This commit contains exactly one logical change — no unrelated modifications.
    4. If any item fails — fix it before committing.
9. **Accuracy** — if you have questions or need clarification, ask the user. Do not make assumptions without confirming.
10. **Language consistency** — when the user writes in Traditional Chinese, respond in Traditional Chinese; otherwise respond in English.
11. **Panic policy — fail early, never at steady-state runtime** — Enforce errors at the earliest possible phase:
    1. Prefer compile-time enforcement via the type system.
    2. **Setup-time programmer errors** (nil handler, empty event name, duplicate registration, invalid option): `panic`. These indicate a caller logic bug; crashing at startup is correct — the process should never start accepting traffic with a misconfigured router or server.
    3. **Steady-state runtime** (`Dispatch`, `Send`, `Close`, reconnect loops, and any code that runs after startup completes): return `error`, never `panic`.

## PR Comment Review — MANDATORY

When handling PR review comments, **every unresponded comment must be analyzed and responded to**. No comment may be silently ignored.

### 1. Fetch unresponded comments

Pull all comments that have not received a reply from the PR author. Bot-generated summaries (e.g. Copilot review overview) may be skipped; individual line comments from bots must still be evaluated.

### 2. Analyze each comment

Evaluate against:

| Criterion | Question |
|-----------|----------|
| **Validity** | Is the observation correct? Is the suggestion reasonable? |
| **Severity** | Is it a bug, a correctness issue, a design concern, or a style/preference nitpick? |
| **Cost** | What is the effort to address? Does the change introduce risk or scope creep? |

### 3. Present analysis for approval

Present all findings to the user before taking action. For each comment, show:
- The comment content and location
- Your assessment (validity, severity, cost)
- Your proposed decision (Fixed / Tracked / Won't fix / Not applicable) with reasoning

**Do not make any code changes or reply to comments until the user has reviewed and approved.** If there are disagreements, discuss until a consensus is reached.

### 4. Execute approved decisions

After approval, carry out each decision and respond on the PR:

- **`Fixed in {hash}. {what changed and why}`** — adopt and fix immediately. Bug and correctness issues must use this path unless the fix requires a separate PR due to scope.
- **`Tracked in TODOS.md — {reason for deferring}`** — adopt but defer. Add entry to repo root `TODOS.md` with context and PR comment link.
- **`Won't fix. {clear reasoning}`** — reject the suggestion with explanation.
- **`Not applicable — {explanation}`** — the comment does not apply (already handled, misunderstanding, duplicate, or already tracked in TODOS.md).

Duplicate or related comments may reference each other: `Same reasoning as {reference} above — {brief}`.

### 5. Zero unresponded comments before merge

The PR must have zero unaddressed comments before merge. This is a hard gate.

## Session Protocol

> Files under `doc/local/` are git-ignored and must **never** be committed.
> This includes plan files (`doc/local/plan/`), review records, and the AI learning log (`doc/local/ai-learning.md`).

### Start of every session — MANDATORY

**Do these steps before writing any code:**

1. Read `doc/local/ai-learning.md` **in full** to recall past mistakes. If the file is missing or empty, create it with the table header (see format below) before proceeding.
2. Check `doc/local/plan/` for any in-progress plan and read it fully.

### During feature work — doc before code

Before writing any production code, create or update `doc/local/plan/<feature-name>.md` with:

1. **What** — what are you changing or adding?
2. **Why** — what problem does it solve? What motivated this change?
3. **How** — what is the intended approach?

Keep it updated as the approach evolves. This is the primary cross-session context for understanding what was done and why.

For bug fixes, the failing test serves as the "what"; add a brief "why" and "how" to the plan file or `doc/local/ai-learning.md`.

### Review records

After conducting any review (code review, plan review, design review, PR review, etc.), record the findings for cross-session context:

- **Where to write**: this repo's `doc/local/`. If working in a multi-module workspace, also write to the workspace root's `doc/local/`.
- **Single truth**: write the full record in one location; the other location keeps a brief summary with a file path reference to the full record.
- **Acceptable formats**:
  1. Update the relevant plan file in `doc/local/plan/` with the review outcome.
  2. Dedicated review file in `doc/local/` if no relevant plan exists.
- **What to record**: review type, key findings, decisions made, action items, and resolution status.

### End of every session — MANDATORY

**Before closing the session, complete this checklist without exception:**

1. Append at least one entry to `doc/local/ai-learning.md` — **even if no mistakes were made**. Record what you confirmed, what technique worked, or what you observed. An empty file is a sign of non-compliance.
2. Update any in-progress plan in `doc/local/plan/` to reflect completed steps.
3. Verify `make check` passes in every module you edited.

**Entry format** for `doc/local/ai-learning.md`:

```
| Date       | Issue or Learning | Root Cause | Prevention Rule |
| ---------- | ----------------- | ---------- | --------------- |
| YYYY-MM-DD | <what happened or what you learned> | <why it happened> | <how to avoid it next time> |
```

**Writing to `ai-learning.md` is not optional. It is the primary cross-session improvement mechanism. An empty file proves the session protocol was ignored.**
