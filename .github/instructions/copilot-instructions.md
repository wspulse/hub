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
# Run all tests with race detector
go test -race -count=3 ./...

# Vet
go vet ./...

# Lint (requires golangci-lint)
golangci-lint run ./...

# Format
goimports -w .

# Benchmarks
go test -bench=. -benchmem -run=^$ ./...
```

## Conventions

- **Go style**: `gofmt`/`goimports`, snake_case filenames, GoDoc on all public symbols, `if err != nil` error handling (never `panic`), secrets from env vars only.
- **Naming — readability is the highest priority**:
  - Use full words for all identifiers. Code is AI-generated; there is no excuse for cryptic names.
  - **Allowed abbreviations** (universally recognized only): ID, URL, HTTP, API, JSON, Msg, Err, Ctx, Buf, Cfg, Fn, Opt, Req, Resp, Src, Dst, Addr, Auth, Init, Exec, Cmd, Env, Pkg, Fmt, Doc, Spec, Sync, Async, Max, Min, Len, Cap, Idx, Tmp, Ref, Val, Str, Int, Bool, Impl, Repo.
  - **Banned** — half-word truncations that harm readability: `sess`, `conn`, `svc`, `mgr`, `recv`, `svr`, `tbl`, `hdlr`, `dlg`, `desc`, `proc`, `coll`.
  - When in doubt, spell out the full word.
- **Markdown**: no emojis in documentation files.
- **Git**:
  - Follow the commit message rules in [commit-message-instructions.md](commit-message-instructions.md).
  - All commit messages in English.
  - Each commit must represent exactly one logical change.
  - Before every commit, run in order:
    1. `goimports -w .` — fix imports and formatting
    2. `golangci-lint run ./...` — must pass with zero warnings
    3. `go test -race ./...` — must pass
- **Tests**: co-located with source (`_test.go`). Cover happy path and at least one error path. Required for new public functions.
  - **Test-first for bug fixes**: when a bug is discovered, write a failing test that reproduces it before touching production code. The PR must include this test.
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
7. **Accuracy** — if you have questions or need clarification, ask the user. Do not make assumptions without confirming.
8. **Language consistency** — when the user writes in Traditional Chinese, respond in Traditional Chinese; otherwise respond in English.
