package cmd

import (
	"testing"
	"time"
)

func TestEngineReadyTimeout(t *testing.T) {
	const specific = "IPLANE_RUNPOD_ENGINE_READY_TIMEOUT"
	const generic = "IPLANE_ENGINE_READY_TIMEOUT"

	t.Run("neither set keeps the caller's default", func(t *testing.T) {
		t.Setenv(specific, "")
		t.Setenv(generic, "")
		if got := engineReadyTimeout(specific); got != 0 {
			t.Errorf("= %v, want 0", got)
		}
	})

	t.Run("generic applies to every path", func(t *testing.T) {
		t.Setenv(specific, "")
		t.Setenv(generic, "25m")
		if got := engineReadyTimeout(specific); got != 25*time.Minute {
			t.Errorf("= %v, want 25m", got)
		}
		// "" means a path with no provider-scoped var of its own.
		if got := engineReadyTimeout(""); got != 25*time.Minute {
			t.Errorf("generic-only lookup = %v, want 25m", got)
		}
	})

	// An operator who already set the RunPod-scoped variable keeps their
	// behaviour when the generic one arrives.
	t.Run("provider-scoped wins over generic", func(t *testing.T) {
		t.Setenv(specific, "40m")
		t.Setenv(generic, "5m")
		if got := engineReadyTimeout(specific); got != 40*time.Minute {
			t.Errorf("= %v, want the provider-scoped 40m", got)
		}
	})

	t.Run("provider-scoped is ignored by paths that do not have one", func(t *testing.T) {
		t.Setenv(specific, "40m")
		t.Setenv(generic, "")
		if got := engineReadyTimeout(""); got != 0 {
			t.Errorf("= %v, want 0; the sshdocker path must not inherit a RunPod-scoped value", got)
		}
	})

	// Refusing to start the daemon over a typo'd duration would be worse
	// than using the default and letting the deploy report what happened.
	t.Run("malformed falls through rather than failing", func(t *testing.T) {
		t.Setenv(specific, "twenty minutes")
		t.Setenv(generic, "12m")
		if got := engineReadyTimeout(specific); got != 12*time.Minute {
			t.Errorf("= %v, want the generic 12m after the malformed specific", got)
		}
	})

	t.Run("zero and negative are ignored", func(t *testing.T) {
		t.Setenv(specific, "0s")
		t.Setenv(generic, "")
		if got := engineReadyTimeout(specific); got != 0 {
			t.Errorf("= %v, want 0", got)
		}
	})
}
