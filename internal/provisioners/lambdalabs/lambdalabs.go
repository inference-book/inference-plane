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
// references them by name (`ssh_key_names: ["GMac"]`). The
// KeyRegistrar implementation uploads the operator's iplane-managed
// key once; subsequent Spawn calls reference it by name.
package lambdalabs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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

	// Look up an existing SSH key to attach. v0.2 expects the
	// operator to have run EnsurePublicKey (via KeyRegistrar)
	// earlier; the key shows up in /api/v1/ssh-keys with a
	// well-known name. For the smoke test the operator's
	// pre-existing key (any name) is fine -- the catalog of one
	// key is the simplest case.
	keyName, err := p.firstSSHKeyName(ctx)
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
			Gpu: &provisionerv1.GpuInfo{
				Sku: instanceTypeName,
			},
		}, nil
	}
	return p.instanceFromAPI(api, spec, instanceTypeName), nil
}

// Terminate deletes a rented Lambda Labs instance via POST
// /api/v1/instance-operations/terminate with the instance id in
// `instance_ids`. 404 surfaces as ErrNotFound.
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
		return wrapErr("terminate", err)
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
		iplaneID := strings.TrimPrefix(a.Name, instanceNamePrefix)
		out = append(out, &provisionerv1.InstanceRef{
			ProviderId:    a.ID,
			ProviderState: a.Status,
			Tags: map[string]string{
				provisioners.TagID: iplaneID,
			},
		})
	}
	return out, nil
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

// firstSSHKeyName returns the name of the first SSH key registered
// on the operator's Lambda Labs account. v0.2's Spawn references a
// single key by name; multi-key support would mean threading a
// preference (e.g., "iplane-<operator>") through the API.
//
// Returns "" + nil error when no keys are present; the caller
// distinguishes this from an actual API failure.
func (p *Provider) firstSSHKeyName(ctx context.Context) (string, error) {
	req, err := p.client.newReq(http.MethodGet, pathSSHKeys, nil, nil)
	if err != nil {
		return "", err
	}
	resp, err := skhttp.Call[sshKeysResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return "", err
	}
	if len(resp.Data) == 0 {
		return "", nil
	}
	return resp.Data[0].Name, nil
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
	vram := 0
	gpuCount := 1
	if api.InstanceType.Specs.GPUs > 0 {
		gpuCount = api.InstanceType.Specs.GPUs
	}
	if sku := LookupSKU(api.InstanceType.Name); sku != nil {
		vram = sku.VRAMGb
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
		Gpu: &provisionerv1.GpuInfo{
			Sku:    instanceTypeName,
			Count:  int32(gpuCount),
			VramGb: int32(vram),
		},
	}
	if api.IP != "" {
		inst.Ssh = &provisionerv1.SshTarget{
			Host: api.IP,
			Port: 22,
			User: "ubuntu", // Lambda Labs default SSH user
		}
	}
	return inst
}

// mapLambdaState translates Lambda's status enum into iplane's
// InstanceState. Values verified via the API docs + probe:
//
//	"booting"      -> PENDING
//	"active"       -> ACTIVE
//	"unhealthy"    -> ACTIVE (still rented; not a terminal state)
//	"terminating"  -> TERMINATING
//	"terminated"   -> TERMINATED
//
// Unknown values default to PENDING (conservative; treat as
// not-yet-active rather than ACTIVE-by-default).
func mapLambdaState(status string) provisionerv1.InstanceState {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active", "unhealthy":
		return provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE
	case "booting", "":
		return provisionerv1.InstanceState_INSTANCE_STATE_PENDING
	case "terminating":
		return provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING
	case "terminated":
		return provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED
	default:
		return provisionerv1.InstanceState_INSTANCE_STATE_PENDING
	}
}

// API response shapes. Field names verified via real-API probe
// 2026-06.

type launchResponse struct {
	Data struct {
		InstanceIDs []string `json:"instance_ids"`
	} `json:"data"`
}

type sshKeysResponse struct {
	Data []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	} `json:"data"`
}

type instanceTypeBlock struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	GPUDescription    string `json:"gpu_description"`
	PriceCentsPerHour int    `json:"price_cents_per_hour"`
	Specs             struct {
		VCPUs      int `json:"vcpus"`
		MemoryGiB  int `json:"memory_gib"`
		StorageGiB int `json:"storage_gib"`
		GPUs       int `json:"gpus"`
	} `json:"specs"`
}

type apiInstance struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	IP           string            `json:"ip"`
	Hostname     string            `json:"hostname"`
	InstanceType instanceTypeBlock `json:"instance_type"`
	Region       struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"region"`
}

type instanceResponse struct {
	Data apiInstance `json:"data"`
}

type instanceListResponse struct {
	Data []apiInstance `json:"data"`
}
