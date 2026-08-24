// Package lambdalabs implements the Provider interface against
// Lambda Labs Cloud's REST API (https://cloud.lambdalabs.com/api/v1).
// Verified against the live API on 2026-06; wire-format quirks
// captured in code comments rather than docstrings about what the
// API "should" be.
//
// Endpoints used:
//
//   - GET  /api/v1/instances                         list
//   - GET  /api/v1/instances/{id}                    describe
//   - POST /api/v1/instance-operations/launch        rent (Spawn)
//   - POST /api/v1/instance-operations/terminate     terminate
//   - GET  /api/v1/ssh-keys                          list keys (KeyRegistrar)
//   - POST /api/v1/ssh-keys                          add key (KeyRegistrar)
//
// VM-style provisioning. Lambda rents you a GPU VM with SSH access;
// iplane's sshdocker fallback executor docker-runs the engine
// container on top. Not image-native (no Deployer here).
//
// Auth uses HTTP Basic with the API key as the username and an
// empty password -- verified via probe. NOT a Bearer token (RunPod
// and Vast both use Bearer; Lambda is the outlier in the v0.2
// catalog).
//
// Tag stamping. Lambda instances carry a free-form `name` field
// (operator-supplied at launch time). We stamp it with the prefix
// "iplane-<id>" so List filtering recovers operator-owned instances
// after a state-file loss.
//
// SSH key model. Lambda's API has first-class SSH key management:
// keys live as named objects with their own endpoint, and Spawn
// references them by name (`ssh_key_names: ["iplane-<operator>-
// lambdalabs"]`). keyregistrar.go uploads iplane's own key under a
// name derived from the key comment, and Spawn prefers that name over
// anything else on the account. See keyregistrar.go for why the two
// halves have to agree.
package lambdalabs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// Defaults that operators can override via spec or future flags.
const (
	// instanceNamePrefix is stamped onto every launched instance's
	// `name` field. List filtering uses it to find operator-owned
	// instances on the account.
	instanceNamePrefix = "iplane-"

	// sshUser is the login on every image Lambda ships.
	sshUser = "ubuntu"

	// defaultRegion is the fallback when spec.region is empty.
	// us-east-1 is one of Lambda's larger regions; capacity varies
	// by instance type, so the smoke test and demos that care
	// should set region explicitly.
	defaultRegion = "us-east-1"
)

