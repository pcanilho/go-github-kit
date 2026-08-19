.PHONY: test test-unit test-live test-fuzz bench bench-update lint vuln tidy help

# Default target
help:
	@echo "Common targets:"
	@echo "  make test         -- run the full test suite with -race"
	@echo "  make test-unit    -- run short unit tests only"
	@echo "  make test-live    -- run the live probes (needs GITHUB_TOKEN)"
	@echo "  make test-fuzz    -- fuzz the ETag hash and Retry-After parser"
	@echo "  make bench        -- write benchmarks to dist/bench-current.txt"
	@echo "  make bench-update -- prompt to update the benchmark baseline"
	@echo "  make lint         -- golangci-lint run"
	@echo "  make vuln         -- govulncheck on the module"
	@echo "  make tidy         -- go mod tidy with a diff gate"
	@echo ""
	@echo "Releases: lightweight tag + push (see Makefile footer)."

test:
	go test -race ./...

test-unit:
	go test -race -short ./...

test-live:
	@[ -n "$$GITHUB_TOKEN" ] || { echo "GITHUB_TOKEN required"; exit 1; }
	go test -tags=live -run 'TestETag_Live|TestRetry_Live|TestPages_Live|TestPoll_Live' ./etag/... ./retry/... ./pages/... ./polling/...

test-fuzz:
	go test -fuzz=FuzzETag_ComputeExpectedETag -fuzztime=30s ./etag/...
	go test -fuzz=FuzzParseRetryAfter -fuzztime=30s ./retry/...

bench:
	@mkdir -p dist
	go test -bench=. -benchmem -run=^$$ ./... | tee dist/bench-current.txt

bench-update:
	@echo "Review dist/bench-current.txt and copy manually to dist/bench-baseline.txt."

lint:
	golangci-lint run

vuln:
	# Mirrors the CI govulncheck job, which runs golang.org/x/vuln @latest
	# directly. This is the local-dev equivalent; it pulls whatever @latest
	# is today. Pin a specific version here for a deterministic local run.
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

tidy:
	go mod tidy
	@git diff --exit-code go.mod go.sum
