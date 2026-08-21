package cmd

import (
	"fmt"
	"strings"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// archFlagUsage is shared by every verb that takes a shape, so an operator
// reads the same sentence wherever they meet it.
const archFlagUsage = `CPU architecture the host must have: amd64 | arm64. ` +
	`Comes from the engine image rather than from preference: an x86 image will not start on an arm64 host, ` +
	`and nothing notices until the container fails on a machine that is already billing. ` +
	`Unset means unconstrained. Accepts the spellings vendors use (x86_64, aarch64)`

// parseArch normalizes the architecture spellings operators and vendors use.
//
// Vendors disagree on the word for one fact: Lambda says x86_64 where Vast
// says amd64, which is why provisioners.NormalizeArch exists. An operator
// typing either should get the same filter, and anything unrecognised is an
// error rather than a silent pass, since a typo that quietly disabled the
// filter would surface as a container that will not start.
func parseArch(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", nil
	}
	norm := provisioners.NormalizeArch(trimmed)
	switch norm {
	case provisioners.ArchAMD64, provisioners.ArchARM64:
		return norm, nil
	default:
		return "", fmt.Errorf("--arch %q is not an architecture this filters on (want amd64 or arm64, or the vendor spellings x86_64 / aarch64)", s)
	}
}
