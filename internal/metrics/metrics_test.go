package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ozimakov/kohen/internal/metrics"
)

func TestSetConfigVersionBoundsCardinality(t *testing.T) {
	metrics.ClearConfigSync("ns", "cs")
	t.Cleanup(func() { metrics.ClearConfigSync("ns", "cs") })

	metrics.SetConfigVersion("ns", "cs", "git:aaaa")
	if got := testutil.CollectAndCount(metrics.ConfigVersionInfo); got != 1 {
		t.Fatalf("expected 1 version series, got %d", got)
	}

	// Updating the version must replace, not accumulate, the series.
	metrics.SetConfigVersion("ns", "cs", "git:bbbb")
	if got := testutil.CollectAndCount(metrics.ConfigVersionInfo); got != 1 {
		t.Fatalf("expected version series to be replaced (1), got %d", got)
	}

	metrics.ClearConfigSync("ns", "cs")
	if got := testutil.CollectAndCount(metrics.ConfigVersionInfo); got != 0 {
		t.Fatalf("expected series cleared on delete, got %d", got)
	}
}
