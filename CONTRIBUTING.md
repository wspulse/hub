# Contributing to wspulse/server

Thank you for your interest in contributing. This document describes the process and conventions expected for all contributions.

## Before You Start

- Open an issue to discuss significant changes before starting work.
- For bug fixes, write a failing test that reproduces the issue before modifying production code. The PR must include this test.
- For new features, confirm scope and API design in an issue first.
- Read `doc/internals.md` before writing any code. For the wire protocol spec see the [centralized protocol doc](https://github.com/wspulse/.github/blob/main/doc/protocol.md).

## Development Setup

```bash
git clone https://github.com/wspulse/server
cd server
# Clone core alongside server (required for local replace directive)
git clone https://github.com/wspulse/core ../core
go mod tidy
```

Requires: Go 1.26+, [golangci-lint](https://golangci-lint.run/), [goimports](https://pkg.go.dev/golang.org/x/tools/cmd/goimports).

## Pre-Commit Checklist

Run `make check` before every commit. It runs in order:

1. `make fmt` — formats all source files
2. `make lint` — runs `go vet` and `golangci-lint`; must pass with zero warnings
3. `make test` — runs tests with `-race`; must pass

If any step fails, do not commit.

## Commit Messages

Follow the format in [`.github/instructions/commit-message-instructions.md`](.github/instructions/commit-message-instructions.md):

```
<type>: <subject>

1.<reason> → <change>
```

All commit messages must be in English.

## Naming Conventions

- Use full words for all identifiers. Cryptic abbreviations are not acceptable.
- Banned: `sess`, `conn`, `svc`, `mgr`, `recv`, `svr`, `tbl`, `hdlr`, `dlg`, `desc`, `proc`, `coll`.
- Allowed short forms: ID, URL, HTTP, API, JSON, Msg, Err, Ctx, Buf, Cfg, and others listed in the copilot instructions.

## Architecture Constraints

**Heart serialization is critical.** All session state mutations must go through the heart's event loop. Never mutate session state from outside the heart goroutine. PRs that violate this will be rejected regardless of test coverage.

**Goroutine lifecycle.** Every goroutine started must have a documented, explicit exit condition. `Close()` must not leak goroutines. Use `go.uber.org/goleak` to verify — it is integrated in `TestMain`.

## API Compatibility

wspulse/server follows semantic versioning. Any change that removes, renames, or alters the signature of an exported symbol is a **breaking change** and requires a major version bump.

- Before removing a symbol, mark it `// Deprecated: use Xxx instead.` in a minor release.
- Adding a method to an exported interface is also a breaking change.
- When in doubt, add a new symbol alongside the old one.

## Performance-Sensitive Changes

Changes to ring buffer, broadcast fan-out, or frame encoding must include a benchmark. Run `make bench` and include before/after numbers in the PR description.

## Pull Request Guidelines

- One PR per logical change.
- Do not reformat code unrelated to your change — it creates noise in the diff.
- All CI checks must pass before review.
- Describe what changed and why, not just what the diff shows.
