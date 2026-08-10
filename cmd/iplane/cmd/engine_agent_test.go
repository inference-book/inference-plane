package cmd

import "testing"

func TestEnvOr(t *testing.T) {
	const key = "IPLANE_TEST_ENVOR"

	t.Run("unset falls back", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOr(key, "fallback"); got != "fallback" {
			t.Errorf("envOr = %q, want fallback", got)
		}
	})

	t.Run("set wins", func(t *testing.T) {
		t.Setenv(key, "injected")
		if got := envOr(key, "fallback"); got != "injected" {
			t.Errorf("envOr = %q, want injected", got)
		}
	})
}

func TestEnvInt(t *testing.T) {
	const key = "IPLANE_TEST_ENVINT"

	for _, tc := range []struct {
		name string
		set  string
		want int
	}{
		{"unset falls back", "", 0},
		{"parses a value", "3", 3},
		{"zero is honoured", "0", 0},
		// A malformed node index is display ordering, not correctness.
		// Falling back keeps the member visible; failing would trade a
		// cosmetic problem for an invisible one.
		{"malformed falls back", "node-two", 0},
		{"negative parses", "-1", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.set)
			if got := envInt(key, 0); got != tc.want {
				t.Errorf("envInt(%q) = %d, want %d", tc.set, got, tc.want)
			}
		})
	}
}

// The deploy path stamps these and the agent reads them. A rename on one
// side without the other produces a member with no identity, which is the
// exact failure issue 214's attribution cannot survive, so the names are
// pinned rather than left to drift.
func TestInjectedEnvVarNames(t *testing.T) {
	for name, got := range map[string]string{
		"IPLANE_ENGINE_ID":         EnvEngineID,
		"IPLANE_DEPLOYMENT_ID":     EnvEngineDeployID,
		"IPLANE_ENGINE_MODEL":      EnvEngineModel,
		"IPLANE_ENGINE_ENDPOINT":   EnvEngineEndpoint,
		"IPLANE_ENGINE_HEALTH_URL": EnvEngineHealthURL,
		"IPLANE_HOST_ID":           EnvEngineHostID,
		"IPLANE_NODE_INDEX":        EnvEngineNodeIndex,
	} {
		if got != name {
			t.Errorf("env constant = %q, want %q", got, name)
		}
	}
}
