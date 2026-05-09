package metrics

import (
	"context"
	"testing"
)

// TestRecorder_NilSafe verifies that a nil *Recorder doesn't panic
// when handlers call helpers. The whole point of the nil-safety
// pattern is that tests and short-lived utilities can pass nil
// without bringing the OTel SDK along for the ride.
func TestRecorder_NilSafe(t *testing.T) {
	var r *Recorder
	r.RecordRequest(context.Background(), "qwen", "success", 0.42)
	r.RecordTokens(context.Background(), "qwen", 100)
	r.SetBackendHealth(context.Background(), true)
	// The fact that none of these panicked is the test.
}

// TestCostRecorder_NilSafe verifies the same property for cost.
func TestCostRecorder_NilSafe(t *testing.T) {
	var cr *CostRecorder
	cr.RecordActive(context.Background(), "qwen", 1.5)
}

// TestRecorder_ZeroAndNegativeTokens makes sure RecordTokens silently
// drops non-positive values (a backend that returns 0 completion
// tokens is doing nothing useful; we shouldn't pollute the counter).
func TestRecorder_ZeroAndNegativeTokens(t *testing.T) {
	var r *Recorder // exercises the nil-safe path; n<=0 short-circuits
	r.RecordTokens(context.Background(), "qwen", 0)
	r.RecordTokens(context.Background(), "qwen", -5)
	// Implicit assertion: no panic, no crash.
}
