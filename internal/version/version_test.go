package version

import "testing"

// The default has to stay "dev" and stay in step with pinned-versions.env's
// CP_VERSION on main. It is not a placeholder: the agent delivery path reads
// it as "this control plane has no published artifact to fetch", so changing
// it to something version-shaped would send pods after a release that was
// never cut.
func TestDefaultIsDev(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version = %q on an unstamped build, want \"dev\"", Version)
	}
}

func TestIsRelease(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"dev", false},
		{"", false},
		{"v0.2.3", true},
		{"v1.0.0-rc1", true},
	} {
		Version = tc.v
		if got := IsRelease(); got != tc.want {
			t.Errorf("IsRelease() with Version=%q = %v, want %v", tc.v, got, tc.want)
		}
	}
}
