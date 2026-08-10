package engineagent

import (
	"context"
	"os/exec"
	"strings"
)

// CountCards reports how many GPUs this node contributes, by asking
// nvidia-smi.
//
// This is the discovered half of the span (see the package doc). Card count
// is one of the few things a box genuinely knows about itself, in contrast
// to its provider identity, which it cannot see at all.
//
// A missing or failing nvidia-smi returns 0 with no error. That is a
// deliberate choice and the same one issue 213 makes for a sensor-less pool:
// an agent on a CPU box, or one whose provider does not expose the NVIDIA
// tooling inside the container, must still register. Reporting zero cards is
// a legible gap in the fleet view; refusing to register turns a missing
// reading into a missing member, which is a much worse failure to debug.
func CountCards(ctx context.Context) int32 {
	out, err := exec.CommandContext(ctx, "nvidia-smi", "-L").Output()
	if err != nil {
		return 0
	}
	return parseCardCount(string(out))
}

// parseCardCount counts the GPUs in `nvidia-smi -L` output. Split out from
// CountCards so the parsing is testable without the tool present.
//
// The format is one line per card:
//
//	GPU 0: NVIDIA RTX A6000 (UUID: GPU-f3aa01f8-...)
//	GPU 1: NVIDIA RTX A6000 (UUID: GPU-6c620167-...)
//
// MIG instances appear as indented "MIG ..." lines beneath their parent GPU.
// They are not counted: a MIG slice is a partition of a card already counted
// on the line above, so counting both would double-report the node's
// contribution to the group.
func parseCardCount(out string) int32 {
	var n int32
	for line := range strings.SplitSeq(out, "\n") {
		// No TrimSpace: the leading indent is exactly what distinguishes a
		// MIG instance line from a card line.
		if strings.HasPrefix(line, "GPU ") {
			n++
		}
	}
	return n
}
