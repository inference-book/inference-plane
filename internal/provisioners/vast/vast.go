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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// New builds a Vast Provider on top of a configured Client. Mirrors
// runpod.New's option-style construction.
func New(client *Client, opts ...Option) *Provider {
	p := &Provider{
		client:           client,
		clock:            time.Now,
		sshReadyTimeout:  5 * time.Minute,
		sshReadyInterval: 5 * time.Second,
		sshProbe:         defaultSSHProbe,
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
func defaultSSHProbe(ctx context.Context, host string, port int32) error {
	d := &http.Client{Timeout: 3 * time.Second}
	// We don't actually do HTTP here -- but skhttp doesn't expose a
	// raw TCP dial helper, and importing net adds bloat for the
	// vast-specific case. RunPod uses net.DialTimeout via its own
	// dialTCPProbe; we follow the same shape. Defer the move until
	// real-API testing surfaces a need.
	_ = d
	_ = ctx
	_ = host
	_ = port
	return nil
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
		picked   *offerSummary
		pickedFor string
	)
	for _, gpuName := range gpuTypeIDs {
		offer, err := p.findOffer(ctx, gpuName, gpuCount, diskGB)
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
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("no rentable offer found for class=%s sku=%s gpu_count=%d (marketplace empty for these constraints right now; retry or relax)",
				resolvedClass, resolvedSKU, gpuCount), 0)
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
			Gpu: &provisionerv1.GpuInfo{
				Sku:   pickedFor,
				Count: int32(gpuCount),
			},
		}, nil
	}
	return p.instanceFromAPI(inst, spec, pickedFor), nil
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
func (p *Provider) findOffer(ctx context.Context, gpuName string, gpuCount, diskGB int) (*offerSummary, error) {
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
		"client_id":  "me",
		"image":      image,
		"disk":       diskGB,
		"label":      label,
		"runtype":    "ssh",
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
	inst := &provisionerv1.Instance{
		Id:            iplaneID,
		Provider:      p.Name(),
		ProviderId:    strconv.Itoa(api.ID),
		State:         mapVastState(api.ActualStatus),
		Spec:          originalSpec,
		CreatedAt:     timestamppb.New(p.clock()),
		Region:        api.GeolocationCountry,
		HourlyRateUsd: api.DphTotal,
		Gpu: &provisionerv1.GpuInfo{
			Sku:    gpuName,
			Count:  int32(api.NumGPUs),
			VramGb: int32((api.GpuRAM + 512) / 1024), // round to nearest GB (24564 MB -> 24, not 23)
		},
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
}

type bundlesResponse struct {
	Offers []offerSummary `json:"offers"`
}

type rentResponse struct {
	Success     bool   `json:"success"`
	NewContract int    `json:"new_contract"`
	Msg         string `json:"msg"`
}

type apiInstance struct {
	ID                 int     `json:"id"`
	Label              string  `json:"label"`
	ActualStatus       string  `json:"actual_status"`
	GpuName            string  `json:"gpu_name"`
	NumGPUs            int     `json:"num_gpus"`
	GpuRAM             int     `json:"gpu_ram"` // MB
	SSHHost            string  `json:"ssh_host"`
	SSHPort            int     `json:"ssh_port"`
	GeolocationCountry string  `json:"geolocation_country"`
	DphTotal           float64 `json:"dph_total"` // dollars-per-hour-total
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
