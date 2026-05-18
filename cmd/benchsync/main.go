// Command benchsync regenerates the benchmark tables in docs from a fresh
// `go test -bench` output file. It parses the standard Go benchmark format,
// emits one markdown row per known benchmark name, and replaces the table
// inside a managed marker block in the target document.
//
// Run via `make bench-sync` from the repo root.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var benchLinePattern = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+(\S+)\s+ns/op\s+(\S+)\s+B/op\s+(\S+)\s+allocs/op$`)

type benchEnv struct {
	GOOS   string
	GOARCH string
	CPU    string
}

type benchReport struct {
	Env     benchEnv
	Results map[string]benchResult
}

type benchResult struct {
	NSPerOp     string
	BPerOp      string
	AllocsPerOp string
}

type benchRow struct {
	Name  string
	Label string
}

type benchTarget struct {
	Path       string
	MarkerName string
	Rows       []benchRow
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("benchsync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	root := fs.String("root", ".", "repository root")
	inputPath := fs.String("input", "", "path to benchmark output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *inputPath == "" {
		return fmt.Errorf("input benchmark file is required")
	}

	input, err := os.ReadFile(*inputPath)
	if err != nil {
		return fmt.Errorf("read benchmark input: %w", err)
	}

	report, err := parseBenchResults(input)
	if err != nil {
		return err
	}

	for _, target := range defaultTargets(*root) {
		markdown, err := os.ReadFile(target.Path)
		if err != nil {
			return fmt.Errorf("read markdown %s: %w", target.Path, err)
		}

		table, err := renderBenchTable(target.Rows, report)
		if err != nil {
			return err
		}

		updated, err := replaceManagedBlock(markdown, target.MarkerName, table)
		if err != nil {
			return fmt.Errorf("update %s: %w", target.Path, err)
		}

		if err := os.WriteFile(target.Path, updated, 0o644); err != nil {
			return fmt.Errorf("write markdown %s: %w", target.Path, err)
		}
	}

	return nil
}

// defaultTargets enumerates the benchmark tables maintained in docs.
// Each row maps a Go benchmark name (with sub-benchmark path, no -GOMAXPROCS
// suffix) to its display label. Add new rows here when adding new benchmark
// cases to keep docs/bench.md in sync.
func defaultTargets(root string) []benchTarget {
	var broadcastRows []benchRow
	for _, room := range []int{1, 10, 100, 1000} {
		for _, ms := range []struct {
			label string
			tag   string
		}{
			{"64 B", "64B"},
			{"1 KiB", "1KiB"},
			{"16 KiB", "16KiB"},
		} {
			broadcastRows = append(broadcastRows, benchRow{
				Name:  fmt.Sprintf("BenchmarkBroadcast/room=%d/messageSize=%s", room, ms.tag),
				Label: fmt.Sprintf("Broadcast (room=%d, %s)", room, ms.label),
			})
		}
	}

	rows := append([]benchRow{}, broadcastRows...)
	rows = append(rows,
		benchRow{Name: "BenchmarkSend/messageSize=64B", Label: "Send (64 B)"},
		benchRow{Name: "BenchmarkSend/messageSize=1KiB", Label: "Send (1 KiB)"},
		benchRow{Name: "BenchmarkSend/messageSize=16KiB", Label: "Send (16 KiB)"},
		benchRow{Name: "BenchmarkEnqueue_DropOldest", Label: "Enqueue (drop-oldest)"},
		benchRow{Name: "BenchmarkResumeBufferDrain", Label: "Resume buffer drain (256 msgs)"},
	)

	return []benchTarget{
		{
			Path:       filepath.Join(root, "docs", "bench.md"),
			MarkerName: "benchsync:hub",
			Rows:       rows,
		},
	}
}

func parseBenchResults(input []byte) (benchReport, error) {
	report := benchReport{
		Results: make(map[string]benchResult),
	}

	scanner := bufio.NewScanner(bytes.NewReader(input))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "goos: "):
			report.Env.GOOS = strings.TrimSpace(strings.TrimPrefix(line, "goos: "))
			continue
		case strings.HasPrefix(line, "goarch: "):
			report.Env.GOARCH = strings.TrimSpace(strings.TrimPrefix(line, "goarch: "))
			continue
		case strings.HasPrefix(line, "cpu: "):
			report.Env.CPU = strings.TrimSpace(strings.TrimPrefix(line, "cpu: "))
			continue
		}

		matches := benchLinePattern.FindStringSubmatch(line)
		if len(matches) == 0 {
			continue
		}

		report.Results[matches[1]] = benchResult{
			NSPerOp:     matches[2],
			BPerOp:      matches[3],
			AllocsPerOp: matches[4],
		}
	}
	if err := scanner.Err(); err != nil {
		return benchReport{}, fmt.Errorf("scan benchmark output: %w", err)
	}
	return report, nil
}

