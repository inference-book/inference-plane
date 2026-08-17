package vast

import (
	"fmt"
	"strings"
)

// Cold-start phases, and why a marketplace deploy needs them.
//
// Every pre-serving condition used to collapse into one line: "waiting for
// port 8000 to be mapped (9m20s elapsed)". During one session on 2026-08-11
// that same line covered an image downloading normally, an image pull
// retrying against a broken IPv6 path that would never finish, a container
// that had already exited with a CDI error, and an engine loading a model.
// Four situations, one of them fatal, indistinguishable to the operator and
// to the phase histogram (#259).
//
// The ladder mirrors RunPod's, which learned the same lesson in #208, and the
// engine-facing rungs are spelled without a provider prefix on purpose. A
// deploy dashboard slicing `iplane.deployment.phase.duration` by phase should
// put a RunPod engine-init next to a Vast one; the parts that differ are the
// provider's own scheduling and pull, and those keep their prefix.
//
// The issue proposed `vast:engine-init`. Unprefixed here instead, because the
// same issue asks for the metrics to "slice the same way RunPod's do", and
// RunPod already emits `engine:init`.
const (
	phaseScheduling = "vast:scheduling"
	phaseImagePull  = "vast:image-pull"
	phaseEngineInit = "engine:init"
)

// Vast's own status vocabulary, as observed on real contracts. actual_status
// walks created -> loading -> running while the container is being made,
// pulled, and started.
const (
	vastStatusCreated = "created"
	vastStatusLoading = "loading"
	vastStatusRunning = "running"
)

// enginePhaseOrdinal ranks the phases so the loop only ever advances the
// operator's view. Unknown ranks below everything, so a status read that
// flakes or returns a word we do not model is never mistaken for progress
// and never rewinds the ladder.
func enginePhaseOrdinal(phase string) int {
	switch phase {
	case phaseScheduling:
		return 1
	case phaseImagePull:
		return 2
	case phaseEngineInit:
		return 3
	default:
		return 0
	}
}

// classifyEnginePhase maps one observation onto a rung.
//
// endpointReady outranks the status word because it is a stronger signal: the
// port is mapped only once the container is running, and Vast's status can lag
// behind the docker daemon it is reporting on.
//
// Keyed on actual_status rather than on elapsed time, which is the correction
// #208 made for RunPod. A timestamp says something happened at some point; the
// question the operator is asking is what is true now.
func classifyEnginePhase(actualStatus string, endpointReady bool) string {
	if endpointReady {
		return phaseEngineInit
	}
	switch strings.ToLower(strings.TrimSpace(actualStatus)) {
	case vastStatusRunning:
		return phaseEngineInit
	case vastStatusLoading:
		return phaseImagePull
	case vastStatusCreated:
		return phaseScheduling
	default:
		return phaseScheduling
	}
}

// enginePhaseDescription is the operator-facing prefix for a rung, used when
// the host has nothing more specific to say.
func enginePhaseDescription(phase string) string {
	switch phase {
	case phaseImagePull:
		return "pulling engine image"
	case phaseEngineInit:
		return "engine starting (model download + load)"
	default:
		return "waiting for the host to create the container"
	}
}

// pullProgress renders docker's own account of the pull, or "" when the host
// is saying nothing worth repeating.
//
// The host's words are the whole value here. status_msg carries the layer
// progress verbatim, and the one phrase that matters most is "Retrying",
// which is the difference between a pull that is slow and a pull that will
// never finish. A generic wait message hides exactly that distinction, and
// hiding it cost nine minutes and a rental on 2026-08-11.
//
// Only progress-shaped messages are surfaced. A terminal one is the
// FailureReporter's to report, and repeating it here as though it were
// progress would tell the operator to keep waiting for a container that has
// already died.
func pullProgress(statusMsg string) string {
	msg := collapseWhitespace(statusMsg)
	if msg == "" {
		return ""
	}
	lower := strings.ToLower(msg)
	for _, s := range transientSignatures {
		if strings.Contains(lower, s) {
			return truncate(msg, 120)
		}
	}
	return ""
}

// phaseProgress composes what the operator sees on one tick: the rung, the
// host's own words when it has any, and the fallback endpoint/health note
// otherwise.
func phaseProgress(phase, hostMsg, note string) string {
	switch {
	case hostMsg != "":
		return fmt.Sprintf("%s: %s", enginePhaseDescription(phase), hostMsg)
	case note != "":
		return fmt.Sprintf("%s (%s)", enginePhaseDescription(phase), note)
	default:
		return enginePhaseDescription(phase)
	}
}

// collapseWhitespace flattens docker's multi-line output onto one line, since
// status_msg is rendered into a single progress field.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate bounds a host-supplied string so one chatty message cannot swamp
// the progress field.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
