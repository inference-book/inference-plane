// Package vast implements the Provider interface against Vast.ai's
// REST API (https://console.vast.ai/api/). The adapter speaks four
// endpoint families:
//
//   - POST /api/v0/bundles/        search the marketplace for offers
//   - PUT  /api/v0/asks/{offer_id}/  rent a specific offer (Spawn)
//   - GET  /api/v0/instances/{id}    fetch one instance (Describe)
//   - GET  /api/v1/instances/        list operator's instances
//   - DEL  /api/v0/instances/{id}    terminate
//
// Vast.ai is a marketplace, not a fixed-catalog provider. The Spawn
// path is two round-trips: search returns a list of currently-rentable
// offers matching the operator's class/SKU/VRAM constraints; we pick
// the cheapest and rent it. RunPod's single-create-call pattern doesn't
// fit because Vast offers come and go with marketplace availability.
//
// VM-style provisioning. Vast rents you a containerized GPU host with
// SSH access; the engine container is docker-run via iplane's
// sshdocker fallback executor (not a Deployer here). The Instance
// returned by Spawn carries Ssh{} when the offer's machine info is
// already populated; Describe (and WaitForSSHReady, which the Service
// calls in the deploy path) handles the case where ssh_host arrives
// a few seconds later.
//
// Tag stamping. Vast.ai instances have a free-form `label` field; we
// stamp it with the prefix "iplane-<id>" so List filtering by label
// recovers operator-owned instances after a state-file loss. The
// iplane-operator tag lives only in the state file in v0.2 (single-
// operator); multi-operator state revisits with templates.
//
// SSH key management. Vast.ai's marketplace offers carry the operator's
// already-uploaded SSH keys at rent time; the renter does NOT inline
// keys in the rent request the way RunPod does. v0.2 treats this as
// an operator pre-requisite -- the iplane-managed key must be uploaded
// to Vast.ai (via their console or API) before Spawn. A future
// `keyregistrar.go` can automate this once we verify the SSH key
// endpoint shape against the real API.
//
// Verified against the live API on 2026-06 via tests/smoke-vast.
// Wire-format quirks discovered during the smoke run, locked in
// here:
//
//   - Search is GET /api/v0/bundles/ with a `q` URL-encoded JSON
//     parameter. POST returns 200 with empty offers silently --
//     SAME endpoint, different method = no error, just no results.
//   - The filter dict goes INSIDE q: `?q={"gpu_name":{"eq":"RTX 4090"},...}`.
//   - GPU name in the search filter uses the space form ("RTX 4090"),
//     NOT the underscored token Vast.ai's older docs sometimes show.
//     The adapter normalizes at the boundary via gpuNameForVast.
//   - Boolean filters (rentable, verified) require the {"eq": true}
//     operator form. Bare booleans return 400 "Input should be a
//     valid dictionary".
//   - The `verified` filter excludes ~all community offers (the
//     cheap RTX tier); omitted from the default filter.
package vast

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// Defaults for fields the operator can override via spec or future
// flags. instanceLabelPrefix is the only iplane-managed tag stamped
// onto Vast instances; List filtering uses it to find operator-owned
// instances on the marketplace.
const (
	instanceLabelPrefix = "iplane-"

	// defaultImage is the base docker image the rented instance boots
	// into. Vast.ai's rent API requires an image; the sshdocker
	// executor will docker-run the engine container on top of it.
	// PyTorch + CUDA base matches RunPod's default so the executor's
	// expectations carry across providers.
	defaultImage = "pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime"

	// defaultDiskGB is the on-instance disk size if the operator
	// didn't specify a requirement. 40 GB covers the base image + a
	// small model; chapter narratives that need larger should override
	// via min_disk_gb.
	defaultDiskGB = 40
)

// Provider implements provisioners.Provider for Vast.ai.
type Provider struct {
	client *Client
	clock  func() time.Time

	// sshReadyTimeout / sshReadyInterval bound the WaitForSSHReady
	// poll. Vast.ai instances typically have ssh_host populated within
	// 60-120s of rent, depending on the host's container pull time.
	// 5 min is comfortable. Tests inject shorter values.
	sshReadyTimeout  time.Duration
	sshReadyInterval time.Duration

	// sshProbe verifies tcp/22 is actually accepting connections after
	// the host info is populated. Default: net.DialTimeout-based probe
	// (mirrors RunPod's dialTCPProbe). Tests inject a no-op.
	sshProbe func(ctx context.Context, host string, port int32) error

	// engineReadyTimeout bounds the wait for a deployed engine to answer
	// /health. Separate from the SSH wait because it covers the image pull
	// and the model load, which a large model dominates.
	engineReadyTimeout time.Duration

	// minInetDownMbps / minReliability are the marketplace-quality floors
	// applied to every offer search. 0 disables that floor. See
	// WithHostQualityFloor for why they default to non-zero.
	minInetDownMbps float64
	minReliability  float64
}

