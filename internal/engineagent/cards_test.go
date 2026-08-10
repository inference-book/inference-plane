package engineagent

import "testing"

func TestParseCardCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want int32
	}{
		{
			// The real output from the 2x RTX A6000 box in
			// docs/design/0007.
			name: "two cards",
			out: "GPU 0: NVIDIA RTX A6000 (UUID: GPU-f3aa01f8-1234-5678-9abc-def012345678)\n" +
				"GPU 1: NVIDIA RTX A6000 (UUID: GPU-6c620167-1234-5678-9abc-def012345678)\n",
			want: 2,
		},
		{
			name: "single card",
			out:  "GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-aaaaaaaa-1111-2222-3333-444444444444)\n",
			want: 1,
		},
		{
			name: "no trailing newline",
			out:  "GPU 0: NVIDIA L40S (UUID: GPU-bbbbbbbb-1111-2222-3333-444444444444)",
			want: 1,
		},
		{
			// A MIG slice is a partition of the card listed above it.
			// Counting both would double-report the node's contribution.
			name: "mig instances are not counted as cards",
			out: "GPU 0: NVIDIA A100-SXM4-80GB (UUID: GPU-cccccccc-1111-2222-3333-444444444444)\n" +
				"  MIG 3g.40gb     Device  0: (UUID: MIG-11111111-1111-1111-1111-111111111111)\n" +
				"  MIG 3g.40gb     Device  1: (UUID: MIG-22222222-2222-2222-2222-222222222222)\n" +
				"GPU 1: NVIDIA A100-SXM4-80GB (UUID: GPU-dddddddd-1111-2222-3333-444444444444)\n",
			want: 2,
		},
		{
			name: "empty output",
			out:  "",
			want: 0,
		},
		{
			// nvidia-smi is present but sees nothing. Zero is the honest
			// answer and the agent still registers.
			name: "no devices found",
			out:  "No devices were found.\n",
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCardCount(tc.out); got != tc.want {
				t.Errorf("parseCardCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// A box without the NVIDIA tooling in the container must still register.
// Reporting zero cards is a legible gap; refusing to register turns a
// missing reading into a missing member.
func TestCountCardsWithoutNvidiaSmiReturnsZero(t *testing.T) {
	// PATH is emptied for this process only; t.Setenv restores it.
	t.Setenv("PATH", "")
	if got := CountCards(t.Context()); got != 0 {
		t.Errorf("CountCards without nvidia-smi = %d, want 0", got)
	}
}
