package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadProviders_AllBillingModes verifies that each documented
// billing mode (metered, bare-metal monthly, owned capex) normalizes
// into a sensible per-second rate. Numbers chosen so the expected
// results have small fractional drift.
func TestLoadProviders_AllBillingModes(t *testing.T) {
	yaml := `
last_updated: 2026-05-07
providers:
  - name: runpod
    billing_mode: metered_per_second
    gpu_type: a10g
    rate_usd_per_hour: 0.36       # $0.36/hr -> $0.0001/s
  - name: equinix_metal
    billing_mode: bare_metal_monthly
    gpu_type: a40
    fixed_usd_per_month: 730      # $1/hr -> $0.000277.../s
  - name: owned_4090
    billing_mode: owned_capex
    gpu_type: rtx4090
    capex_usd: 2400               # 2400 / 36 / 730 / 3600 = $2.54e-5/s
    amort_months: 36
    avg_power_watts: 350          # 350 * 0.12 / 1000 / 3600 = $1.17e-5/s
    power_usd_per_kwh: 0.12
`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d providers, want 3", len(got))
	}

	// metered: 0.36 / 3600 = 0.0001
	approxEq(t, "metered_per_second runpod", got[0].EffectiveRateUSDPerSec, 0.0001)
	// monthly: 730 / 730 / 3600 = 1/3600 ≈ 0.000277...
	approxEq(t, "bare_metal_monthly equinix", got[1].EffectiveRateUSDPerSec, 1.0/3600)
	// capex 2400/36/730/3600 + power 350*0.12/1000/3600
	expectedOwned := 2400.0/36/730/3600 + 350.0*0.12/1000/3600
	approxEq(t, "owned_capex 4090", got[2].EffectiveRateUSDPerSec, expectedOwned)
}

func TestLoadProviders_RejectsUnknownMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  - name: x\n    billing_mode: future_quantum_billing\n    gpu_type: a10\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadProviders(path)
	if err == nil {
		t.Fatal("expected error for unknown billing_mode")
	}
	if !strings.Contains(err.Error(), "future_quantum_billing") {
		t.Errorf("error message should name the bad mode, got: %v", err)
	}
}

func TestLoadProviders_RejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"metered missing rate", `providers:
  - name: x
    billing_mode: metered_per_second
    gpu_type: a10
`},
		{"monthly missing fixed", `providers:
  - name: x
    billing_mode: bare_metal_monthly
    gpu_type: a40
`},
		{"capex missing amort", `providers:
  - name: x
    billing_mode: owned_capex
    gpu_type: rtx4090
    capex_usd: 2400
`},
		{"missing gpu_type", `providers:
  - name: x
    billing_mode: metered_per_second
    rate_usd_per_hour: 0.5
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "p.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadProviders(path); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

// TestLoadProviders_FileMissing verifies the error path when the YAML
// itself can't be read. The control plane treats this as
// non-fatal at startup (cost panels degrade gracefully); this test
// just confirms the error surfaces with useful context.
func TestLoadProviders_FileMissing(t *testing.T) {
	_, err := LoadProviders("/no/such/path.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func approxEq(t *testing.T, name string, got, want float64) {
	t.Helper()
	const eps = 1e-9
	if got > want+eps || got < want-eps {
		t.Errorf("%s: got %.10f, want %.10f", name, got, want)
	}
}
