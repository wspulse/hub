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
- `Resume buffer drain (256 msgs)` — heart-side cost of replaying a full
  256-slot resume buffer into the send queue at reconnect time.

<!-- benchsync:hub:start -->
Measured on `darwin/arm64` (`Apple M1 Max`).

| Operation | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `Broadcast (room=1, 64 B)` | 590.1 | 337 | 4 |
| `Broadcast (room=1, 1 KiB)` | 4,456 | 1,394 | 4 |
| `Broadcast (room=1, 16 KiB)` | 62,792 | 18,686 | 4 |
| `Broadcast (room=10, 64 B)` | 1,003 | 944 | 13 |
| `Broadcast (room=10, 1 KiB)` | 4,579 | 1,989 | 13 |
| `Broadcast (room=10, 16 KiB)` | 63,093 | 19,274 | 13 |
| `Broadcast (room=100, 64 B)` | 9,384 | 7,842 | 119 |
| `Broadcast (room=100, 1 KiB)` | 6,937 | 7,352 | 96 |
| `Broadcast (room=100, 16 KiB)` | 63,211 | 25,167 | 105 |
| `Broadcast (room=1000, 64 B)` | 136,610 | 99,925 | 1,491 |
| `Broadcast (room=1000, 1 KiB)` | 4,819 | 1,591 | 7 |
| `Broadcast (room=1000, 16 KiB)` | 65,607 | 23,086 | 72 |
| `Send (64 B)` | 521.7 | 353 | 5 |
| `Send (1 KiB)` | 4,648 | 4,779 | 37 |
| `Send (16 KiB)` | 74,246 | 61,737 | 96 |
| `Enqueue (drop-oldest)` | 412.2 | 289 | 4 |
| `Resume buffer drain (256 msgs)` | 28,747 | 32,343 | 372 |
<!-- benchsync:hub:end -->
