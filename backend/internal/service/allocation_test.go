package service

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AllocsPerRun counts allocations for the entire process. Run the measurement
// in its own test process so other tests' background work cannot affect it.
func runIsolatedAllocationTest(t *testing.T, measure func()) {
	t.Helper()
	const childEnv = "SUB2API_ALLOCATION_TEST_CHILD"
	if os.Getenv(childEnv) == t.Name() {
		measure()
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "isolated allocation test failed:\n%s", output)
}
