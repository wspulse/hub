# Benchmarks

The table below is a checked-in baseline for `wspulse/hub`, last refreshed
locally by a maintainer running `make bench-sync` on the hardware noted in
the table. The CI workflow runs the same benchmarks on every PR and uploads
the raw `bench.txt` as an artefact; download it from the run page if you
need to compare specific numbers between branches. CI does not regenerate
or commit this file.

Variance between machines is expected — these baselines are a regression
sanity check, not a portability claim. Single runs at `-benchtime=3s -count=1`
are noisy at the few-percent level; a one-off `bench-sync` is enough for
order-of-magnitude regression detection but not micro-optimisation.

The matrix covers:

- `Broadcast` — fan-out cost across `room ∈ {1, 10, 100, 1000}` and
  `messageSize ∈ {64 B, 1 KiB, 16 KiB}`.
- `Send` — direct per-connection enqueue cost across the same payload sizes.
- `Enqueue (drop-oldest)` — backpressure path when the send buffer is full.
- `Resume buffer drain (256 msgs)` — per-cycle cost of staging a full
  256-slot resume buffer (256 `ForcePush` calls) and draining it back into
  the send queue (`RingBuffer.Drain` + 256 `ForceEnqueue`). The bench uses
  the test-only `PrefillResumeBuffer` / `DrainResumeBuffer` hooks so that
  transition-goroutine scheduling, `InjectTransport` routing, and codec
  encode cost are excluded — regressions land directly on the buffer-copy
  operations. Divide ns/op by 256 for amortised per-message cost.

<!-- benchsync:hub:start -->
Measured on `darwin/arm64` (`Apple M1 Max`).

| Operation | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `Broadcast (room=1, 64 B)` | 631.8 | 337 | 4 |
| `Broadcast (room=1, 1 KiB)` | 4,633 | 1,395 | 4 |
| `Broadcast (room=1, 16 KiB)` | 64,090 | 18,686 | 4 |
| `Broadcast (room=10, 64 B)` | 1,020 | 939 | 13 |
| `Broadcast (room=10, 1 KiB)` | 4,580 | 1,988 | 13 |
| `Broadcast (room=10, 16 KiB)` | 66,498 | 19,276 | 13 |
| `Broadcast (room=100, 64 B)` | 10,016 | 7,922 | 120 |
| `Broadcast (room=100, 1 KiB)` | 8,520 | 8,028 | 106 |
| `Broadcast (room=100, 16 KiB)` | 72,170 | 25,141 | 104 |
| `Broadcast (room=1000, 64 B)` | 304,390 | 134,025 | 1,949 |
| `Broadcast (room=1000, 1 KiB)` | 6,001 | 1,779 | 10 |
| `Broadcast (room=1000, 16 KiB)` | 66,870 | 23,705 | 81 |
| `Send (64 B)` | 550.7 | 348 | 4 |
| `Send (1 KiB)` | 4,795 | 4,706 | 36 |
| `Send (16 KiB)` | 73,549 | 61,668 | 94 |
| `Enqueue (drop-oldest)` | 396.8 | 289 | 4 |
| `Resume buffer drain (256 msgs)` | 6,140 | 6,528 | 1 |
<!-- benchsync:hub:end -->