// Marketplace-quality floors, applied to every offer search unless
// WithHostQualityFloor overrides them.
//
// Vast is a marketplace of independent hosts, so "cheapest rentable offer"
// selects for hosts that are cheap for a reason. Ordering by dph_total with no
// quality floor picked a host whose model download stalled for 30 minutes on a
// 0.5B, and a rental that cannot pull its weights costs more than the price
// difference that chose it.
//
// Measured against the live marketplace on 2026-08-11, RTX 3090, single GPU:
//
//	price-only cheapest:  $0.0684/hr, inet_down 357 Mbps, reliability2 0.9558
//	with these floors:    $0.0763/hr, inet_down 1009 Mbps, reliability2 0.9878
//
// So the floors cost about 12% per hour and buy roughly 3x the download
// bandwidth. Ordering stays cheapest-first; these only bound which hosts are
// eligible to be cheapest.
const (
	// Exported so the CLI can start from these when only one of the two env
	// overrides is set, rather than restating the numbers and letting the two
	// copies drift.
	DefaultMinInetDownMbps = 1000
	DefaultMinReliability  = 0.98
)

// WithHostQualityFloor overrides the marketplace-quality floors used when
// searching for offers. inet_down is megabits per second as Vast reports it;
// reliability2 is Vast's host uptime score in [0,1].
//
// Either value at or below 0 disables that floor, which is the escape hatch
// for a search that legitimately returns nothing: on thin capacity a lower
// floor may beat no host at all. That is an operator's call to make
// deliberately, not a fallback the adapter takes on its own, because a silent
// downgrade to a slow host reproduces the failure the floors exist to prevent.
func WithHostQualityFloor(minInetDownMbps, minReliability float64) Option {
	return func(p *Provider) {
		p.minInetDownMbps = max(minInetDownMbps, 0)
		p.minReliability = max(minReliability, 0)
	}
}

// WithEngineReadyTimeout overrides how long Deploy waits for the engine to
// serve. Mirrors runpod.WithEngineReadyTimeout.
func WithEngineReadyTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.engineReadyTimeout = d
		}
	}
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithSSHReadyWait overrides the WaitForSSHReady poll deadline and
// interval. Mirrors runpod.WithSSHReadyWait's shape; tests use this
// to keep wait loops fast.
func WithSSHReadyWait(timeout, interval time.Duration) Option {
	return func(p *Provider) {
		p.sshReadyTimeout = timeout
		p.sshReadyInterval = interval
	}
}

// WithSSHProbe overrides the tcp/22 reachability probe used after
// the host info is populated. Tests pass a no-op.
func WithSSHProbe(probe func(ctx context.Context, host string, port int32) error) Option {
	return func(p *Provider) { p.sshProbe = probe }
}

// defaultSSHReadyTimeout is how long to wait for a rented machine to accept
// SSH.
//
// Measured rather than guessed: on the cheapest available box, TCP first
// accepted 273 seconds after the rent call returned. The previous 5 minutes
// left 27 seconds of headroom over that single observation, which is not
// margin so much as luck, and a larger multi-GPU host has more to do before
// sshd answers.
//
// Erring long is cheap here. The wait ends as soon as the port answers, so
// the only cost of a generous ceiling is how long a genuinely dead machine
// takes to be called dead.
const defaultSSHReadyTimeout = 12 * time.Minute

