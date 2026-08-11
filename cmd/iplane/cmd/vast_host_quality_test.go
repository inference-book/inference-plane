package cmd

import (
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners/vast"
)

func TestVastHostQualityFloor(t *testing.T) {
	const inetKey = "IPLANE_VAST_MIN_INET_DOWN_MBPS"
	const relKey = "IPLANE_VAST_MIN_RELIABILITY"

	t.Run("neither set leaves the adapter's defaults alone", func(t *testing.T) {
		t.Setenv(inetKey, "")
		t.Setenv(relKey, "")
		if _, _, ok := vastHostQualityFloor(); ok {
			t.Error("reported an override with nothing set; the adapter default would be restated by the CLI")
		}
	})

	// The knobs are independent. Lowering the bandwidth floor for a
	// thin-capacity search must not also silently drop the reliability floor,
	// which is what a naive "set both from env" would do.
	t.Run("one set keeps the other at its default", func(t *testing.T) {
		t.Setenv(inetKey, "250")
		t.Setenv(relKey, "")
		inet, rel, ok := vastHostQualityFloor()
		if !ok {
			t.Fatal("override not reported")
		}
		if inet != 250 {
			t.Errorf("inet = %v, want 250", inet)
		}
		if rel != vast.DefaultMinReliability {
			t.Errorf("reliability = %v, want the untouched default %v", rel, vast.DefaultMinReliability)
		}
	})

	// 0 is a real value here: it turns the floor off.
	t.Run("zero disables a floor", func(t *testing.T) {
		t.Setenv(inetKey, "0")
		t.Setenv(relKey, "0")
		inet, rel, ok := vastHostQualityFloor()
		if !ok || inet != 0 || rel != 0 {
			t.Errorf("= (%v, %v, %v), want (0, 0, true)", inet, rel, ok)
		}
	})

	// A typo must not read as "disable the floor". Silently rescuing a bad
	// value to 0 would reproduce the slow-host failure the floors prevent,
	// and it would do so on the runs where someone was trying to be careful.
	t.Run("malformed and negative values are ignored", func(t *testing.T) {
		for _, bad := range []string{"fast", "-1", "1e"} {
			t.Setenv(inetKey, bad)
			t.Setenv(relKey, "")
			inet, _, ok := vastHostQualityFloor()
			if ok || inet != vast.DefaultMinInetDownMbps {
				t.Errorf("%q: = (%v, ok=%v), want the default %v kept and ok=false",
					bad, inet, ok, float64(vast.DefaultMinInetDownMbps))
			}
		}
	})
}
