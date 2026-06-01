package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// renderDeployment writes one Deployment using --output format.
func renderDeployment(w io.Writer, format string, dep *provisionerv1.Deployment) error {
	if format == outputJSON {
		return writeProtoJSON(w, dep)
	}
	writeDeploymentDetail(w, dep)
	return nil
}

// renderDeployments writes a list of Deployments.
func renderDeployments(w io.Writer, format string, deps []*provisionerv1.Deployment) error {
	if format == outputJSON {
		return writeProtoJSON(w, &provisionerv1.ListDeploymentsResponse{Deployments: deps})
	}
	writeDeploymentTable(w, deps)
	return nil
}

// writeDeploymentDetail prints every operator-facing field on the
// Deployment as a key/value block. Used by describe and (in lighter
// form) deploy.
func writeDeploymentDetail(w io.Writer, dep *provisionerv1.Deployment) {
	fmt.Fprintf(w, "id:              %s\n", dep.GetId())
	fmt.Fprintf(w, "instance:        %s\n", dep.GetInstanceId())
	fmt.Fprintf(w, "image:           %s\n", dep.GetImage())
	fmt.Fprintf(w, "model:           %s\n", dep.GetModel())
	fmt.Fprintf(w, "state:           %s\n", deploymentStateLabel(dep.GetState()))
	if phase := dep.GetCurrentPhase(); phase != "" {
		fmt.Fprintf(w, "phase:           %s\n", phase)
	}
	if msg := dep.GetProgressMessage(); msg != "" {
		fmt.Fprintf(w, "progress:        %s\n", msg)
	}
	fmt.Fprintf(w, "engine port:     %d\n", dep.GetEnginePort())
	if endpoint := dep.GetEngineEndpoint(); endpoint != "" {
		fmt.Fprintf(w, "engine endpoint: %s\n", endpoint)
	}
	// v0.2 ch7-beat1.3b: when a running daemon is the data source,
	// render the OpenAI-compat base URL (the flat /v1 endpoint that
	// routes by model-in-body). This is what operators paste into
	// OpenAI SDKs. The deploy-id form lives as an explicit-deployment
	// escape hatch and is documented in CLI help rather than shouted
	// in every describe.
	if dep.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING && deploymentServiceURL != "" {
		fmt.Fprintf(w, "openai base url: %s/v1\n", deploymentServiceURL)
		fmt.Fprintf(w, "  (model field in request body selects this deployment when set to %q)\n", dep.GetModel())
		fmt.Fprintf(w, "  (explicit-dispatch URL: %s/v1/%s/v1)\n", deploymentServiceURL, dep.GetId())
	}
	if cid := dep.GetContainerId(); cid != "" {
		fmt.Fprintf(w, "container:       %s\n", cid)
	}
	if args := dep.GetEngineArgs(); len(args) > 0 {
		fmt.Fprintf(w, "engine args:     %v\n", args)
	}
	if env := dep.GetEnv(); len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "env %-12s %s\n", k+":", env[k])
		}
	}
	if ts := dep.GetCreatedAt(); ts != nil {
		fmt.Fprintf(w, "created at:      %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := dep.GetStartedAt(); ts != nil {
		fmt.Fprintf(w, "started at:      %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := dep.GetReadyAt(); ts != nil {
		fmt.Fprintf(w, "ready at:        %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := dep.GetTerminatedAt(); ts != nil {
		fmt.Fprintf(w, "terminated at:   %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ttl := dep.GetIdleTtlSeconds(); ttl > 0 {
		fmt.Fprintf(w, "idle ttl:        %ds\n", ttl)
	}
	if ts := dep.GetLastActivityAt(); ts != nil {
		fmt.Fprintf(w, "last activity:   %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if dep.GetNoIdleDestroy() {
		fmt.Fprintf(w, "pinned:          true (no idle destroy)\n")
	}
	if reason := dep.GetFailureReason(); reason != "" {
		fmt.Fprintf(w, "failure:         %s\n", reason)
	}
}

// writeDeploymentTable prints a tabwriter-aligned summary suitable
// for the operator's first-glance scan: id, instance, state, model,
// endpoint. Full record lives in describe.
func writeDeploymentTable(w io.Writer, deps []*provisionerv1.Deployment) {
	if len(deps) == 0 {
		fmt.Fprintln(w, "(no deployments)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tINSTANCE\tSTATE\tMODEL\tENDPOINT")
	for _, dep := range deps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			dep.GetId(),
			dep.GetInstanceId(),
			deploymentStateLabel(dep.GetState()),
			dep.GetModel(),
			emptyAsDash(dep.GetEngineEndpoint()),
		)
	}
	_ = tw.Flush()
}

// deploymentStateLabel strips the protobuf enum prefix so humans see
// "RUNNING" rather than "DEPLOYMENT_STATE_RUNNING".
func deploymentStateLabel(s provisionerv1.DeploymentState) string {
	const prefix = "DEPLOYMENT_STATE_"
	name := s.String()
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

// dryRunDeploy is the deploy-verb dry-run path. Mirrors the Service's
// validation + idempotency lookup but stops at "would deploy" without
// touching SSH or docker on the target instance.
//
// Three terminal cases:
//
//   1. Deployment id already exists with matching (image, model) and
//      a live state (PENDING / STARTING / CONFIGURING / RUNNING):
//      print "would no-op" and exit. The Service's idempotency
//      contract returns the existing record without instance calls.
//
//   2. Deployment id already exists with drifting (image, model) or
//      a recyclable state (TERMINATED / FAILED): print "would
//      replace" -- the executor will stop+rm the existing container
//      before pulling and running the new one.
//
//   3. No record (or NotFound): print "would deploy" fresh.
//
// In all cases the instance is looked up so we can surface its
// provider + SSH endpoint to the operator; an instance that doesn't
// exist or has no SSH endpoint is reported as a hard error before
// the executor would ever run.
func dryRunDeploy(ctx context.Context, w io.Writer, client deploymentClient, dep *provisionerv1.Deployment) error {
	descCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.DescribeDeployment(descCtx, &provisionerv1.DescribeDeploymentRequest{
		Id: dep.GetId(),
	})
	switch {
	case err == nil:
		existing := resp.GetDeployment()
		matchingImage := existing.GetImage() == dep.GetImage()
		matchingModel := existing.GetModel() == dep.GetModel()
		isLive := existing.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING ||
			existing.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING ||
			existing.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING ||
			existing.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING
		if matchingImage && matchingModel && isLive {
			fmt.Fprintf(w, "[dry-run] would no-op: %q already exists on instance %q (state=%s). idempotent re-deploy returns the existing record without an instance call.\n",
				existing.GetId(), existing.GetInstanceId(),
				deploymentStateLabel(existing.GetState()))
			return nil
		}
		fmt.Fprintf(w, "[dry-run] would replace %q on instance %q\n",
			existing.GetId(), existing.GetInstanceId())
		fmt.Fprintf(w, "[dry-run]   existing image:    %s\n", existing.GetImage())
		fmt.Fprintf(w, "[dry-run]   existing model:    %s\n", existing.GetModel())
		fmt.Fprintf(w, "[dry-run]   existing state:    %s\n", deploymentStateLabel(existing.GetState()))
		fmt.Fprintf(w, "[dry-run]   new image:         %s\n", dep.GetImage())
		fmt.Fprintf(w, "[dry-run]   new model:         %s\n", dep.GetModel())
		fmt.Fprintln(w, "[dry-run] the executor would stop + rm the existing container before pulling and running the new one.")
		return nil
	case isNotFound(err):
		// fall through to "would deploy" fresh
	default:
		return fmt.Errorf("dry-run lookup of %q: %w", dep.GetId(), err)
	}

	fmt.Fprintf(w, "[dry-run] would deploy %q on instance %q\n", dep.GetId(), dep.GetInstanceId())
	fmt.Fprintf(w, "[dry-run]   image:       %s\n", dep.GetImage())
	fmt.Fprintf(w, "[dry-run]   model:       %s\n", dep.GetModel())
	fmt.Fprintf(w, "[dry-run]   engine port: %d\n", dep.GetEnginePort())
	if len(dep.GetEngineArgs()) > 0 {
		fmt.Fprintf(w, "[dry-run]   engine args: %v\n", dep.GetEngineArgs())
	}
	if len(dep.GetEnv()) > 0 {
		fmt.Fprintf(w, "[dry-run]   env:         %d vars\n", len(dep.GetEnv()))
	}
	fmt.Fprintln(w, "[dry-run] no SSH or docker calls made, no state file changes.")
	return nil
}

// dryRunDeploymentDestroy is the destroy-verb dry-run path. Looks up
// the record and prints what would happen. NotFound is reported as
// an error (operators destroying nothing is more often a typo than
// intent); an already-TERMINATED record is a no-op.
func dryRunDeploymentDestroy(ctx context.Context, w io.Writer, client deploymentClient, id string) error {
	descCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.DescribeDeployment(descCtx, &provisionerv1.DescribeDeploymentRequest{Id: id})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("no deployment with id %q (nothing to destroy)", id)
		}
		return fmt.Errorf("dry-run lookup of %q: %w", id, err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		fmt.Fprintf(w, "[dry-run] would no-op: %q is already TERMINATED.\n", id)
		return nil
	}
	fmt.Fprintf(w, "[dry-run] would destroy %q on instance %q\n", id, dep.GetInstanceId())
	fmt.Fprintf(w, "[dry-run]   from state:  %s\n", deploymentStateLabel(dep.GetState()))
	if cid := dep.GetContainerId(); cid != "" {
		fmt.Fprintf(w, "[dry-run]   container:   %s\n", cid)
	}
	fmt.Fprintln(w, "[dry-run] no SSH or docker calls made, no state file changes.")
	return nil
}
