package cmd

import (
	"fmt"
	"net/http"

	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// Local-service constructors shared by the read-only verbs.
//
// They live here rather than beside any one command because more than one
// command needs them, and a helper filed under `capacity` that `model` also
// calls sends the next reader to the wrong file.

// buildCapacityClient resolves who answers a capacity question.
//
// Remote when --service-url is set, and this is the whole of #304. The first
// version of these verbs always built a local service, so pointing them at a
// control plane produced a confident answer from the wrong host: provider
// credentials live in the daemon's environment, so a CLI without keys reported
// "no capacity" for vendors the daemon could see perfectly well.
//
// The reasoning that got this wrong is worth keeping. The test applied was
// "does it mutate state", which is why the write verbs got RPCs and the read
// verbs did not. The test that matters is where the INPUTS live. A read that
// depends on privileged inputs belongs where those inputs are, exactly as much
// as a write does.
func buildCapacityClient() (provisionerClient, error) {
	if instanceServiceURL != "" {
		return &connectProvisionerClient{
			c: provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, instanceServiceURL),
		}, nil
	}
	return buildReadOnlyService()
}

// buildReadOnlyService opens the state store WITHOUT taking the lifetime lock
// and wires the same provider set the daemon uses.
//
// The missing lock is the point rather than an oversight. `iplane model pin`
// takes it because it writes; this path only reads a provider's API and never
// touches state, so taking the lock would make the command fail exactly when a
// daemon is up, which is when an operator is most likely to be asking whether
// there is capacity to scale onto.
//
// Only the local branch of buildCapacityClient uses this. In remote mode there
// is no store to open, because the daemon holds it.
func buildReadOnlyService() (*provisioners.Service, error) {
	dir, err := resolveDeploymentStateDir()
	if err != nil {
		return nil, err
	}
	store, err := file.Open(dir, deploymentOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	return buildLocalService(store, deploymentOperatorID)
}

// cannotAnswerError explains a provider that has no way to answer.
//
// Says it is a property of the provider's API rather than a gap in iplane,
// because otherwise it reads as a missing feature and somebody files a ticket
// for it.
func cannotAnswerError(provider string) error {
	return fmt.Errorf("%s cannot list candidates without renting one.\n"+
		"  this is a property of the provider's API, not a gap in iplane: a marketplace\n"+
		"  publishes live offers to choose among, while a fixed-catalog provider may only\n"+
		"  publish a price list. `iplane instance create --dry-run` still shows what the\n"+
		"  static catalog would resolve to on %s", provider, provider)
}
