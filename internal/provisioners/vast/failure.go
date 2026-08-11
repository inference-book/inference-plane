package vast

import "strings"

// Terminal host failures, and why noticing them is worth its own file.
//
// Vast tells us when a container is never going to run. We used to keep
// polling anyway. Two hosts failed this way in one session on 2026-08-11 and
// both reported it clearly:
//
//	# broken IPv6 path to the registry CDN
//	actual_status: loading   cur_state: stopped
//	status_msg: error pulling image configuration: download failed after
//	  attempts=6: read tcp [2409:...]->[2600:9000:...]:443: read: connection
//	  reset by peer
//
//	# broken NVIDIA Container Device Interface
//	actual_status: created   cur_state: stopped
//	status_msg: Error response from daemon: ... OCI runtime create failed:
//	  ... failed to inject CDI devices: unresolvable CDI devices .../gpu=0
//
// In both cases the deploy sat at "waiting for port 8000 to be mapped" and
// would have burned the full engine-ready timeout. The cost is linear in the
// price of the box: on the 4x A100 the Ch 10 A/B needs, that is roughly $1.20
// per attempt instead of $0.02, and issue #214 means a retry may land on the
// same host again.
//
// The hard part is not spotting a failure. It is not spotting one that is not
// there. A false positive kills a healthy deploy that was merely slow, and a
// slow deploy is the normal case here: a 10 GB engine image on community
// capacity routinely takes minutes. So this errs heavily toward waiting.

// terminalSignatures are substrings that only ever appear when the container
// will not run. Matched case-insensitively against status_msg.
//
// Every entry is a string observed on a real failed host, not a guess about
// what docker might say. Adding a speculative pattern here is how a healthy
// deploy gets killed.
var terminalSignatures = []string{
	// The image cannot be fetched. Docker has already exhausted its retries by
	// the time this appears, which is what makes it terminal rather than slow.
	"error pulling image",
	// The container could not be created: runtime, device, or spec failure.
	"oci runtime create failed",
	"failed to start containers",
	"error response from daemon",
	// The image reference itself is wrong, so no amount of waiting helps.
	"manifest unknown",
	"repository does not exist",
	"no such image",
	"pull access denied",
}

// transientSignatures are substrings that look alarming and are not terminal.
// Checked first, so a message carrying both a retry notice and an error-ish
// word keeps waiting.
//
// "Retrying" is the important one. Docker prints it while it is still working,
// and the same host that printed it forty times did eventually fail with a
// terminal message. Treating the retry itself as fatal would give up on hosts
// that recover, which several did.
var transientSignatures = []string{
	"retrying",
	"downloading",
	"verifying checksum",
	"download complete",
	"pull complete",
	"extracting",
	"waiting",
}

// terminalHostFailure reports whether the host has definitively failed, and
// the signature to show the operator.
//
// Deliberately keyed on status_msg rather than on cur_state. cur_state reaches
// "stopped" for benign reasons early in a container's life, and both observed
// failures carried a specific, unambiguous message; the message is the
// evidence and the state is corroboration. Requiring both would add false
// negatives without removing false positives.
func terminalHostFailure(curState, statusMsg string) (bool, string) {
	msg := strings.ToLower(statusMsg)
	if strings.TrimSpace(msg) == "" {
		return false, ""
	}
	for _, s := range transientSignatures {
		if strings.Contains(msg, s) {
			return false, ""
		}
	}
	for _, s := range terminalSignatures {
		if strings.Contains(msg, s) {
			return true, summarizeFailure(statusMsg, curState)
		}
	}
	return false, ""
}

// summarizeFailure renders the host's own words for the operator, because the
// message IS the diagnosis. "error pulling image configuration ... connection
// reset by peer" against an IPv6 CDN address is what identified a broken host
// network path; discarding it in favour of "deploy failed" would have left
// nothing to act on.
//
// Collapsed to one line and bounded, since docker's multi-line output would
// otherwise wreck the progress display it lands in.
func summarizeFailure(statusMsg, curState string) string {
	one := strings.Join(strings.Fields(statusMsg), " ")
	const limit = 240
	if len(one) > limit {
		one = one[:limit] + "..."
	}
	if curState != "" {
		return one + " (cur_state=" + curState + ")"
	}
	return one
}
