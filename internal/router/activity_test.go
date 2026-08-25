package router

import (
	"testing"
	"time"
)

// A replica that has never completed anything must report "unknown" rather
// than a large duration. The health checker treats unknown as no evidence and
// falls through to its probes; a duration would look like a replica that had
// gone quiet, which is a different claim.
func TestLastCompletionUnknownUntilFirstCompletion(t *testing.T) {
	var l lastCompletion
	if _, ok := l.since("d1", "r1", time.Now()); ok {
		t.Fatal("a replica with no completions should report unknown")
	}
	l.mark("d1", "r1", time.Now())
	if _, ok := l.since("d1", "r1", time.Now()); !ok {
		t.Fatal("after a completion the replica should be known")
	}
}

func TestLastCompletionAgesFromTheMark(t *testing.T) {
	var l lastCompletion
	now := time.Now()
	l.mark("d1", "r1", now.Add(-90*time.Second))

	since, ok := l.since("d1", "r1", now)
	if !ok {
		t.Fatal("expected the replica to be known")
	}
	if since < 89*time.Second || since > 91*time.Second {
		t.Errorf("since = %v, want about 90s", since)
	}
}

// Replicas are tracked independently; one completing must not vouch for
// another on the same deployment.
func TestLastCompletionIsPerReplica(t *testing.T) {
	var l lastCompletion
	l.mark("d1", "r1", time.Now())
	if _, ok := l.since("d1", "r2", time.Now()); ok {
		t.Error("r2 should be unknown when only r1 completed a request")
	}
}