// Provider implements provisioners.Provider for Lambda Labs.
type Provider struct {
	client *Client
	clock  func() time.Time

	// SSH readiness gap is shorter on Lambda than on RunPod or
	// Vast -- usually under 60s. 3 min default is comfortable.
	sshReadyTimeout  time.Duration
	sshReadyInterval time.Duration
	sshProbe         func(ctx context.Context, host string, port int32) error
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithSSHReadyWait overrides the WaitForSSHReady poll deadline +
// interval. Tests inject short values to keep them fast.
func WithSSHReadyWait(timeout, interval time.Duration) Option {
	return func(p *Provider) {
		p.sshReadyTimeout = timeout
		p.sshReadyInterval = interval
	}
}

// WithSSHProbe overrides the tcp/22 reachability probe. Tests
// pass a no-op so they don't need a real listener.
func WithSSHProbe(probe func(ctx context.Context, host string, port int32) error) Option {
	return func(p *Provider) { p.sshProbe = probe }
}

// New builds a Lambda Labs Provider on top of a configured Client.
// Mirrors runpod.New / vast.New's option-style construction.
func New(client *Client, opts ...Option) *Provider {
	p := &Provider{
		client:           client,
		clock:            time.Now,
		sshReadyTimeout:  3 * time.Minute,
		sshReadyInterval: 5 * time.Second,
		sshProbe:         func(_ context.Context, _ string, _ int32) error { return nil },
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name satisfies provisioners.Provider.
func (p *Provider) Name() string { return provisioners.ProviderLambdaLabs }

// IsActiveProviderState satisfies provisioners.ActiveStateChecker.
func (p *Provider) IsActiveProviderState(status string) bool {
	return isActiveProviderState(status)
}

// Spawn rents a Lambda Labs instance. Sequence:
//
//  1. Resolve requirements -> instance_type_name (skus.MatchSKUs).
//     Operator --gpu-sku bypasses the catalog.
//  2. Pick region: spec.Region if set, else defaultRegion. (Lambda's
//     /instance-types tells us which regions have capacity for the
//     chosen SKU, but Spawn passes region explicitly; capacity
//     mismatches surface as a 4xx from the launch API.)
//  3. List existing SSH keys; pick the first one whose name
//     matches our iplane-managed convention. If none exist, error
//     out -- the operator runs `iplane instance create lambdalabs`
//     once after EnsurePublicKey registered the iplane key.
//  4. POST /api/v1/instance-operations/launch with the resolved
//     parameters + `ssh_key_names`.
//  5. Describe the returned instance id for the full record.
//
// Returns an Instance with provider_id = the Lambda instance UUID,
// state = PENDING (Lambda's "booting"), Ssh populated when the API
// has already assigned ip/port.
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}
	reqs := spec.GetRequirements()
	if reqs == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("requirements is required"), 0)
	}

	instanceTypeName := reqs.GetSku()
	resolvedClass := reqs.GetClass()
	if instanceTypeName == "" {
		ids := MatchSKUs(reqs)
		if len(ids) == 0 {
			return nil, provisioners.NewProviderError(p.Name(), "spawn",
				fmt.Errorf("no SKU in the lambdalabs catalog satisfies min_vram_gb=%d gpu_count=%d",
					reqs.GetMinVramGb(), reqs.GetGpuCount()), 0)
		}
		instanceTypeName = ids[0]
		if resolvedClass == "" {
			resolvedClass = classifySKU(instanceTypeName)
		}
	}

	region := spec.GetRegion()
	if region == "" {
		region = defaultRegion
	}

	keyName, err := p.launchSSHKeyName(ctx)
	if err != nil {
		return nil, wrapErr("spawn:ssh-keys", err)
	}
	if keyName == "" {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("no SSH keys registered on this Lambda Labs account; add one via https://cloud.lambdalabs.com/ssh-keys before provisioning"), 0)
	}

	launchBody := map[string]any{
		"region_name":        region,
		"instance_type_name": instanceTypeName,
		"ssh_key_names":      []string{keyName},
		"quantity":           1,
		"name":               instanceNamePrefix + spec.GetId(),
	}
	// Ownership goes on the tags as well as the name. The name is a display
	// field an operator can change from the console, and changing it used to
	// be enough to hide a rented box from List and from the watchdog.
	// The name stamp stays because rentals made before this carry nothing
	// else, and because hack/lambda-watchdog.sh matches on it.
	if tags := launchTags(spec.GetTags()); len(tags) > 0 {
		launchBody["tags"] = tags
	}
	req, err := p.client.newReq(http.MethodPost, pathInstanceLaunch, nil, launchBody)
	if err != nil {
		return nil, wrapErr("spawn:launch", err)
	}
	resp, err := skhttp.Call[launchResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("spawn:launch", err)
	}
	if len(resp.Data.InstanceIDs) == 0 {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("launch response did not include instance ids"), 0)
	}
	instanceID := resp.Data.InstanceIDs[0]

	// Pull the full record (launch returns only ids).
	api, derr := p.describeOne(ctx, instanceID)
	if derr != nil {
		// Launch succeeded but describe failed -- still return a
		// minimal Instance carrying the id so Destroy can clean up.
		return &provisionerv1.Instance{
			Id:         spec.GetId(),
			Provider:   p.Name(),
			ProviderId: instanceID,
			Spec:       spec,
			State:      provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			Region:     region,
			CreatedAt:  timestamppb.New(p.clock()),
			Hardware: func() *provisionerv1.Hardware {
				hw := &provisionerv1.Hardware{GpuSku: instanceTypeName}
				stampFabric(hw, instanceTypeName)
				return hw
			}(),
		}, nil
	}
	return p.instanceFromAPI(api, spec, instanceTypeName), nil
}

// Terminate releases a rented Lambda Labs VM via POST
// /api/v1/instance-operations/terminate with the instance id in
// `instance_ids`.
//
// Idempotent per the Provider contract: an instance Lambda no longer has
// (`global/object-does-not-exist`, HTTP 404) is the end state this call
// exists to reach, so it returns nil. Every other status still surfaces,
// because swallowing a 403 would report a clean teardown over a VM that
// is still billing. The distinction matters more since issue 161: the
// coupled deployment teardown calls Terminate on every replica it owns,
// so a double-terminate is now the routine case rather than the odd one.
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if providerID == "" {
		return provisioners.NewProviderError(p.Name(), "terminate",
			fmt.Errorf("provider id is required"), 0)
	}
	body := map[string]any{
		"instance_ids": []string{providerID},
	}
	req, err := p.client.newReq(http.MethodPost, pathInstanceTerminate, nil, body)
	if err != nil {
		return wrapErr("terminate", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		wrapped := wrapErr("terminate", err)
		if errors.Is(wrapped, provisioners.ErrNotFound) {
			return nil
		}
		return wrapped
	}
	return nil
}

