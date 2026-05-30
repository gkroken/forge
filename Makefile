.PHONY: build test test/integration test/all lint coverage clean

build:
	go build -o forge ./cmd/forge

# Unit tests with race detector. No containers required.
test:
	go test -race ./...

# Integration tests — requires a running Docker daemon.
test/integration:
	go test -tags integration -race -timeout 120s ./...

test/conformance:
	go test -tags conformance -timeout 600s -v ./internal/conformance/...

test/all: test test/integration test/conformance

lint:
	go vet ./...
	go mod verify
	helm lint deploy/helm/forge/

# Print coverage summary. Scoped to packages with test files to avoid a
# covdata tool lookup that fails when using the auto-downloaded Go toolchain.
TEST_PKGS := $(shell go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v '^$$')
coverage:
	go test -count=1 -coverprofile=coverage.out -covermode=atomic $(TEST_PKGS)
	go tool cover -func=coverage.out | tail -1

clean:
	rm -f forge coverage.out
