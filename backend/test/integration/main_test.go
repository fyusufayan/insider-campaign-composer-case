//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// pg is the PostgreSQL container shared by every test in this package; each
// test gets its own database from it.
var pg *testsupport.Postgres

func TestMain(m *testing.M) {
	var err error
	pg, err = testsupport.StartPostgres(context.Background())
	if err != nil {
		// Fail loudly: integration tests are never skipped silently.
		fmt.Fprintf(os.Stderr, "FAIL: integration tests require Docker: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := pg.Terminate(); err != nil {
		fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
	}
	cleanupSeedBinary()
	os.Exit(code)
}
