.PHONY: help test test-cover bench bench-ci bench-sync lint fmt check tidy deps clean

# bench-ci and bench-sync use bash for `set -o pipefail` and `trap`; all
# other recipes stay POSIX-compatible so they run in any /bin/sh.
bench-ci bench-sync: SHELL := bash

# Default target
help: ## Show available commands
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

test: ## Run all tests (unit + component) with race detector
	@go test -race -count=$${TEST_COUNT:-50} ./...

test-cover: ## Run all tests with coverage report
	@go test -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

bench: ## Run benchmarks with memory allocation stats
	@go test -bench=. -benchmem -run=^$$ ./...

bench-ci: ## Run the CI benchmark suite and write results to BENCH_OUT or bench.txt
	@set -o pipefail; \
		outfile="$${BENCH_OUT:-bench.txt}"; \
		go test -bench=. -benchmem -benchtime=3s -count=1 -run=^$$ ./... | tee "$$outfile"

bench-sync: ## Refresh docs/bench.md from a fresh local benchmark run
	@echo "── running benchmarks ──"
	@set -e; \
		tmpfile="$$(mktemp -t wspulse-benchsync.XXXXXX)"; \
		trap 'rm -f "$$tmpfile"' EXIT; \
		$(MAKE) --no-print-directory bench-ci BENCH_OUT="$$tmpfile"; \
		echo "── refreshing docs/bench.md ──"; \
		go run ./cmd/benchsync -input "$$tmpfile"; \
		echo "── done ──"

lint: ## Run go vet and golangci-lint
	@go vet ./...
	@golangci-lint run ./...

fmt: ## Format source files
	@gofmt -w .
	@go run golang.org/x/tools/cmd/goimports@latest -local github.com/wspulse -w .

check: ## Run fmt-check, lint, all tests
	@echo "── fmt ──"
	@test -z "$$(gofmt -l .)" || (echo "formatting issues — run 'make fmt'"; exit 1)
	@test -z "$$(go run golang.org/x/tools/cmd/goimports@latest -local github.com/wspulse -l .)" || (echo "import issues — run 'make fmt'"; exit 1)
	@echo "── lint ──"
	@$(MAKE) --no-print-directory lint
	@echo "── test ──"
	@$(MAKE) --no-print-directory test
	@echo "── all passed ──"

tidy: ## Tidy module dependencies
	@GOWORK=off go mod tidy

deps: ## Download all modules and sync go.sum, then tidy
	@go mod download
	@GOWORK=off go mod tidy

clean: ## Remove build artifacts and test cache
	@rm -f coverage.out coverage.html
