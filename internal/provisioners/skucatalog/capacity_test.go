package skucatalog

import "testing"

func TestExactVRAMBytesReadsAVerifiedLabelAsBinary(t *testing.T) {
	// The whole point of the split. An A100 80GB holds 80 GiB, which
	// nvidia-smi shows as 81920 MiB and which is 85.9 decimal GB, so a
	// budget handed 80,000,000,000 has lost seven percent of the card
	// before it starts.
	got := ExactVRAMBytes(80)
	if want := int64(85_899_345_920); got != want {
		t.Errorf("ExactVRAMBytes(80) = %d, want %d (80 GiB)", got, want)
	}
	if got == 80*1_000_000_000 {
		t.Error("the label was read as decimal GB, which is the bug this exists to fix")
	}
}

func TestExactVRAMBytesRefusesLabelsNobodyVerified(t *testing.T) {
	// H200's 141 and Blackwell's 180 and 192 are vendor decimal figures
	// rather than binary counts. Converting them anyway would put a
	// multi-gigabyte error on the most expensive cards we can rent, and
	// it would do it silently.
	for _, gb := range []int{141, 180, 192} {
		if got := ExactVRAMBytes(gb); got != 0 {
			t.Errorf("ExactVRAMBytes(%d) = %d, want 0; that label is not a binary count", gb, got)
		}
	}
}

func TestExactVRAMBytesDefaultsAnUnknownLabelToUnknown(t *testing.T) {
	// A card nobody has catalogued yet must not acquire a capacity by
	// arithmetic. Unknown sends an operator to check; a fabricated
	// figure does not.
	for _, gb := range []int{0, -1, 7, 111} {
		if got := ExactVRAMBytes(gb); got != 0 {
			t.Errorf("ExactVRAMBytes(%d) = %d, want 0 for a label with no verified capacity", gb, got)
		}
	}
}
