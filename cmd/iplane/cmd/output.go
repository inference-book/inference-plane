package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// outputFormat enumerates valid --output values. We don't enforce a
// Go enum here -- the flag is a free-form string -- but the renderers
// fall through to table for any unknown value (with no error) so a
// future format addition can land without breaking older invocations.
const (
	outputTable = "table"
	outputJSON  = "json"
)

// renderInstance writes one Instance using the operator's chosen
// --output format. Used by create / describe / destroy.
//
// table format is human-readable; JSON is protojson with proto-name
// fields (matching the state-file's wire shape) so callers can pipe
// `iplane instance describe foo --output json | jq '.gpu.sku'`
// without remembering camelCase.
func renderInstance(w io.Writer, format string, inst *provisionerv1.Instance) error {
	if format == outputJSON {
		return writeProtoJSON(w, inst)
	}
	writeInstanceDetail(w, inst)
	return nil
}

// renderInstances writes a list of Instances. table format is the
// tabwriter summary; JSON emits the same shape as
// ListInstancesResponse so a downstream tool sees one consistent
// envelope across in-process and remote transports.
func renderInstances(w io.Writer, format string, instances []*provisionerv1.Instance) error {
	if format == outputJSON {
		return writeProtoJSON(w, &provisionerv1.ListInstancesResponse{Instances: instances})
	}
	writeInstanceTable(w, instances)
	return nil
}

// writeProtoJSON is the centralized protojson invocation so every
// command formats the same way. Indented for readability;
// UseProtoNames keeps the wire field names (`provider_id` etc.) so
// they match what the state file stores -- no camelCase surprise
// when piping to jq.
func writeProtoJSON(w io.Writer, m proto.Message) error {
	marshal := protojson.MarshalOptions{
		UseProtoNames: true,
		Multiline:     true,
		Indent:        "  ",
	}
	b, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// writeInstanceDetail prints every operator-facing field on the
// Instance as a key/value block. Used by describe and (in lighter
// form) create.
func writeInstanceDetail(w io.Writer, inst *provisionerv1.Instance) {
	fmt.Fprintf(w, "id:            %s\n", inst.GetId())
	fmt.Fprintf(w, "provider:      %s\n", inst.GetProvider())
	fmt.Fprintf(w, "provider id:   %s\n", inst.GetProviderId())
	fmt.Fprintf(w, "state:         %s\n", instanceStateLabel(inst.GetState()))
	fmt.Fprintf(w, "region:        %s\n", emptyAsDash(inst.GetRegion()))
	if gpu := inst.GetGpu(); gpu != nil {
		fmt.Fprintf(w, "gpu class:     %s\n", emptyAsDash(gpu.GetClass()))
		fmt.Fprintf(w, "gpu sku:       %s\n", emptyAsDash(gpu.GetSku()))
		fmt.Fprintf(w, "gpu count:     %d\n", gpu.GetCount())
		fmt.Fprintf(w, "vram (GB):     %d\n", gpu.GetVramGb())
	}
	fmt.Fprintf(w, "hourly rate:   $%.4f/hr\n", inst.GetHourlyRateUsd())
	if ts := inst.GetCreatedAt(); ts != nil {
		fmt.Fprintf(w, "created at:    %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := inst.GetActivatedAt(); ts != nil {
		fmt.Fprintf(w, "activated at:  %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := inst.GetTerminatedAt(); ts != nil {
		fmt.Fprintf(w, "terminated at: %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ssh := inst.GetSsh(); ssh != nil && ssh.GetHost() != "" {
		fmt.Fprintf(w, "ssh:           %s@%s:%d\n", ssh.GetUser(), ssh.GetHost(), ssh.GetPort())
	}
	if reason := inst.GetFailureReason(); reason != "" {
		fmt.Fprintf(w, "failure:       %s\n", reason)
	}
}

// writeInstanceTable prints a tabwriter-aligned summary suitable for
// the operator's first-glance scan: id, provider, state, sku, rate,
// region. Full record lives in describe.
func writeInstanceTable(w io.Writer, instances []*provisionerv1.Instance) {
	if len(instances) == 0 {
		fmt.Fprintln(w, "(no instances)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROVIDER\tSTATE\tSKU\tRATE\tREGION")
	for _, inst := range instances {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t$%.4f/hr\t%s\n",
			inst.GetId(),
			inst.GetProvider(),
			instanceStateLabel(inst.GetState()),
			emptyAsDash(inst.GetGpu().GetSku()),
			inst.GetHourlyRateUsd(),
			emptyAsDash(inst.GetRegion()),
		)
	}
	_ = tw.Flush()
}

// instanceStateLabel strips the protobuf enum prefix so humans see
// "ACTIVE" rather than "INSTANCE_STATE_ACTIVE".
func instanceStateLabel(s provisionerv1.InstanceState) string {
	const prefix = "INSTANCE_STATE_"
	name := s.String()
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

func emptyAsDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