// New builds a Vast Provider on top of a configured Client. Mirrors
// runpod.New's option-style construction.
func New(client *Client, opts ...Option) *Provider {
	p := &Provider{
		client:             client,
		clock:              time.Now,
		sshReadyTimeout:    defaultSSHReadyTimeout,
		sshReadyInterval:   5 * time.Second,
		sshProbe:           defaultSSHProbe,
		engineReadyTimeout: defaultEngineReadyTimeout,
		minInetDownMbps:    DefaultMinInetDownMbps,
		minReliability:     DefaultMinReliability,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// defaultSSHProbe is the production sshProbe: open a TCP connection
// to host:port with a tight timeout and close it. A successful dial
// means sshd accepted the SYN; the actual SSH handshake happens later
// in the deployment executor.
// It dials. That sentence should be unremarkable, and is worth writing down
// because this function previously did not: it took the arguments, discarded
// them, and returned nil. It satisfied the type, it was called on the right
// path, and it reported every address as reachable.
//
// The cost of that was not a missing feature but a misleading one. Callers
// treated "probe passed" as evidence, so WaitForSSHReady returned instantly
// against machines that were not up, and the failure surfaced minutes later
// in whatever dialled next. Measured on a rented box: Vast publishes the SSH
// endpoint about 3 seconds after the rent call and the port does not accept
// until roughly 273 seconds, so the stub was wrong for four and a half
// minutes of every rental.
func defaultSSHProbe(ctx context.Context, host string, port int32) error {
	if port <= 0 {
		port = 22
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	return conn.Close()
}

// Name satisfies provisioners.Provider.
func (p *Provider) Name() string { return provisioners.ProviderVast }

// IsActiveProviderState satisfies provisioners.ActiveStateChecker.
// Delegates to the adapter-local mapping in skus.go.
func (p *Provider) IsActiveProviderState(state string) bool {
	return isActiveProviderState(state)
}

// Spawn rents a Vast.ai offer matching the operator's requirements.
// Sequence:
//
//  1. Resolve requirements -> ordered SKU list (skus.MatchSKUs).
//     Operator-supplied --gpu-sku bypasses the catalog.
//  2. Search /api/v0/bundles/ for rentable offers matching the SKU,
//     ordered cheapest-first. We try SKU[0] first; if no offers,
//     try SKU[1]; ... up to MaxSKUsPerRequest.
//  3. Pick the cheapest offer that matches.
//  4. Rent the offer via PUT /api/v0/asks/{offer_id}/.
//  5. Return Instance with provider_id = the contract id, state =
//     ACTIVE (Vast's "scheduling" state, which IsActiveProviderState
//     treats as adoptable). Ssh{} populated when host info already
//     present in the rent response; otherwise empty -- the Service
//     calls WaitForSSHReady before the executor SSHes in.
//
// Idempotency. The Service's pre-Spawn List filter checks for an
// existing instance with label="iplane-<id>"; we don't re-check here.
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}
	reqs := spec.GetRequirements()
	if reqs == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("requirements is required"), 0)
	}

	// Resolve SKU candidate list (cheapest-first, capped).
	var gpuTypeIDs []string
	resolvedSKU := reqs.GetSku()
	resolvedClass := reqs.GetClass()
	switch {
	case resolvedSKU != "":
		gpuTypeIDs = []string{normalizeGpuName(resolvedSKU)}
		if resolvedClass == "" {
			resolvedClass = classifySKU(resolvedSKU)
		}
	default:
		gpuTypeIDs = MatchSKUs(reqs)
		if len(gpuTypeIDs) == 0 {
			return nil, provisioners.NewProviderError(p.Name(), "spawn",
				fmt.Errorf("no SKU in the vast catalog satisfies min_vram_gb=%d min_disk_gb=%d min_ram_gb=%d",
					reqs.GetMinVramGb(), reqs.GetMinDiskGb(), reqs.GetMinRamGb()), 0)
		}
		resolvedSKU = gpuTypeIDs[0]
		if resolvedClass == "" {
			resolvedClass = classifySKU(resolvedSKU)
		}
	}

	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := int(reqs.GetMinDiskGb())
	if diskGB <= 0 {
		diskGB = defaultDiskGB
	}
	image := spec.GetBaseImage()
	if image == "" {
		image = defaultImage
	}
	label := instanceLabelPrefix + spec.GetId()

	// Search-then-rent loop: try the SKU list in order until we find
	// an offer to rent.
	var (
		picked    *offerSummary
		pickedFor string
	)
	for _, gpuName := range gpuTypeIDs {
		offer, err := p.findOffer(ctx, gpuName, gpuCount, diskGB, reqs)
		if err != nil {
			return nil, wrapErr("spawn:search", err)
		}
		if offer != nil {
			picked = offer
			pickedFor = gpuName
			break
		}
	}
	if picked == nil {
		// Name the quality floors in the message. They are the one constraint
		// the operator did not type, so an empty result reads as "no capacity"
		// when it may well be "capacity exists, all of it below the floor".
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("no rentable offer found for class=%s sku=%s gpu_count=%d (search also required inet_down>=%gMbps reliability2>=%g; retry, relax the constraints, or lower the floors via IPLANE_VAST_MIN_INET_DOWN_MBPS / IPLANE_VAST_MIN_RELIABILITY)",
				resolvedClass, resolvedSKU, gpuCount, p.minInetDownMbps, p.minReliability), 0)
	}

	rented, err := p.rentOffer(ctx, picked.ID, image, label, diskGB)
	if err != nil {
		return nil, wrapErr("spawn:rent", err)
	}
	contractID := rented.NewContract
	if contractID == 0 {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("rent response did not include new_contract id"), 0)
	}

	// Pull the full instance record so the returned Instance has
	// machine info, ssh_host (when assigned), provider state. The
	// rent response carries only `success` + `new_contract`.
	inst, derr := p.describeContract(ctx, contractID)
	if derr != nil {
		// Rent succeeded but describe failed -- we have a live contract
		// at the provider. Return an Instance carrying the contract id
		// so the Service can record it; the operator's destroy path
		// will still terminate at the provider.
		return &provisionerv1.Instance{
			Id:         spec.GetId(),
			Provider:   p.Name(),
			ProviderId: strconv.Itoa(contractID),
			Spec:       spec,
			State:      provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			Region:     spec.GetRegion(),
			CreatedAt:  timestamppb.New(p.clock()),
			Hardware: func() *provisionerv1.Hardware {
				hw := &provisionerv1.Hardware{
					GpuSku:   pickedFor,
					GpuCount: int32(gpuCount),
				}
				stampFabric(hw, pickedFor, picked.BwNvlink)
				return hw
			}(),
		}, nil
	}
	out := p.instanceFromAPI(inst, spec, pickedFor)
	// Prefer the OFFER's reading over the rented record's. The offer is what
	// the marketplace filter matched and it always carries bw_nvlink; the
	// instance record is not guaranteed to echo it, and falling back to the
	// declared tier there would downgrade a host we just measured to UNKNOWN.
	if picked.BwNvlink != nil {
		stampFabric(out.GetHardware(), pickedFor, picked.BwNvlink)
	}
	return out, nil
}

