package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBenchResults_HubNames(t *testing.T) {
	t.Parallel()

	input := []byte(`
goos: darwin
goarch: arm64
cpu: Apple M1 Max
BenchmarkBroadcast/room=1/messageSize=64B-10            6140518       588.2 ns/op      337 B/op       4 allocs/op
BenchmarkBroadcast/room=1000/messageSize=16KiB-10         48624     65118 ns/op    36829 B/op     286 allocs/op
BenchmarkSend/messageSize=1KiB-10                       1000000      1234 ns/op      256 B/op       3 allocs/op
BenchmarkResumeBufferDrain-10                                 3     42070 ns/op    26429 B/op     281 allocs/op
PASS
`)

	got, err := parseBenchResults(input)
	require.NoError(t, err)

	assert.Equal(t, "darwin", got.Env.GOOS)
	assert.Equal(t, "arm64", got.Env.GOARCH)
	assert.Equal(t, "Apple M1 Max", got.Env.CPU)

	// Sub-benchmark names with slashes and equals signs preserve correctly.
	assert.Equal(t, "588.2", got.Results["BenchmarkBroadcast/room=1/messageSize=64B"].NSPerOp)
	assert.Equal(t, "65118", got.Results["BenchmarkBroadcast/room=1000/messageSize=16KiB"].NSPerOp)
	assert.Equal(t, "1234", got.Results["BenchmarkSend/messageSize=1KiB"].NSPerOp)

	// Singleton benchmarks (no sub-name) parse correctly.
	assert.Equal(t, "42070", got.Results["BenchmarkResumeBufferDrain"].NSPerOp)
	assert.Equal(t, "26429", got.Results["BenchmarkResumeBufferDrain"].BPerOp)
	assert.Equal(t, "281", got.Results["BenchmarkResumeBufferDrain"].AllocsPerOp)
}

func TestRun(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))

	benchFile := filepath.Join(root, "bench.txt")
	require.NoError(t, os.WriteFile(benchFile, []byte(`
goos: darwin
goarch: arm64
cpu: Apple M1 Max
BenchmarkBroadcast/room=1/messageSize=64B-10            6140518       588.2 ns/op      337 B/op       4 allocs/op
BenchmarkBroadcast/room=1/messageSize=1KiB-10           5000000       720.0 ns/op      512 B/op       4 allocs/op
BenchmarkBroadcast/room=1/messageSize=16KiB-10          1000000      4500   ns/op    16500 B/op       4 allocs/op
BenchmarkBroadcast/room=10/messageSize=64B-10           4000000       780   ns/op      400 B/op       6 allocs/op
BenchmarkBroadcast/room=10/messageSize=1KiB-10          3000000       950   ns/op      640 B/op       6 allocs/op
BenchmarkBroadcast/room=10/messageSize=16KiB-10          800000      5800   ns/op    16700 B/op       6 allocs/op
BenchmarkBroadcast/room=100/messageSize=64B-10          1000000      2100   ns/op      900 B/op      18 allocs/op
BenchmarkBroadcast/room=100/messageSize=1KiB-10          800000      2700   ns/op     1200 B/op      18 allocs/op
BenchmarkBroadcast/room=100/messageSize=16KiB-10         300000     12000   ns/op    18000 B/op      18 allocs/op
BenchmarkBroadcast/room=1000/messageSize=64B-10          200000     12000   ns/op     6500 B/op     156 allocs/op
BenchmarkBroadcast/room=1000/messageSize=1KiB-10         150000     17000   ns/op     8200 B/op     156 allocs/op
BenchmarkBroadcast/room=1000/messageSize=16KiB-10         48624     65118   ns/op    36829 B/op     286 allocs/op
BenchmarkSend/messageSize=64B-10                        2000000       420   ns/op      120 B/op       3 allocs/op
BenchmarkSend/messageSize=1KiB-10                       1000000      1234   ns/op      256 B/op       3 allocs/op
BenchmarkSend/messageSize=16KiB-10                       500000      3700   ns/op     2200 B/op       3 allocs/op
BenchmarkEnqueue_DropOldest-10                          5000000       310   ns/op       80 B/op       2 allocs/op
BenchmarkResumeBufferDrain-10                                 3     42070   ns/op    26429 B/op     281 allocs/op
`), 0o644))

	docPath := filepath.Join(root, "docs", "bench.md")
	require.NoError(t, os.WriteFile(docPath, []byte(strings.TrimSpace(`
# Benchmarks

<!-- benchsync:hub:start -->
stale
<!-- benchsync:hub:end -->
`)+"\n"), 0o644))

	require.NoError(t, run([]string{"-root", root, "-input", benchFile}))

	got, err := os.ReadFile(docPath)
	require.NoError(t, err)
	doc := string(got)

	assert.Contains(t, doc, "Measured on `darwin/arm64` (`Apple M1 Max`).")
	assert.Contains(t, doc, "| `Broadcast (room=1, 64 B)` | 588.2 | 337 | 4 |")
	assert.Contains(t, doc, "| `Broadcast (room=1000, 16 KiB)` | 65,118 | 36,829 | 286 |")
	assert.Contains(t, doc, "| `Send (1 KiB)` | 1,234 | 256 | 3 |")
	assert.Contains(t, doc, "| `Enqueue (drop-oldest)` | 310 | 80 | 2 |")
	assert.Contains(t, doc, "| `Resume buffer drain (256 msgs)` | 42,070 | 26,429 | 281 |")
}

func TestRun_MissingResult(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))

	// Bench output is missing several rows the target expects.
	benchFile := filepath.Join(root, "bench.txt")
	require.NoError(t, os.WriteFile(benchFile, []byte(`
BenchmarkBroadcast/room=1/messageSize=64B-10  100 100 ns/op 0 B/op 0 allocs/op
`), 0o644))

	docPath := filepath.Join(root, "docs", "bench.md")
	require.NoError(t, os.WriteFile(docPath, []byte("<!-- benchsync:hub:start -->\n<!-- benchsync:hub:end -->\n"), 0o644))

	err := run([]string{"-root", root, "-input", benchFile})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing benchmark result")
}

func TestFormatMetric(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "107", formatMetric("107.0"))
	assert.Equal(t, "5.59", formatMetric("5.590"))
	assert.Equal(t, "1,626", formatMetric("1626"))
	assert.Equal(t, "3.007", formatMetric("3.007"))
	assert.Equal(t, "65,118", formatMetric("65118"))
}