func renderBenchTable(rows []benchRow, report benchReport) (string, error) {
	var out strings.Builder

	if measurementLine := formatMeasurementLine(report.Env); measurementLine != "" {
		out.WriteString(measurementLine)
		out.WriteString("\n\n")
	}

	out.WriteString("| Operation | ns/op | B/op | allocs/op |\n")
	out.WriteString("|---|---:|---:|---:|\n")

	for _, row := range rows {
		result, ok := report.Results[row.Name]
		if !ok {
			return "", fmt.Errorf("missing benchmark result for %s", row.Name)
		}

		if _, err := fmt.Fprintf(&out, "| `%s` | %s | %s | %s |\n",
			row.Label,
			formatMetric(result.NSPerOp),
			formatMetric(result.BPerOp),
			formatMetric(result.AllocsPerOp),
		); err != nil {
			return "", fmt.Errorf("render benchmark row %s: %w", row.Name, err)
		}
	}

	return strings.TrimRight(out.String(), "\n"), nil
}

func formatMeasurementLine(env benchEnv) string {
	platform := strings.Trim(strings.TrimSpace(env.GOOS)+"/"+strings.TrimSpace(env.GOARCH), "/")
	switch {
	case platform != "" && env.CPU != "":
		return fmt.Sprintf("Measured on `%s` (`%s`).", platform, env.CPU)
	case platform != "":
		return fmt.Sprintf("Measured on `%s`.", platform)
	case env.CPU != "":
		return fmt.Sprintf("Measured on `%s`.", env.CPU)
	default:
		return ""
	}
}

func replaceManagedBlock(markdown []byte, marker, content string) ([]byte, error) {
	startMarker := "<!-- " + marker + ":start -->"
	endMarker := "<!-- " + marker + ":end -->"

	start := bytes.Index(markdown, []byte(startMarker))
	if start == -1 {
		return nil, fmt.Errorf("start marker not found: %s", marker)
	}

	end := bytes.Index(markdown[start:], []byte(endMarker))
	if end == -1 {
		return nil, fmt.Errorf("end marker not found: %s", marker)
	}
	end += start

	var out bytes.Buffer
	out.Write(markdown[:start+len(startMarker)])
	out.WriteString("\n")
	out.WriteString(content)
	out.WriteString("\n")
	out.Write(markdown[end:])
	return out.Bytes(), nil
}

func formatMetric(value string) string {
	if strings.Contains(value, ".") {
		return strings.TrimRight(strings.TrimRight(value, "0"), ".")
	}

	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return value
	}

	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}

	raw := strconv.FormatInt(n, 10)
	if len(raw) <= 3 {
		return sign + raw
	}

	var parts []string
	for len(raw) > 3 {
		parts = append([]string{raw[len(raw)-3:]}, parts...)
		raw = raw[:len(raw)-3]
	}
	parts = append([]string{raw}, parts...)
	return sign + strings.Join(parts, ",")
}
