package cmd

import "testing"

// A typo that quietly disabled the filter would surface as a container that
// will not start on a machine that is already billing, so an unrecognised
// architecture is an error rather than a silent pass.
func TestParseArch(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"amd64", "amd64", false},
		{"x86_64", "amd64", false},   // Lambda's spelling
		{"aarch64", "arm64", false},  // the other one vendors use
		{"  arm64 ", "arm64", false}, // operators type spaces
		{"x86", "", true},            // close enough to be a plausible typo
		{"amd", "", true},
		{"any", "", true},
	} {
		got, err := parseArch(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseArch(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("parseArch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
