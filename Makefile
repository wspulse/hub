.PHONY: help test test-integration test-cover bench lint fmt check tidy deps clean

# Default target
help: ## Show available commands
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

test: ## Run unit tests with race detector
	@go test -race -count=3 ./...

test-integration: ## Run all tests (unit + integration) with race detector
	@go test -race -count=1 -tags integration ./...

test-cover: ## Run all tests with coverage report
	@go test -race -tags integration -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

bench: ## Run benchmarks with memory allocation stats
	@go test -bench=. -benchmem -run=^$ ./...

lint: ## Run go vet and golangci-lint
	@go vet ./...
	@golangci-lint run ./...

fmt: ## Format source files
	@gofmt -w .
	@go run golang.org/x/tools/cmd/goimports@latest -local github.com/wspulse -w .

check: ## Run fmt-check, lint, unit tests (set INCLUDE_INTEGRATION=1 to also run integration tests)
	@echo "── fmt ──"
	@test -z "$$(gofmt -l .)" || (echo "formatting issues — run 'make fmt'"; exit 1)
	@test -z "$$(go run golang.org/x/tools/cmd/goimports@latest -local github.com/wspulse -l .)" || (echo "import issues — run 'make fmt'"; exit 1)
	@echo "── lint ──"
	@$(MAKE) --no-print-directory lint
	@if [ "$$INCLUDE_INTEGRATION" = "1" ]; then \
		echo "── test-integration (unit + integration) ──"; \
		$(MAKE) --no-print-directory test-integration; \
	else \
		echo "── test ──"; \
		$(MAKE) --no-print-directory test; \
		echo "── test-integration skipped (set INCLUDE_INTEGRATION=1 to enable) ──"; \
	fi
	@echo "── all passed ──"

tidy: ## Tidy module dependencies
	@GOWORK=off go mod tidy

deps: ## Download all modules and sync go.sum, then tidy
	@go mod download
	@GOWORK=off go mod tidy

clean: ## Remove build artifacts and test cache
	@rm -f coverage.out coverage.html