// Terminate deletes a rented Vast.ai instance via DELETE /api/v0/instances/{id}.
// 404 surfaces as ErrNotFound (the Service treats not-found as success
// for terminate -- the desired end state matches).
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if providerID == "" {
		return provisioners.NewProviderError(p.Name(), "terminate",
			fmt.Errorf("provider id is required"), 0)
	}
	path := pathInstancesV0 + providerID + "/"
	req, err := p.client.newReq(http.MethodDelete, path, nil, nil)
	if err != nil {
		return wrapErr("terminate", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		return wrapErr("terminate", err)
	}
	return nil
}

// Describe fetches one instance via GET /api/v0/instances/{id} and
// renders it as a provisionerv1.Instance. 404 surfaces as ErrNotFound.
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "describe",
			fmt.Errorf("provider id is required"), 0)
	}
	id, perr := strconv.Atoi(providerID)
	if perr != nil {
		return nil, provisioners.NewProviderError(p.Name(), "describe",
			fmt.Errorf("provider id %q is not numeric: %v", providerID, perr), 0)
	}
	api, derr := p.describeContract(ctx, id)
	if derr != nil {
		return nil, wrapErr("describe", derr)
	}
	return p.instanceFromAPI(api, nil, api.GpuName), nil
}

// List returns the operator's currently-running instances. Filter
// keys honored:
//   - "label-prefix" -> server-side filter for instances whose label
//     starts with this prefix. The Service uses "iplane-" to scope.
//
// Vast.ai's GET /api/v1/instances/ returns ALL instances on the
// operator's account; we filter the response by label-prefix
// client-side because the API's filter shape doesn't accept arbitrary
// label prefixes (it accepts exact labels).
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	req, err := p.client.newReq(http.MethodGet, pathInstancesV1, nil, nil)
	if err != nil {
		return nil, wrapErr("list", err)
	}
	body, err := skhttp.Call[instanceListResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("list", err)
	}

	prefix := filter["label-prefix"]
	out := make([]*provisionerv1.InstanceRef, 0, len(body.Instances))
	for i := range body.Instances {
		a := &body.Instances[i]
		if prefix != "" && !strings.HasPrefix(a.Label, prefix) {
			continue
		}
		// Strip the iplane- prefix to recover the iplane Instance id.
		// InstanceRef carries ProviderId + ProviderState (raw); the
		// Service maps ProviderState -> InstanceState via the
		// IsActiveProviderState callback and the iplane Instance id
		// via Tags["iplane-id"]. We stamp both.
		iplaneID := strings.TrimPrefix(a.Label, instanceLabelPrefix)
		out = append(out, &provisionerv1.InstanceRef{
			ProviderId:    strconv.Itoa(a.ID),
			ProviderState: a.ActualStatus,
			Tags: map[string]string{
				provisioners.TagID: iplaneID,
			},
		})
	}
	return out, nil
}

