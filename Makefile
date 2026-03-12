.PHONY: help test test-cover bench lint fmt check tidy deps clean

# Default target
help: ## Show available commands
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

test: ## Run all tests with race detector
	@go test -race -count=3 ./...

test-cover: ## Run tests with coverage report
	@go test -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

bench: ## Run benchmarks with memory allocation stats
	@go test -bench=. -benchmem -run=^$ ./...

lint: ## Run go vet and golangci-lint
	@go vet ./...
	@golangci-lint run ./...

fmt: ## Format source files
	@gofmt -w .
	@goimports -w . 2>/dev/null || true

check: ## Run fmt, lint, and test (pre-commit gate)
	@echo "── fmt ──"
	@$(MAKE) --no-print-directory fmt
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