// Describe fetches one instance via GET /api/v1/instances/{id}.
// 404 surfaces as ErrNotFound.
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "describe",
			fmt.Errorf("provider id is required"), 0)
	}
	api, err := p.describeOne(ctx, providerID)
	if err != nil {
		return nil, wrapErr("describe", err)
	}
	return p.instanceFromAPI(api, nil, api.InstanceType.Name), nil
}

// List returns the operator's currently-running instances.
//
// Tag keys are applied match-all over the tags recovered below, which comes
// from two places. Lambda's own `tags` array carries whatever Spawn stamped,
// and the instance `name` still yields an id for a rental made before this
// adapter stamped tags at all. Tags win where both answer, because the name
// is a display field an operator can change from the console and the tags
// are not.
//
// A filter key neither source recovers matches nothing, which is the honest
// answer to a question this adapter cannot answer. Dropping such a key
// instead returned the whole account, and the Service read the length of
// that as evidence about one instance (#431).
//
// Filter keys honored:
//   - "name-prefix" -> client-side filter for instances whose
//     `name` field starts with this prefix. Lambda's list endpoint
//     doesn't accept arbitrary filters, so the filtering is local.
//
// Lambda's GET /api/v1/instances returns ALL instances on the
// account.
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	req, err := p.client.newReq(http.MethodGet, pathInstances, nil, nil)
	if err != nil {
		return nil, wrapErr("list", err)
	}
	resp, err := skhttp.Call[instanceListResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("list", err)
	}

	prefix := filter["name-prefix"]
	out := make([]*provisionerv1.InstanceRef, 0, len(resp.Data))
	for i := range resp.Data {
		a := &resp.Data[i]
		if prefix != "" && !strings.HasPrefix(a.Name, prefix) {
			continue
		}
		// Stamped only when the name actually carried the prefix.
		// TrimPrefix returns the string unchanged when it does not, so a box
		// the operator launched from the console used to come back wearing
		// its own name as an iplane id, and the Service's ownership check
		// reads exactly this tag.
		tags := map[string]string{}
		if id, ok := strings.CutPrefix(a.Name, instanceNamePrefix); ok && id != "" {
			tags[provisioners.TagID] = id
		}
		// Tags win over the name-derived id: they are what the instance was
		// stamped with, and the name may have been changed since.
		for _, tag := range a.Tags {
			tags[tag.Key] = tag.Value
		}
		out = append(out, &provisionerv1.InstanceRef{
			ProviderId:    a.ID,
			ProviderState: a.Status,
			Tags:          tags,
		})
	}
	return provisioners.FilterRefs(out, provisioners.TagsOnly(filter, "name-prefix")), nil
}

