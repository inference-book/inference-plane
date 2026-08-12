package engineagent

import (
	"context"
	"os/exec"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Interconnect health: the reading that tells a working group from a working
// group running at a fraction of its speed.
//
// Lose one NVLink in a tensor-parallel group and the cards route around it.
// The collective falls back to a slower path, every process stays alive, the
// endpoint answers, and the tokens are correct. `/health` says serving, which
// is true. The router's in-flight counts look normal. No VRAM or utilisation
// gauge moves in a way that distinguishes this from a quiet period. The
// deployment reads as healthy while delivering a fraction of the machine
// being paid for, and nothing in iplane could see it before this.
//
// This is the sensor. The channel that carries it upward is the agent's
// registration, and the decision to do anything about it is deliberately
// somewhere else (issue 205): a degraded group might be worth draining,
// replacing, or leaving alone, and that is an operator's call.

// ReadInterconnect reports link health for this node by asking nvidia-smi.
//
// Never returns nil, and never reports a fault it did not observe. A missing
// nvidia-smi, a provider that hides the NVIDIA tooling inside the container,
// and a PCIe-only box with no links at all are all the same answer:
// available=false, meaning no reading. That is the same choice CountCards
// makes for a missing tool, and for the same reason -- a legible gap in the
// fleet view beats either a false healthy or a hard error, and an agent that
// refuses to register turns a missing reading into a missing member, which is
// far worse to debug.
func ReadInterconnect(ctx context.Context) *provisionerv1.InterconnectHealth {
	out, err := exec.CommandContext(ctx, "nvidia-smi", "nvlink", "-s").Output()
	if err != nil {
		// Includes the "no NVLink on this board" case: nvidia-smi exits
		// non-zero rather than printing an empty report on several driver
		// versions.
		return &provisionerv1.InterconnectHealth{Available: false}
	}
	return parseNVLinkStatus(string(out))
}

// parseNVLinkStatus turns `nvidia-smi nvlink -s` output into a reading. Split
// from ReadInterconnect so the parsing is testable without the tool, and
// without an NVLink board to test against.
//
// The format is a GPU header followed by one indented line per link:
//
//	GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-f3aa01f8-...)
//	     Link 0: 25.781 GB/s
//	     Link 1: 25.781 GB/s
//	     Link 2: <inactive>
//
// A link is up when it reports a bandwidth. Down links appear as
// `<inactive>`, and boards without NVLink print `(Not supported)` under the
// GPU header or nothing at all.
//
// No Link lines anywhere means no reading, NOT zero links up. The difference
// is the whole point: a PCIe-only pool must not read as an impaired NVLink
// pool, or every Ch 6-9 deploy starts reporting a fault it does not have.
func parseNVLinkStatus(out string) *provisionerv1.InterconnectHealth {
	var total, up int32
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Link ") {
			continue
		}
		total++
		// A bandwidth figure is the only positive evidence a link carries
		// traffic. Matching on the absence of "<inactive>" instead would
		// count "(Not supported)" and any future status string as healthy,
		// which fails open on exactly the reading this exists to catch.
		if strings.Contains(trimmed, "GB/s") {
			up++
		}
	}
	if total == 0 {
		return &provisionerv1.InterconnectHealth{Available: false}
	}
	return &provisionerv1.InterconnectHealth{
		Available:  true,
		LinksTotal: total,
		LinksUp:    up,
	}
}

// InterconnectImpaired is the threshold, and it is deliberately the simplest
// one that can be defended: on a board that reports N links, fewer than N up
// is an impairment.
//
// Link down is unambiguous, and that is what this catches. A link that is up
// but retrying hard is the interesting middle case and is NOT covered here:
// reporting it needs per-link error and replay counters, and picking a
// retry-rate threshold without a real impaired board to calibrate against
// would be inventing a number. A threshold nobody can defend is worse than a
// narrower one that is obviously right, because the first thing an operator
// asks about a degraded member is why.
//
// No reading is never an impairment. Absence of evidence is not evidence of a
// downed link, and treating it as one would mark every PCIe pool degraded.
func InterconnectImpaired(h *provisionerv1.InterconnectHealth) bool {
	if h == nil || !h.GetAvailable() {
		return false
	}
	return h.GetLinksUp() < h.GetLinksTotal()
}

// InterconnectProbe adapts the sensor into the impairment half of
// AnyDegraded, which is where a health check and a link reading combine into
// one state.
//
// read is injectable so tests, and the mock engine, can supply a reading
// without an NVLink board present.
func InterconnectProbe(read func(context.Context) *provisionerv1.InterconnectHealth) func(context.Context) bool {
	if read == nil {
		read = ReadInterconnect
	}
	return func(ctx context.Context) bool {
		return InterconnectImpaired(read(ctx))
	}
}