// findOffer searches /api/v0/bundles/ for the cheapest rentable
// offer matching the gpu_name + gpu_count + disk constraints.
// Returns nil (not an error) when no offer matched.
//
// Wire shape (verified by real-API smoke 2026-06):
//
//   - Method: GET (NOT POST -- POST returns 200 with empty offers,
//     silently dropping the filter).
//   - Query param: `q` carrying a URL-encoded JSON object. The
//     filter dict goes INSIDE q, not at top level.
//   - GPU name uses the space form ("RTX 4090") -- the underscored
//     form Vast.ai's docs reference ("RTX_4090") returns empty.
//   - Each constraint is an operator object: `{"eq": value}`,
//     `{"gte": value}`. Bare bool/string at top of a field returns
//     400 "Input should be a valid dictionary".
//   - `verified` filter excludes most of the marketplace (community
//     hosts) so we omit it; operators who specifically want vetted
//     hosts can future-proof via a knob.
//
// SKU catalog stores the underscored form for stable Go-identifier
// hygiene; we transform back at the wire boundary via
// gpuNameForVast.
// offerVRAMFloorMB returns the per-GPU memory an offer must have, in the
// megabytes Vast reports, or 0 when nothing constrains it.
//
// Takes the larger of what the operator asked for and what the named SKU is
// documented to have, because both are real constraints: asking for a SKU is
// asking for that card, not for something sharing its marketing name.
func offerVRAMFloorMB(gpuName string, reqs *provisionerv1.ResourceRequirements) int {
	want := int(reqs.GetMinVramGb())
	if spec := LookupSKU(gpuName); spec != nil && int(spec.VRAMGb) > want {
		want = int(spec.VRAMGb)
	}
	if want <= 0 {
		return 0
	}
	// 3% under the nominal figure, so a card advertised as 80 GB still
	// matches when the host reports 81251 MB rather than a round 81920.
	return want * 970
}