// describeOne is the single-instance describe helper, shared by
// Spawn's post-launch fetch and the public Describe.
func (p *Provider) describeOne(ctx context.Context, providerID string) (*apiInstance, error) {
	path := pathInstances + "/" + providerID
	req, err := p.client.newReq(http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[instanceResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// launchSSHKeyName picks the key the launch call attaches to the VM.
//
// iplane's own key wins when it is on the account, because that is the one
// whose private half the deploy path holds. Anything else and the VM boots
// with a key sshdocker cannot present, which is a rental that bills and
// cannot be reached.
//
// The account's first key stays the fallback for an operator driving the
// adapter by hand, who has never run EnsurePublicKey and wants their own
// key attached. Returns "" with a nil error when the account has no keys at
// all; the caller tells that apart from an API failure.
func (p *Provider) launchSSHKeyName(ctx context.Context) (string, error) {
	keys, err := p.listSSHKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if strings.HasPrefix(k.Name, provisioners.ReservedIDPrefix) {
			return k.Name, nil
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	return keys[0].Name, nil
}

// instanceFromAPI renders a Lambda Labs instance record into the
// iplane Instance shape. originalSpec carries the operator's
// iplane-side view (id, image, requirements); nil for the
// Describe-by-pid path.
//
// instanceTypeName is the SKU id we picked at launch time;
// preserved through to the iplane Instance so state reflects
// operator intent.
func (p *Provider) instanceFromAPI(api *apiInstance, originalSpec *provisionerv1.Spec, instanceTypeName string) *provisionerv1.Instance {
	iplaneID := strings.TrimPrefix(api.Name, instanceNamePrefix)
	if originalSpec != nil && originalSpec.GetId() != "" {
		iplaneID = originalSpec.GetId()
	}
	// Lambda's per-instance response doesn't expose per-GPU VRAM
	// directly. Two sources to try:
	//
	//  1. Curated catalog (skus.go).
	//  2. Parsing instance_type.description ("1x A10 (24 GB PCIe)")
	//     for the "(N GB ...)" form, which Lambda's API uses
	//     consistently. Catches non-curated SKUs.
	//
	// First successful match wins; otherwise vram stays 0.
	vramMB := 0
	if sku := LookupSKU(api.InstanceType.Name); sku != nil {
		vramMB = sku.VRAMGb * 1024
	} else if v := parseVRAMFromDescription(api.InstanceType.Description); v > 0 {
		vramMB = v * 1024
	}
	gpuCount := 1
	if api.InstanceType.Specs.GPUs > 0 {
		gpuCount = api.InstanceType.Specs.GPUs
	}
	inst := &provisionerv1.Instance{
		Id:            iplaneID,
		Provider:      p.Name(),
		ProviderId:    api.ID,
		State:         mapLambdaState(api.Status),
		Spec:          originalSpec,
		CreatedAt:     timestamppb.New(p.clock()),
		Region:        api.Region.Name,
		HourlyRateUsd: float64(api.InstanceType.PriceCentsPerHour) / 100.0,
		Hardware: func() *provisionerv1.Hardware {
			hw := &provisionerv1.Hardware{
				GpuSku:    instanceTypeName,
				GpuCount:  int32(gpuCount),
				GpuVramMb: int32(vramMB),
				Vcpus:     int32(api.InstanceType.Specs.VCPUs),
				CpuRamMb:  int32(api.InstanceType.Specs.MemoryGiB * 1024),
				DiskMb:    int32(api.InstanceType.Specs.StorageGiB * 1024),
			}
			stampFabric(hw, instanceTypeName)
			return hw
		}(),
		Metadata: lambdaMetadata(api),
	}
	if api.IP != "" {
		inst.Ssh = sshTargetFor(api.IP)
	}
	return inst
}

// lambdaTagKey is Lambda's own bound on a tag key. A key outside it fails
// the launch with a 400.
var lambdaTagKey = regexp.MustCompile(`^[a-z][a-z0-9-:]{0,54}$`)

// launchTags renders the Spec's tags into Lambda's key/value array, dropping
// any the vendor would reject.
//
// Dropping rather than refusing is deliberate. A tag is bookkeeping and a
// launch is the thing the operator actually asked for, so failing the rent
// over an unusable key would trade the expensive half for the cheap one. The
// two keys iplane stamps itself always pass; this only bites a caller-supplied
// tag, and such a tag is absent from the instance rather than silently
// altered.
//
// Returns nil for an empty map so the caller can omit the field rather than
// sending an empty array.
func launchTags(tags map[string]string) []apiTag {
	out := make([]apiTag, 0, len(tags))
	for k, v := range tags {
		if !lambdaTagKey.MatchString(k) || len(v) > 128 {
			continue
		}
		out = append(out, apiTag{Key: k, Value: v})
	}
	if len(out) == 0 {
		return nil
	}
	// Stable order so a launch body is comparable between runs.
	slices.SortFunc(out, func(a, b apiTag) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// parseVRAMFromDescription extracts a VRAM value in GB from
// Lambda's `instance_type.description` field. Lambda's API uses a
// consistent "(N GB ...)" pattern -- "1x A10 (24 GB PCIe)",
// "8x H100 (80 GB SXM5)". We pull the first "(N GB" match.
//
// Returns 0 if no match is found (the catalog lookup is preferred
// when available; this is the fallback for non-curated SKUs).
func parseVRAMFromDescription(desc string) int {
	// Look for "(N GB" anywhere in the description.
	i := strings.Index(desc, "(")
	if i < 0 {
		return 0
	}
	tail := desc[i+1:]
	end := strings.Index(tail, " GB")
	if end < 0 {
		return 0
	}
	var n int
	for _, r := range tail[:end] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// lambdaMetadata captures the Lambda-specific fields outside
// Hardware. Lambda's instance record is comparatively lean
// (the rich detail is on the host record, not surfaced).
func lambdaMetadata(api *apiInstance) map[string]*structpb.Value {
	out := map[string]*structpb.Value{}
	if api.Region.Description != "" {
		out["lambda.region_description"] = structpb.NewStringValue(api.Region.Description)
	}
	if api.InstanceType.Description != "" {
		out["lambda.gpu_description"] = structpb.NewStringValue(api.InstanceType.Description)
	}
	if api.Hostname != "" {
		out["lambda.hostname"] = structpb.NewStringValue(api.Hostname)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapLambdaState translates Lambda's status enum into iplane's
// InstanceState. The vendor publishes six values and this handles all six:
//
//	"booting"      -> PENDING
//	"active"       -> ACTIVE
//	"unhealthy"    -> ACTIVE (still rented; not a terminal state)
//	"terminating"  -> TERMINATING
//	"terminated"   -> TERMINATED
//	"preempted"    -> TERMINATED
//
// `preempted` used to fall through to the unknown-value default, which is
// PENDING, and PENDING is the one answer that costs something: the caller
// waits out the whole engine-ready deadline on a machine that is gone and
// never coming back. Lambda sells no reclaimable tier, so a preemption is
// the vendor taking the box back rather than anything an operator asked
// for, which makes it worth being precise about.
//
// An unrecognised value still defaults to PENDING, on the reasoning that a
// vocabulary iplane has not caught up with is more likely a new
// intermediate state than a new terminal one. The cost of that guess is the
// preempted case above, so a new value showing up in
// TestMapLambdaStateCoversTheVendorsEnum is worth reading carefully.
func mapLambdaState(status string) provisionerv1.InstanceState {
	if s, ok := lambdaStates[strings.ToLower(strings.TrimSpace(status))]; ok {
		return s
	}
	return provisionerv1.InstanceState_INSTANCE_STATE_PENDING
}

// lambdaStates is the mapping as data rather than as a switch, so a test can
// compare its keys against the vendor's recorded enum. A status Lambda
// publishes and this map omits is the drift that matters.
var lambdaStates = map[string]provisionerv1.InstanceState{
	"booting":     provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
	"active":      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	"unhealthy":   provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	"terminating": provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING,
	"terminated":  provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED,
	"preempted":   provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED,
}

// API response shapes. Field names verified via real-API probe
// 2026-06.

type launchResponse struct {
	Data struct {
		InstanceIDs []string `json:"instance_ids"`
	} `json:"data"`
}

// apiSSHKey is one stored key on the operator's Lambda account. Named
// rather than inline because both the launch path and the KeyRegistrar
// read it, and the registrar has to compare the public half.
type apiSSHKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

type sshKeysResponse struct {
	Data []apiSSHKey `json:"data"`
}

type instanceTypeBlock struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	GPUDescription    string `json:"gpu_description"`
	PriceCentsPerHour int    `json:"price_cents_per_hour"`

	// Architecture is the host CPU architecture ("x86_64", "arm64"). Load
	// bearing rather than cosmetic: Lambda's GH200 shapes are arm64, and an
	// engine image built for x86 will not run on one. Normalized onto the
	// shared vocabulary before it leaves the adapter.
	Architecture string `json:"architecture"`
	Specs        struct {
		VCPUs      int `json:"vcpus"`
		MemoryGiB  int `json:"memory_gib"`
		StorageGiB int `json:"storage_gib"`
		GPUs       int `json:"gpus"`
	} `json:"specs"`
}

// apiTag is one key/value pair on an instance. Lambda bounds a key to
// `^[a-z][a-z0-9-:]+$` at 55 characters and a value at 128, and rejects the
// whole launch when a key is outside that, which is why Spawn filters rather
// than forwarding whatever it was handed.
type apiTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type apiInstance struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	IP           string            `json:"ip"`
	Hostname     string            `json:"hostname"`
	Tags         []apiTag          `json:"tags"`
	InstanceType instanceTypeBlock `json:"instance_type"`
	Region       struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"region"`
}

// instanceTypesEntry is one shape plus where it can currently be had.
//
// The capacity list is the fact the static catalog cannot hold. Probing live
// on 2026-08-15, fifteen of Lambda's twenty-three shapes had capacity in no
// region at all, and an empty list is a real answer rather than a missing one.
type instanceTypesEntry struct {
	InstanceType        instanceTypeBlock `json:"instance_type"`
	RegionsWithCapacity []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"regions_with_capacity_available"`
}

// instanceTypesResponse is keyed by instance-type name rather than being a
// list, which is why the decode target is a map.
type instanceTypesResponse struct {
	Data map[string]instanceTypesEntry `json:"data"`
}

type instanceResponse struct {
	Data apiInstance `json:"data"`
}

type instanceListResponse struct {
	Data []apiInstance `json:"data"`
}

// TracksInstances implements provisioners.InstanceTracker: this provider
// keeps a registry of what it rents, so a not-found from Describe is
// evidence the instance is gone rather than evidence it was never tracked.
func (p *Provider) TracksInstances() bool { return true }
