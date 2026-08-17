package skucatalog

// GiB is one binary gibibyte, the unit a GPU's memory is actually built
// in and the unit nvidia-smi reports.
const GiB int64 = 1 << 30

// binaryLabels are the advertised per-card figures that name a binary
// count, so a card labelled "N GB" holds exactly N GiB.
//
// An allow-list rather than a conversion, because the labels are
// marketing and only some of them are honest about the arithmetic. An
// A100 80GB holds 80 GiB, which nvidia-smi shows as 81920 MiB and which
// is 85.9 decimal GB. An H200's 141 and a Blackwell's 180 or 192 are
// vendor decimal figures that are not clean binary counts, so they are
// absent here and resolve to unknown rather than to a number that is
// several gigabytes wrong on the most expensive cards we can rent.
//
// A label nobody has verified defaults to unknown, which is the safe
// direction: a budget that declines to conclude sends an operator to
// check, and a budget computed from a fabricated capacity does not.
var binaryLabels = map[int]bool{
	24: true, // RTX 4090, A10, A10G, L4
	32: true, // V100 32GB
	40: true, // A100 40GB
	48: true, // A40, L40S, RTX 6000 Ada
	80: true, // A100 80GB, H100 80GB
	94: true, // H100 NVL
	96: true, // RTX PRO 6000 Blackwell
}

// ExactVRAMBytes converts a catalog's advertised per-card figure into the
// card's actual capacity, or 0 when no exact figure is known for that
// label.
//
// Callers pass the CATALOG's figure rather than the provider's, which is
// the whole point of the split. A vendor reports what its API rounds to
// and the three we talk to disagree by a gigabyte on the same physical
// part; the catalog row is a statement about which card this SKU is, and
// that is the thing capacity can be resolved from.
//
// 0 out means unknown and never zero. See Candidate.vram_bytes_per_gpu
// for what a caller owes that case (#323).
func ExactVRAMBytes(advertisedGb int) int64 {
	if advertisedGb <= 0 || !binaryLabels[advertisedGb] {
		return 0
	}
	return int64(advertisedGb) * GiB
}