func (p *Provider) findOffer(ctx context.Context, gpuName string, gpuCount, diskGB int, reqs *provisionerv1.ResourceRequirements) (*offerSummary, error) {
	q := map[string]any{
		"gpu_name": map[string]string{"eq": gpuNameForVast(gpuName)},
		"num_gpus": map[string]int{"eq": gpuCount},
		"rentable": map[string]bool{"eq": true},
		"limit":    5,
		"order":    [][]string{{"dph_total", "asc"}},
	}
	if diskGB > 0 {
		q["disk_space"] = map[string]int{"gte": diskGB}
	}
	// VRAM floor, also pushed server-side.
	//
	// Vast lists several physically different cards under one gpu_name: an
	// "A100 PCIE" offer may be the 40 GB part or the 80 GB part, and the
	// marketplace happily returns the cheaper 40 GB one first because the
	// results are ordered by price. Without this the resolver would rent
	// half the VRAM the catalog claims for that SKU, and a model sized
	// against the catalog would OOM on arrival with nothing in the
	// deployment record hinting why.
	//
	// The floor comes from the resolved SKU's catalog entry, so naming a
	// SKU implies its advertised memory, and an explicit min_vram_gb raises
	// it further. Vast reports gpu_ram in MB. The 1000 rather than 1024
	// conversion plus a small margin absorbs the vendors who report 81920
	// and the ones who report 81251.
	if floor := offerVRAMFloorMB(gpuName, reqs); floor > 0 {
		q["gpu_ram"] = map[string]int{"gte": floor}
	}
	// Fabric filter, pushed server-side. Vast's query language filters on
	// bw_nvlink directly, so the marketplace does the narrowing and we never
	// page through hosts that cannot satisfy the request.
	//
	// The floor is at least 1 GB/s rather than 0, because a filter of >= 0
	// matches every host including the unmeasured ones and would silently
	// undo the whole point. Values are gigaBYTES here: Vast's unit, not ours.
	if reqs.GetFabricScope() == provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE {
		minGBps := 1
		if want := int(reqs.GetMinFabricGbps()); want > 0 {
			minGBps = want / 8
			if minGBps < 1 {
				minGBps = 1
			}
		}
		q["bw_nvlink"] = map[string]int{"gte": minGBps}
	}
	// The other direction: the operator wants a host with NO intra-node
	// fabric. Ch 10's A/B control arm is exactly this request, and until now
	// it could not be made at all.
	//
	// It is not the same as leaving fabric_scope unset. UNSPECIFIED means "do
	// not care" and admits anything; NONE means "must not have one", and the
	// difference decides whether an experiment is valid. Vast lists
	// bridge-capable cards under PCIe names -- machine 6566 was an "A100 PCIE"
	// reporting 300 GB/s on 2026-08-11 -- so a control arm chosen without this
	// filter can silently contain NVLink and make the A/B compare NVLink
	// against NVLink.
	//
	// What this does and does not guarantee, because the gap matters. It
	// excludes every host with a POSITIVE reading, which is the observed
	// contamination. It cannot promise the absence of a link, because Vast
	// reports 0 both for "no link" and for "never measured": the same probe
	// that found the bridged PCIe hosts also found roughly a quarter of SXM
	// machines reporting zero on boards that are physically always NVLinked.
	// So this is "no measured fabric", not "provably none", and the resolved
	// Hardware keeps FABRIC_SOURCE_UNKNOWN on a bridge-capable card to say so
	// rather than claiming a certainty the data does not support. Settling it
	// for real needs an on-box reading (issue #213).
	if reqs.GetFabricScope() == provisionerv1.FabricScope_FABRIC_SCOPE_NONE {
		q["bw_nvlink"] = map[string]int{"lte": 0}
	}
	// Marketplace-quality floors, pushed server-side alongside the rest. Both
	// are Vast marketplace columns rather than workload requirements, which is
	// why they live here and not on ResourceRequirements: no caller wants a
	// slow, flaky host, so there is nothing for an operator to express.
	if p.minInetDownMbps > 0 {
		q["inet_down"] = map[string]float64{"gte": p.minInetDownMbps}
	}
	if p.minReliability > 0 {
		q["reliability2"] = map[string]float64{"gte": p.minReliability}
	}
	qBytes, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("encode q: %w", err)
	}
	params := url.Values{}
	params.Set("q", string(qBytes))
	req, err := p.client.newReq(http.MethodGet, pathBundles, params, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[bundlesResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	if len(resp.Offers) == 0 {
		return nil, nil
	}
	return &resp.Offers[0], nil
}

// gpuNameForVast converts the underscored SKU token used in our
// catalog ("RTX_4090") into the space-form gpu_name Vast.ai's API
// filter expects ("RTX 4090"). Verified via smoke: passing the
// underscored form to the bundles search returns 0 offers; passing
// the space form returns the full set.
func gpuNameForVast(gpuName string) string {
	return strings.ReplaceAll(gpuName, "_", " ")
}

// rentOffer PUTs to /api/v0/asks/{offer_id}/ with a rent config and
// returns Vast.ai's rent response (success bool + new_contract id).
func (p *Provider) rentOffer(ctx context.Context, offerID int, image, label string, diskGB int) (*rentResponse, error) {
	body := map[string]any{
		"client_id": "me",
		"image":     image,
		"disk":      diskGB,
		"label":     label,
		"runtype":   "ssh",
		// onstart_cmd is left empty so the engine container is started
		// later by sshdocker. Vast's default is to drop into the image
		// entrypoint; we rely on sshd being present in the image.
	}
	path := pathAskPrefix + strconv.Itoa(offerID) + "/"
	req, err := p.client.newReq(http.MethodPut, path, nil, body)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[rentResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("rent failed: %s", resp.Msg)
	}
	return &resp, nil
}

// describeContract fetches one instance record by its contract id
// (Vast.ai's terminology for the rent id). The response is the same
// shape as one element of the List response.
func (p *Provider) describeContract(ctx context.Context, id int) (*apiInstance, error) {
	path := pathInstancesV0 + strconv.Itoa(id) + "/"
	req, err := p.client.newReq(http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[instanceResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	return &resp.Instances, nil
}

// instanceFromAPI renders a Vast.ai instance record into the iplane
// Instance shape. originalSpec carries the operator's iplane-side
// view (id, image, requirements); when nil (the Describe-by-pid
// path) we don't have it and leave Spec empty.
//
// gpuName is the SKU id we PICKED at rent time (RunPod's catalog
// token); Vast's `gpu_name` field on the API record is the human-
// readable display name. We use the picked name when populating
// Gpu.Sku so the iplane state reflects the operator's intent.
func (p *Provider) instanceFromAPI(api *apiInstance, originalSpec *provisionerv1.Spec, gpuName string) *provisionerv1.Instance {
	iplaneID := strings.TrimPrefix(api.Label, instanceLabelPrefix)
	if originalSpec != nil && originalSpec.GetId() != "" {
		iplaneID = originalSpec.GetId()
	}
	// disk_space comes back from Vast in GB (float, host's total
	// disk-for-rent at the offer). Convert to MB for the
	// Hardware.disk_mb uniform unit. Vast.ai's response calls the
	// per-GPU VRAM `gpu_ram` and reports MB directly -- no
	// conversion needed.
	diskMB := int32(api.DiskSpace * 1024)
	inst := &provisionerv1.Instance{
		Id:            iplaneID,
		Provider:      p.Name(),
		ProviderId:    strconv.Itoa(api.ID),
		State:         mapVastState(api.ActualStatus),
		Spec:          originalSpec,
		CreatedAt:     timestamppb.New(p.clock()),
		Region:        api.GeolocationCountry,
		HourlyRateUsd: api.DphTotal,
		Hardware: func() *provisionerv1.Hardware {
			hw := &provisionerv1.Hardware{
				GpuSku:    gpuName,
				GpuCount:  int32(api.NumGPUs),
				GpuVramMb: int32(api.GpuRAM),
				Vcpus:     int32(api.CPUCores),
				CpuModel:  api.CPUName,
				CpuRamMb:  int32(api.CPURam),
				DiskMb:    diskMB,
			}
			stampFabric(hw, gpuName, api.BwNvlink)
			return hw
		}(),
		Metadata: vastMetadata(api),
	}
	if api.SSHHost != "" {
		inst.Ssh = &provisionerv1.SshTarget{
			Host: api.SSHHost,
			Port: int32(api.SSHPort),
			User: "root",
		}
	}
	return inst
}

// vastMetadata captures the Vast-specific fields outside the
// Hardware base, with type fidelity preserved via google.protobuf.Value.
// Empty / zero values are dropped so describe output isn't cluttered.
func vastMetadata(api *apiInstance) map[string]*structpb.Value {
	out := map[string]*structpb.Value{}
	if api.Geolocation != "" {
		out["vast.geolocation"] = structpb.NewStringValue(api.Geolocation)
	}
	if api.HostID > 0 {
		out["vast.host_id"] = structpb.NewNumberValue(float64(api.HostID))
	}
	if api.MachineID > 0 {
		out["vast.machine_id"] = structpb.NewNumberValue(float64(api.MachineID))
	}
	if api.Reliability2 > 0 {
		out["vast.reliability2"] = structpb.NewNumberValue(api.Reliability2)
	}
	if api.Verification != "" {
		out["vast.verification"] = structpb.NewStringValue(api.Verification)
	}
	if api.IsBid {
		out["vast.is_bid"] = structpb.NewBoolValue(true)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapVastState translates Vast.ai's actual_status enum into the
// iplane InstanceState enum. Vast values seen in the docs/observed:
//
//	"scheduling"  -> PENDING (rented, host assigning)
//	"loading"     -> PENDING (image pulling)
//	"running"     -> ACTIVE (container up; SSH reachable if host info present)
//	"stopped"     -> ACTIVE (paused but contract intact)
//	"exited"      -> TERMINATED
//	"offline"     -> FAILED (host unreachable)
//	"created"     -> PENDING
//
// Unknown values default to PENDING (conservative: treat as not-yet-
// active rather than ACTIVE-by-default).
func mapVastState(actualStatus string) provisionerv1.InstanceState {
	switch strings.ToLower(strings.TrimSpace(actualStatus)) {
	case "running", "stopped":
		return provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE
	case "scheduling", "loading", "created", "":
		return provisionerv1.InstanceState_INSTANCE_STATE_PENDING
	case "exited", "terminated":
		return provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED
	case "offline", "failed":
		return provisionerv1.InstanceState_INSTANCE_STATE_FAILED
	default:
		return provisionerv1.InstanceState_INSTANCE_STATE_PENDING
	}
}

// API response shapes. Field names verified against live API in
// the 2026-06 smoke run -- offers come back with `gpu_name`,
// `num_gpus`, `dph_total`, `disk_space` and instance records use
// `ssh_host` / `ssh_port` / `actual_status` / `geolocation_country`.

type offerSummary struct {
	ID       int     `json:"id"`
	GpuName  string  `json:"gpu_name"`
	NumGPUs  int     `json:"num_gpus"`
	DiskGB   float64 `json:"disk_space"`
	DphTotal float64 `json:"dph_total"`

	// BwNvlink is Vast's measured NVLink bandwidth for the host, in
	// gigaBYTES per second. Vast is the only provider that reports this.
	//
	// Pointer, not float64, and the distinction is load-bearing: Vast sends
	// 0.0 both for "this host has no NVLink" and for "we never measured it",
	// and a plain float cannot tell an absent field from a real zero. nil
	// means no reading at all, so the fabric catalog arbitrates instead of
	// a zero being trusted as fact. In the 2026-08-09 probe, 9 of 38
	// "A100 SXM4" offers reported zero on a board that always has NVLink.
	BwNvlink *float64 `json:"bw_nvlink"`

	// InetDown (Mbps) and Reliability2 ([0,1]) are the two marketplace-quality
	// columns findOffer filters on. Decoded rather than ignored so a returned
	// offer can be checked against the floor that was supposed to exclude it:
	// a filter the marketplace silently stops honouring looks identical to one
	// that works until someone reads the value that came back.
	InetDown     float64 `json:"inet_down"`
	Reliability2 float64 `json:"reliability2"`
}

type bundlesResponse struct {
	Offers []offerSummary `json:"offers"`
}

type rentResponse struct {
	Success     bool   `json:"success"`
	NewContract int    `json:"new_contract"`
	Msg         string `json:"msg"`
}

// apiPortBind is one docker port mapping as Vast reports it: the host side
// of a container port. Vast keys the map by the container port in docker's
// "8000/tcp" form.
type apiPortBind struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type apiInstance struct {
	ID           int    `json:"id"`
	Label        string `json:"label"`
	ActualStatus string `json:"actual_status"`
	GpuName      string `json:"gpu_name"`
	NumGPUs      int    `json:"num_gpus"`
	GpuRAM       int    `json:"gpu_ram"` // MB per GPU
	SSHHost      string `json:"ssh_host"`
	SSHPort      int    `json:"ssh_port"`
	// PublicIPAddr and Ports are how an engine becomes reachable. Vast has
	// no proxy URL equivalent to RunPod's <pod>-<port>.proxy.runpod.net, so
	// the endpoint is the host's public address plus whichever high port
	// docker mapped the engine's container port onto. Both are empty until
	// the container is running, which is why the deployer polls for them
	// rather than deriving an address up front.
	PublicIPAddr       string                   `json:"public_ipaddr"`
	Ports              map[string][]apiPortBind `json:"ports"`
	GeolocationCountry string                   `json:"geolocation_country"`
	Geolocation        string                   `json:"geolocation"`
	DphTotal           float64                  `json:"dph_total"`
	// Host details -- populated by both the bundles search
	// response and the instance record. Used to fill Hardware and
	// metadata.
	CPUName      string   `json:"cpu_name"`
	CPUCores     int      `json:"cpu_cores"`
	CPURam       int      `json:"cpu_ram"`    // MB
	DiskSpace    float64  `json:"disk_space"` // GB
	HostID       int      `json:"host_id"`
	MachineID    int      `json:"machine_id"`
	BwNvlink     *float64 `json:"bw_nvlink"` // see offerSummary.BwNvlink
	Reliability2 float64  `json:"reliability2"`
	Verification string   `json:"verification"`
	IsBid        bool     `json:"is_bid"`
}

type instanceResponse struct {
	Instances apiInstance `json:"instances"`
}

type instanceListResponse struct {
	Instances []apiInstance `json:"instances"`
}

// Ensure unused imports above (encoding/json, net/url) compile -- they
// are reserved for future endpoint surface (query-string filters on
// list, alternate JSON encoders for error bodies). When the first
// real-API run lands, prune anything that's still unused.
var _ = json.Marshal
var _ = url.QueryEscape

// WaitForSSHReady satisfies provisioners.SSHReadyWaiter.
//
// Vast returns from the rent call before the machine is reachable: the
// contract exists, but ssh_host and ssh_port are empty until the host has
// pulled the image and started sshd, which takes tens of seconds to a couple
// of minutes. Anything that dials in that window finds no endpoint at all.
//
// The scaffolding for this poll (the timeout, the interval, the TCP probe,
// their options and defaults) was present from the start; only the method
// was missing, so every caller silently got no wait. `iplane instance wait`
// reported "already ready" against a machine with no SSH endpoint, and the
// deploy path dialled into nothing.
//
// Two conditions, not one. The record must carry a host and port, and the
// port must actually accept a connection. Vast publishes the endpoint a few
// seconds before sshd answers on it, so returning on the record alone hands
// the caller an address that refuses the next connection.
func (p *Provider) WaitForSSHReady(ctx context.Context, providerID string) (*provisionerv1.SshTarget, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
			fmt.Errorf("providerID is empty"), 0)
	}
	id, err := strconv.Atoi(providerID)
	if err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
			fmt.Errorf("provider id %q is not a vast contract id: %w", providerID, err), 0)
	}

	timeout := p.sshReadyTimeout
	if timeout <= 0 {
		// Always allow one lookup, so a test that disables polling still
		// gets a best-effort answer rather than an immediate error.
		timeout = time.Second
	}
	interval := p.sshReadyInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	// Deadline on the context rather than arithmetic over p.clock(). The
	// clock is injectable and tests hold it fixed, which would make a
	// clock-based deadline unreachable and this loop run forever. Wall time
	// is also the honest measure here: the wait is against a remote machine
	// booting, not against anything the caller simulates.
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last error
	for {
		api, derr := p.describeContract(waitCtx, id)
		switch {
		case derr != nil:
			// A transient read failure is not a verdict; keep polling until
			// the deadline and report the last error if it never clears.
			last = derr
		case api.SSHHost != "":
			target := &provisionerv1.SshTarget{
				Host: api.SSHHost,
				Port: int32(api.SSHPort),
				User: "root",
			}
			if p.sshProbe == nil {
				return target, nil
			}
			if perr := p.sshProbe(waitCtx, target.Host, target.Port); perr == nil {
				return target, nil
			} else {
				last = perr
			}
		default:
			last = fmt.Errorf("ssh_host not yet published")
		}

		select {
		case <-waitCtx.Done():
			// Distinguish our own deadline from the caller giving up
			// first. Reporting "within 12m" after 90 seconds sends the
			// reader looking for a provider problem when the real answer
			// is that their client timeout is shorter than a boot.
			waited := timeout
			if ctx.Err() != nil {
				return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
					fmt.Errorf("caller stopped waiting for instance %s before it became reachable "+
						"(provider budget was %s); last attempt: %w", providerID, waited, last), 0)
			}
			return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("instance %s had no reachable ssh endpoint within %s: %w", providerID, waited, last), 0)
		case <-time.After(interval):
		}
	}
}
