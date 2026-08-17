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
// Metered is correct for every paid rental today, including on Vast,
// because no rental is reclaimable. reclaim_policy constrains selection
// and nothing more: it is read only in the three candidates.go files,
// RunPod's create request carries an Interruptible field that is never
// assigned, and Vast's rent body sends no bid price. The proto comment
// on ReclaimPolicy says the same thing from the other side.
//
// So this needs revisiting the day a rental can actually be reclaimable,
// and not before. On Vast the two tiers differ by roughly six times, so
// a granted-tier signal will matter then; persisting one now would store
// a flag that is permanently false (#333).
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
// A record with no creation time is omitted, which means one this
// service never wrote. Everything else is billing, including an instance
// still PENDING: on an image-native provider the whole cold start
// happens in that state and the provider charges throughout it (#335).
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

// billingStart is when the meter started, which is when the machine was
// rented rather than when the engine came up.
//
// This preferred activated_at until a real 72B run showed what that
// costs. On an image-native provider the instance IS the engine pod, so
// activated_at is stamped only once the deploy reaches RUNNING, and a
// cold start that took fifteen minutes and $1.71 reported as 0.0 minutes
// and $0.01. The provider had been charging since the contract existed
// (#335).
//
// The reasoning that put activated_at first was that created_at is
// written at PENDING and would bill provisioning latency. That holds
// where a machine is rented and something is deployed onto it later. It
// is backwards where renting the box and starting the engine are one
// call, because there the provisioning latency is exactly what is being
// billed: the meter runs through the image pull and the weight download.
//
// created_at is set by every adapter at construction and guaranteed by
// finalizeActive, and RunPod's is the provider's own pod timestamp
// rather than our clock. activated_at survives only as a fallback for a
// record this service did not write.
func billingStart(inst *provisionerv1.Instance) time.Time {
	if t := tsOrZero(inst.GetCreatedAt()); !t.IsZero() {
		return t
	}
	return tsOrZero(inst.GetActivatedAt())
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
