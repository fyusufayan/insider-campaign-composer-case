//go:build integration

// Package integration holds tests that need real PostgreSQL (testcontainers) and,
// for delivery, the real worker and mock provider. Every file in this tree carries
// the "integration" build tag so that `go test ./...` never compiles it and never
// requires Docker. Run with `make test-integration`.
package integration
