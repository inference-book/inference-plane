package provisioners

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
)

// Billing modes, as far as the record can honestly report them.
//
// Derived from the provider rather than stored, because nothing on an
// Instance says how it is billed. The three paid adapters all meter by
// the second; local hardware is already bought; an operator's own engine
// costs iplane nothing because iplane did not rent it.
//
// What this deliberately cannot express is reclaimable versus on-demand.
// Candidate.reclaimable is the granted signal and it dies in the
// candidate list, and Spec.requirements.reclaim_policy records what was
// asked for rather than what was given. On Vast the two tiers differ by
// roughly six times, so this label is wrong by that much on a bid
// rental, and closing it means persisting the granted flag.
const (
	billingMetered = "metered_per_second"
	billingOwned   = "owned"
	billingNone    = "none"
)

// BillingInstances implements metrics.FleetSource: every instance that
// is on a meter, or was.
//
// Reads the state file rather than asking providers. A cost figure is
// about what we rented and when, both of which are already written down,
// and a metrics scrape is the wrong place to discover a provider is
// slow. Read takes the in-process mutex and not the flock, so this
// answers while the daemon holds one.
//
// A record that never reached ACTIVE is omitted. The meter starts when
// the provider says the instance is up, and the time before that is
// provisioning latency the operator is not billed for.
func (s *Service) BillingInstances() []metrics.BillingInstance {
	file, err := s.store.Read()
	if err != nil {
		return nil
	}
	out := make([]metrics.BillingInstance, 0, len(file.Instances))
	for _, inst := range file.Instances {
		since := billingStart(inst)
		if since.IsZero() {
			continue
		}
		out = append(out, metrics.BillingInstance{
			ID:             inst.GetId(),
			Provider:       inst.GetProvider(),
			GPUSKU:         inst.GetHardware().GetGpuSku(),
			BillingMode:    billingModeFor(inst.GetProvider()),
			RateUSDPerHour: inst.GetHourlyRateUsd(),
			Since:          since,
			Until:          tsOrZero(inst.GetTerminatedAt()),
		})
	}
	return out
}

// billingStart is when the meter started.
//
// activated_at is the answer: the service stamps it in one place, when
// Spawn has returned and the instance is reachable, which is what the
// state enum calls "billing started". created_at is the fallback for
// records that never went through that funnel, and it is a fallback
// rather than the primary because it is written at PENDING, before the
// provider call returns, so it would bill provisioning latency.
func billingStart(inst *provisionerv1.Instance) time.Time {
	if t := tsOrZero(inst.GetActivatedAt()); !t.IsZero() {
		return t
	}
	return tsOrZero(inst.GetCreatedAt())
}

// billingModeFor maps a provider onto how it charges. An unrecognised
// provider reports metered, which is what every paid provider we have
// does and the safe assumption for a new one: reporting it as owned
// would quietly zero its cost.
func billingModeFor(provider string) string {
	switch provider {
	case ProviderLocal:
		return billingOwned
	case ProviderExternal:
		return billingNone
	default:
		return billingMetered
	}
}

// tsOrZero converts an absent timestamp into a zero time.
//
// Takes the concrete type rather than an interface on purpose. A nil
// *timestamppb.Timestamp stored in an interface is not a nil interface,
// so the guard would not fire and AsTime would return the Unix epoch,
// which reads as an instance that has been billing since 1970.
func tsOrZero(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

var _ metrics.FleetSource = (*Service)(nil)
