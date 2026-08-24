package provisioners

import (
	"context"
	"log/slog"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// ImageArchSource answers which CPU architectures a container image runs on.
// Optional: a Service without one behaves as it did before #405, which is to
// say it rents whatever the requirements alone allow.
//
// Returns an empty slice and a reason when it could not find out. That is
// not an error, because there is nothing for a caller to do about it except
// carry on, and the reason exists so carrying on is visible in a log rather
// than silent.
type ImageArchSource interface {
	Architectures(ctx context.Context, image string) ([]string, string)
}

// inferImageArchitecture fills in ResourceRequirements.architecture from the
// engine image, for every replica spec that did not state one.
//
// The operator is the wrong place to ask. `--arch` fixed the trap for
// somebody who already knows their image's platforms (#390); the person who
// does not know rents Lambda's arm64 GH200 for an x86 image and finds out
// when the container will not start on a machine that is already billing
// (#405). The image is the thing that actually constrains this, and the
// registry will say so.
//
// Nothing downstream changes. `skucatalog.Match` already drops a shape whose
// architecture the requirements refuse, and `FilterArchitecture` already
// drops a candidate that reports an incompatible one. This only supplies the
// input both were written for and neither had a way to obtain, so a request
// that could not be satisfied is refused before anything is rented rather
// than after.
//
// Three things it deliberately does not do.
//
// It never overrides a stated architecture. An operator who passed `--arch`
// has made a claim about their own image that may be better informed than a
// manifest, and a cross-built image is a real thing.
//
// It never overrides an explicit `--gpu-sku`. That flag is documented as the
// escape hatch that bypasses the resolver entirely, and refusing a shape the
// operator named by hand would contradict the one thing the flag promises.
// The inferred architecture is still recorded on the requirements, so the
// candidate listing narrows and the operator can see the mismatch.
//
// It never refuses because it could not read the registry. An unreadable
// manifest, a rate limit and a private image are one outcome as far as this
// is concerned: nothing was learned. Refusing there would block deploys that
// work today over a network call that is new. Same shape as budgetCheck,
// which skips when an input is missing and says which one.
//
// A multi-arch image constrains nothing, so it is treated as silence rather
// than as a list. `ResourceRequirements.architecture` is one value, and the
// filters read "" as unconstrained, which is exactly right for an image that
// runs on everything the vendor sells.
func (s *Service) inferImageArchitecture(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) {
	if s.imageArch == nil {
		return
	}
	dep := req.GetDeployment()
	image := dep.GetImage()
	if image == "" || isExternalDeploy(req) {
		return
	}

	// Only ask if somebody would use the answer.
	wanted := false
	for _, spec := range req.GetReplicasSpec() {
		if r := spec.GetRequirements(); r != nil && r.GetArchitecture() == "" {
			wanted = true
			break
		}
	}
	if !wanted {
		return
	}

	arches, why := s.imageArch.Architectures(ctx, image)
	if len(arches) == 0 {
		s.logArchSkip(dep.GetId(), image, why)
		return
	}
	if len(arches) > 1 {
		// Runs on more than one, so it constrains nothing. Recording one of
		// them would be a narrower claim than the image makes.
		slog.Info("engine image runs on several architectures, so it constrains nothing",
			"deployment", dep.GetId(), "image", image, "architectures", arches)
		return
	}

	arch := NormalizeArch(arches[0])
	if arch == "" {
		s.logArchSkip(dep.GetId(), image, "the registry reported architecture "+arches[0]+", which is not one iplane knows")
		return
	}
	for _, spec := range req.GetReplicasSpec() {
		r := spec.GetRequirements()
		if r == nil || r.GetArchitecture() != "" {
			continue
		}
		r.Architecture = arch
	}
	slog.Info("engine image architecture read from the registry",
		"deployment", dep.GetId(), "image", image, "architecture", arch)
}

// logArchSkip records that provisioning is proceeding without knowing what
// the image needs, and why. The reason is the point: a silent fallback and a
// deliberate one look identical in a log otherwise, and this one has a
// network call behind it that can fail in several uninteresting ways.
func (s *Service) logArchSkip(deploymentID, image, why string) {
	slog.Info("could not read the engine image's architecture; provisioning is unconstrained by it",
		"deployment", deploymentID, "image", image, "reason", why)
}

// ArchitecturesFor is a small helper for callers that hold a resolver and
// want the same normalization the deploy path applies.
//
// Returns "" for an image that runs on several architectures or on none it
// recognizes, which is the same "unconstrained" the requirements use.
func ArchitecturesFor(ctx context.Context, src ImageArchSource, image string) string {
	if src == nil || image == "" {
		return ""
	}
	arches, _ := src.Architectures(ctx, image)
	if len(arches) != 1 {
		return ""
	}
	return NormalizeArch(arches[0])
}
