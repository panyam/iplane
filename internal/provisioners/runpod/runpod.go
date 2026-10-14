// Package runpod implements the Provider interface against RunPod's
// REST API v2 (https://api.runpod.io/v2). Pods are created with
// POST /pods, read with GET /pods/{id}, listed with GET /pods (cursor
// paginated, filtered client-side) and terminated with DELETE /pods/{id}.
// The create response is the full pod record, so no follow-up read is
// needed to learn its price or placement.
//
// One GPU type per create. v2's create places exactly one GPU type in one
// cloud and never falls back, where v1 took a list of gpuTypeIds and let
// RunPod pick. The adapter keeps the old behaviour by walking the
// cheapest-first SKU list itself, moving on when RunPod answers 400 (no
// capacity for this combination) or 403 (account cannot use this pool),
// and stopping on anything else. That is the read-then-create loop
// RunPod's own documentation for POST /v2/pods prescribes.
//
// Tag stamping. The create body has no tag field, so the adapter encodes
// iplane-id in the pod name (prefix "iplane-") and that is the only
// iplane tag visible on the pod itself. v2 has no name filter on
// GET /pods, so List reads every page and matches client-side. The
// iplane-operator tag lives only in the iplane state file.
//
// SSH readiness. Spawn returns State=ACTIVE as soon as RunPod
// acknowledges the pod, but the SSH endpoint may take another 20-60s to
// become reachable. ssh fields stay empty in the Spawn response unless
// RunPod has already published a direct SSH endpoint.
package runpod

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Defaults for fields that v0.1 does not expose on Spec.
//
// Cloud tier. RunPod's two clouds are SECURE (T1/T2 datacenters; A100s,
// H100s, datacenter cards) and COMMUNITY (T3/T4 community hosts; cheap
// consumer cards like RTX 4090 and A5000). v1 left cloudType unset on the
// create; v2 defaults it to SECURE and, like the GPU type, takes exactly
// one per create. So the adapter tries the clouds in order itself, the
// same way it walks the SKU list (see createPod).
//
// Which clouds a create may use depends on whether it needs SSH. v1 had a
// supportPublicIp flag that restricted placement to hosts with a routable
// IP; v2 has no such flag. SECURE pods get a public IP by default and
// COMMUNITY pods may not, so a pod that has to be SSH-reachable (Spawn,
// and a deploy with debug_shell) stays on SECURE, and only the proxy-only
// engine deploy also tries COMMUNITY.
//
// Cost-aware default: the Deployer ships the engine port as /http
// (proxied at <pod-id>-<port>.proxy.runpod.net) and does not expose
// 22/tcp. The proxy URL is free, deterministic, and works regardless of
// which cloud RunPod scheduled onto. Health probes hit the proxy URL
// directly; no public IP wait, no NAT lookup.
//
// Opt-in: Deployment.debug_shell=true (CLI: --debug-shell) adds 22/tcp
// and startSsh, and keeps the pod on SECURE. The operator gets shell
// access to the running engine pod (`iplane instance ssh`) in exchange
// for the narrower placement.
//
// Spawn (iplane instance create) always asks for SSH: that path's
// purpose is provisioning an SSH-able VM-style instance, so no
// reachable sshd means no usable instance.
const (
	defaultBaseImage       = "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04"
	defaultContainerDiskGB = 20
	defaultVolumeMountPath = "/workspace" // where a network volume lands when the mount names no path
	podNamePrefix          = "iplane-"

	cloudSecure    = "SECURE"
	cloudCommunity = "COMMUNITY"
)

// Default ports list. SSH (22/tcp) for phase 2's deploy primitive to
// docker-run on top; HTTP (8000/http) for vLLM's OpenAI-compat server
// once the engine container is running.
var defaultPortsList = []string{"22/tcp", "8000/http"}

// Cloud orders for createPod. sshClouds is for a pod that must be
// SSH-reachable; anyCloud is for the proxy-only engine pod.
var (
	sshClouds = []string{cloudSecure}
	anyCloud  = []string{cloudSecure, cloudCommunity}
)

// Provider implements provisioners.Provider for RunPod.
type Provider struct {
	client *Client
	clock  func() time.Time

	// Two distinct readiness waits with very different timing profiles:
	//
	//   sshReadyTimeout    -- WaitForSSHReady: publicIp allocation +
	//                          sshd accepting on tcp/22. Tail is
	//                          dominated by IP allocation (~30s) and
	//                          sshd startup (~15s). 120s is comfortable.
	//
	//   engineReadyTimeout -- waitForEngineReady: image-as-pod deploys
	//                          HTTP-poll the engine /health on the
	//                          provider's proxy URL. Tail is dominated
	//                          by the container image pull (multi-GB,
	//                          1-5 min on a fresh host) and engine model
	//                          load (Qwen2.5-1.5B is ~3GB from HF).
	//                          5min is the conservative default; image
	//                          + model cache hits shrink it to ~60s.
	//
	// Polling interval is shared (the rate doesn't need different
	// tuning per wait kind).
	sshReadyTimeout    time.Duration
	engineReadyTimeout time.Duration
	sshReadyInterval   time.Duration

	// Model-staging poll cadence + budget (VolumeManager.StageModel).
	// Zero falls back to the stagePollInterval / stageTimeout consts;
	// tests set a small interval so they don't sleep between polls.
	stageInterval time.Duration
	stageBudget   time.Duration

	// sshProbe is the function used to verify tcp/22 is actually
	// accepting connections (RunPod's publicIp can be assigned a few
	// seconds before the container's sshd is ready to handshake).
	// Default: net.DialTimeout. Tests inject a no-op via WithSSHProbe
	// so they don't need a real listener.
	sshProbe func(ctx context.Context, host string, port int32) error

	// proxyBaseURL overrides the proxy.runpod.net base for tests --
	// when set, the health probe dials <proxyBaseURL>/<pod-id>-<port>
	// instead of https://<pod-id>-<port>.proxy.runpod.net. Empty in
	// production. Tests stand up an httptest server to play the
	// proxy's role.
	proxyBaseURL string
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithSSHReadyWait overrides the poll deadline + interval used by
// WaitForSSHReady. Tests inject short values to keep them fast.
//
// NOTE: this sets BOTH the ssh-ready timeout AND the engine-ready
// timeout to the same value, for back-compat with existing tests
// (most of which want one short timeout for everything). Production
// callers wanting per-wait tuning should use WithEngineReadyTimeout
// separately.
func WithSSHReadyWait(timeout, interval time.Duration) Option {
	return func(p *Provider) {
		p.sshReadyTimeout = timeout
		p.engineReadyTimeout = timeout
		p.sshReadyInterval = interval
	}
}

// WithEngineReadyTimeout overrides only the engine-ready timeout
// (waitForEngineReady on the image-as-pod deploy path). Useful when
// the engine image is large and cold-pulls regularly take longer
// than the 5-minute default.
func WithEngineReadyTimeout(timeout time.Duration) Option {
	return func(p *Provider) {
		p.engineReadyTimeout = timeout
	}
}

// WithProxyBaseURL overrides the RunPod proxy URL base. Test-only;
// production always uses https://proxy.runpod.net via the deterministic
// per-pod subdomain.
func WithProxyBaseURL(base string) Option {
	return func(p *Provider) {
		p.proxyBaseURL = base
	}
}

// WithSSHProbe overrides the tcp-port-reachability check that
// WaitForSSHReady runs after the publicIp is populated. Tests pass a
// no-op so they don't need a real listener; production uses the
// default net.DialTimeout-based probe.
func WithSSHProbe(probe func(ctx context.Context, host string, port int32) error) Option {
	return func(p *Provider) { p.sshProbe = probe }
}

// New builds a RunPod Provider on top of a configured Client.
func New(client *Client, opts ...Option) *Provider {
	p := &Provider{
		client:             client,
		clock:              time.Now,
		sshReadyTimeout:    120 * time.Second,
		engineReadyTimeout: 10 * time.Minute,
		sshReadyInterval:   5 * time.Second,
		sshProbe:           dialTCPProbe,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// dialTCPProbe is the production sshProbe: open a TCP connection
// with a tight timeout and close it. A successful dial means sshd
// (or whatever the pod has on port 22) accepted the SYN; the actual
// SSH handshake happens later in the deployment executor.
func dialTCPProbe(ctx context.Context, host string, port int32) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// Name satisfies provisioners.Provider.
func (p *Provider) Name() string { return provisioners.ProviderRunPod }

// IsActiveProviderState satisfies provisioners.ActiveStateChecker.
// Delegates to the adapter-local mapping in skus.go.
func (p *Provider) IsActiveProviderState(state string) bool {
	return isActiveProviderState(state)
}

// Spawn issues POST /pods, trying each SKU the requirements resolve to
// until RunPod places one. The 201 carries the full pod record (price,
// placement, GPU), so there is no follow-up read.
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}

	// Resolve requirements -> ordered SKU list.
	//
	//   - Operator-supplied --gpu-sku is the escape hatch: pass through.
	//   - Otherwise, the service already expanded any --gpu-class
	//     shorthand into min_vram_gb / min_disk_gb / min_ram_gb on the
	//     ResourceRequirements before dispatching here. We ask the
	//     resolver (skus.go MatchSKUs) for the gpuTypeIds satisfying
	//     those constraints, cheapest first.
	reqs := spec.GetRequirements()
	if reqs == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("requirements is required"), 0)
	}
	var gpuTypeIDs []string
	if sku := reqs.GetSku(); sku != "" {
		gpuTypeIDs = []string{sku}
	} else {
		gpuTypeIDs = MatchSKUs(reqs)
		if len(gpuTypeIDs) == 0 {
			return nil, provisioners.NewProviderError(p.Name(), "spawn",
				fmt.Errorf("no SKU in the runpod catalog satisfies min_vram_gb=%d min_disk_gb=%d min_ram_gb=%d (try a different class or a lower constraint)%s",
					reqs.GetMinVramGb(), reqs.GetMinDiskGb(), reqs.GetMinRamGb(), splitHint(reqs)), 0)
		}
	}
	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}

	image := spec.GetBaseImage()
	if image == "" {
		image = defaultBaseImage
	}

	// Disk is per-instance in our model; RunPod's container disk is also
	// per-instance, so direct mapping. Use the operator's min_disk_gb if
	// larger than our default.
	containerDisk := defaultContainerDiskGB
	if d := int(reqs.GetMinDiskGb()); d > containerDisk {
		containerDisk = d
	}

	// System RAM is per-instance in our model; RunPod's minRamPerGpu is
	// per-GPU. Convert at the wire boundary: total/N (with a ceiling to
	// round up so we never under-request -- "I asked for 65 GB total on
	// a 2-GPU pod" should send minRamPerGpu=33, not 32).
	minRAMPerGPU := 0
	if r := int(reqs.GetMinRamGb()); r > 0 {
		minRAMPerGPU = (r + gpuCount - 1) / gpuCount
	}

	// Region is a best-effort pin: send dataCenterIds only when the
	// operator actually asked for a region. The demo defaults region to
	// "US-WA-1" for runpod, but pinning a single datacenter when an
	// operator just wants "any cheap GPU" turns "no capacity in this DC"
	// into "no capacity at all". Empty region = no pin = RunPod
	// schedules wherever capacity exists.
	var dataCenterIDs []string
	if r := strings.TrimSpace(spec.GetRegion()); r != "" {
		dataCenterIDs = []string{r}
	}

	createBody := createPodRequest{
		Name:          podNamePrefix + spec.GetId(),
		Image:         image,
		GPU:           &createGPU{Count: gpuCount, MinRAMPerGPU: minRAMPerGPU},
		Disk:          containerDisk,
		Ports:         defaultPortsList,
		DataCenterIDs: dataCenterIDs,
		// The account's registered keys reach the pod only when the
		// create asks for them. v1 injected them unconditionally.
		StartSSH: true,
	}

	pod, placed, err := p.createPod(ctx, "spawn", createBody, gpuPlacements(gpuTypeIDs, sshClouds))
	if err != nil {
		return nil, err
	}

	resolvedSKU := pod.gpuSKU()
	if resolvedSKU == "" {
		resolvedSKU = placed.gpuID
	}
	resolvedClass := reqs.GetClass()
	if resolvedClass == "" {
		resolvedClass = classifySKU(resolvedSKU)
	}

	// Note: the direct SSH endpoint is usually absent here -- RunPod
	// publishes it once the running pod has a public port for 22/tcp.
	// Callers that need the SSH endpoint populated (e.g. before deploying
	// onto the pod) drive the wait explicitly via WaitForSSHReady; Spawn
	// returns ACTIVE fast so callers that don't need SSH (e.g. operators
	// who just want a billed pod for an interactive jupyter / vscode
	// session) don't pay for it.
	return p.podToInstance(&pod, spec, resolvedClass, resolvedSKU, gpuCount), nil
}

// podPlacement is one (GPU type or CPU flavor, cloud) combination a
// create can try. Exactly one of gpuID and cpuID is set.
type podPlacement struct {
	gpuID string
	cpuID string
	cloud string
}

func (pl podPlacement) String() string {
	id := pl.gpuID
	if id == "" {
		id = pl.cpuID
	}
	if pl.cloud == "" {
		return id
	}
	return id + "/" + pl.cloud
}

// gpuPlacements expands a cheapest-first SKU list into the order createPod
// tries them: each SKU on every allowed cloud before the next SKU, so a
// cheaper card on the second cloud still beats a dearer one on the first.
func gpuPlacements(skus, clouds []string) []podPlacement {
	out := make([]podPlacement, 0, len(skus)*len(clouds))
	for _, sku := range skus {
		for _, cloud := range clouds {
			out = append(out, podPlacement{gpuID: sku, cloud: cloud})
		}
	}
	return out
}

// createPod issues POST /pods once per placement until one is accepted,
// returning the pod and the placement that took.
//
// v2's create places exactly one GPU type (or CPU flavor) in one cloud
// and does not fall back, so the fallback v1 did server-side from a list
// of gpuTypeIds now happens here. Which failures move on follows RunPod's
// own guidance for POST /v2/pods:
//
//   - 400: either this combination could not be placed (no capacity) or
//     the body broke a cross-field rule. The two share a status and
//     differ only in the human-readable detail, so try the next one; a
//     rule violation fails identically everywhere and surfaces as the
//     last error.
//   - 403: the account cannot use this pool (B200, H200 and H100 NVL
//     typically need approval). Skip it. Under v1 one restricted SKU in
//     gpuTypeIds failed the whole request.
//   - anything else (401, 402 insufficient balance, 422 malformed body,
//     429, 5xx, transport): stop. No other candidate will do better, and
//     retrying a 5xx is the caller's decision, not a reason to rent a
//     different card.
func (p *Provider) createPod(ctx context.Context, op string, base createPodRequest, tries []podPlacement) (podBody, podPlacement, error) {
	if len(tries) == 0 {
		return podBody{}, podPlacement{}, provisioners.NewProviderError(p.Name(), op, fmt.Errorf("no placement to try"), 0)
	}
	var lastErr error
	var tried []string
	for _, pl := range tries {
		body := base
		body.Cloud = pl.cloud
		if pl.gpuID != "" {
			g := createGPU{}
			if base.GPU != nil {
				g = *base.GPU
			}
			g.ID = pl.gpuID
			body.GPU = &g
		}
		if pl.cpuID != "" {
			c := createCPU{}
			if base.CPU != nil {
				c = *base.CPU
			}
			c.ID = pl.cpuID
			body.CPU = &c
		}

		req, err := p.client.newReq("POST", "/pods", nil, body)
		if err != nil {
			return podBody{}, pl, provisioners.NewProviderError(p.Name(), op, err, 0)
		}
		pod, err := skhttp.Call[podBody](ctx, req, p.client.callOpts()...)
		if err == nil {
			if pod.ID == "" {
				return podBody{}, pl, provisioners.NewProviderError(p.Name(), op,
					fmt.Errorf("runpod accepted the create but returned an empty pod id (placement %s)", pl), 0)
			}
			return pod, pl, nil
		}
		lastErr = wrapErr(op, err)
		tried = append(tried, pl.String())
		if status := httpStatus(err); status != 400 && status != 403 {
			return podBody{}, pl, lastErr
		}
	}
	last := tries[len(tries)-1]
	if len(tried) == 1 {
		return podBody{}, last, lastErr
	}
	// Every candidate refused. Keep the last refusal as the cause so its
	// detail (the only place RunPod says why) reaches the operator, and its
	// status so callers can still classify it.
	return podBody{}, last, provisioners.NewProviderError(p.Name(), op,
		fmt.Errorf("no capacity for any of %d placements [%s]; last refusal: %w",
			len(tried), strings.Join(tried, ", "), lastErr), httpStatus(lastErr))
}

// podAction issues POST /pods/{id}/action, v2's state-transition endpoint
// (start | stop | restart | terminate). start/stop/restart answer 200 with
// the updated pod; terminate answers 204 with no body, for which the
// returned pod is zero.
//
// No Provider verb maps onto stop/start today: iplane rents and releases,
// it does not pause, and Terminate uses DELETE /pods/{id}, the documented
// equivalent of the terminate action. This is the one place the transition
// endpoint is spoken, so a future pause verb does not have to rediscover
// its shape.
func (p *Provider) podAction(ctx context.Context, podID, action string) (podBody, error) {
	if podID == "" {
		return podBody{}, provisioners.NewProviderError(p.Name(), action, fmt.Errorf("providerID is empty"), 0)
	}
	req, err := p.client.newReq("POST", "/pods/"+url.PathEscape(podID)+"/action", nil, podActionRequest{Action: action})
	if err != nil {
		return podBody{}, provisioners.NewProviderError(p.Name(), action, err, 0)
	}
	pod, err := skhttp.Call[podBody](ctx, req, p.client.callOpts()...)
	if err != nil {
		return podBody{}, wrapErr(action, err)
	}
	return pod, nil
}

// getPod reads GET /pods/{id}.
func (p *Provider) getPod(ctx context.Context, podID string) (podBody, error) {
	req, err := p.client.newReq("GET", "/pods/"+url.PathEscape(podID), nil, nil)
	if err != nil {
		return podBody{}, err
	}
	return skhttp.Call[podBody](ctx, req, p.client.callOpts()...)
}

// WaitForSSHReady waits until the pod's SSH endpoint is genuinely
// usable -- both (1) RunPod has published a direct SSH endpoint (a
// public host and port for the pod's 22/tcp) and (2) the pod's sshd is
// accepting TCP connections on it. Both conditions have an unbounded
// tail (RunPod's port assignment ~5-10s, container boot + sshd startup
// another ~10-30s), and the whole window has to elapse before a Phase 2
// deployment can SSH in. This is the single "Join" point for callers.
//
// Returns the resolved SshTarget on success, or an error on timeout
// / ctx cancellation / underlying GET failure. On timeout, the
// partial SshTarget is returned alongside the error so callers who
// want a "best effort" view (e.g. iplane instance describe after a
// failed wait) can still inspect what came back.
//
// Multiple callers can safely call this concurrently; each is a
// no-op-when-already-populated check followed by a polling loop.
func (p *Provider) WaitForSSHReady(ctx context.Context, providerID string) (*provisionerv1.SshTarget, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready", fmt.Errorf("providerID is empty"), 0)
	}
	timeout := p.sshReadyTimeout
	if timeout <= 0 {
		// Always allow at least one GET; tests that disable polling
		// still want a single best-effort lookup.
		timeout = 1 * time.Second
	}
	interval := p.sshReadyInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)

	var last podBody
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return sshTargetFromPod(&last), ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return sshTargetFromPod(&last), provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("no direct ssh endpoint published within %s", timeout), 0)
		}
		pod, err := p.getPod(ctx, providerID)
		if err != nil {
			return sshTargetFromPod(&last), wrapErr("wait_ssh_ready", err)
		}
		last = pod
		if target := sshTargetFromPod(&pod); target != nil {
			// Stage 2: probe the port until sshd inside the pod is
			// reachable. Reuses the same deadline + interval so the
			// overall budget covers both stages.
			if err := p.waitForSSHPort(ctx, target, deadline, interval); err != nil {
				return target, err
			}
			return target, nil
		}
	}
}

// waitForSSHPort retries the TCP probe until it succeeds or the
// deadline fires. A successful probe means the pod's sshd is up;
// the deployment executor's actual SSH handshake will be the next
// step.
func (p *Provider) waitForSSHPort(ctx context.Context, target *provisionerv1.SshTarget, deadline time.Time, interval time.Duration) error {
	if p.sshProbe == nil {
		return nil
	}
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("tcp/%d on %s not reachable before deadline", target.GetPort(), target.GetHost()), 0)
		}
		if err := p.sshProbe(ctx, target.GetHost(), target.GetPort()); err == nil {
			return nil
		}
	}
}

// sshTargetFromPod builds a SshTarget from the pod's direct SSH endpoint.
// RunPod NATs the container's 22/tcp onto an arbitrary public port (e.g.
// 22085), so the port comes from the endpoint and is not assumed to be 22;
// dialing 22 would land on the host's sshd or nothing at all. Returns nil
// when no direct endpoint is published yet, so callers can distinguish
// "haven't observed one" from "observed one".
//
// Only `ssh.direct` qualifies. `ssh.proxy` (ssh.runpod.io) carries an
// interactive shell only, without the port forwarding and file copy the
// deployment executor needs.
func sshTargetFromPod(pod *podBody) *provisionerv1.SshTarget {
	host, port, user := pod.directSSH()
	if host == "" {
		return nil
	}
	return &provisionerv1.SshTarget{Host: host, Port: port, User: user}
}

// Terminate calls DELETE /pods/{id}. Idempotent: a 404 (not found)
// is treated as success because the pod is already in the desired
// end state.
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if providerID == "" {
		return provisioners.NewProviderError(p.Name(), "terminate", fmt.Errorf("providerID is empty"), 0)
	}
	req, err := p.client.newReq("DELETE", "/pods/"+url.PathEscape(providerID), nil, nil)
	if err != nil {
		return provisioners.NewProviderError(p.Name(), "terminate", err, 0)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		wrapped := wrapErr("terminate", err)
		if isWrappedNotFound(wrapped) {
			return nil
		}
		return wrapped
	}
	return nil
}

// Describe calls GET /pods/{id}. Returns ErrNotFound wrapped in
// ProviderError when RunPod returns 404.
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "describe", fmt.Errorf("providerID is empty"), 0)
	}
	pod, err := p.getPod(ctx, providerID)
	if err != nil {
		return nil, wrapErr("describe", err)
	}
	tags := tagsFromPodName(pod.Name)
	return p.podToInstance(&pod, specFromPod(&pod, tags), classifySKU(pod.gpuSKU()), pod.gpuSKU(), pod.gpuCountInt()), nil
}

// tagsFromPodName recovers the iplane tags encoded in a pod name. Only
// iplane-id survives the round trip; a pod without the prefix is one the
// operator created outside iplane and carries none.
func tagsFromPodName(name string) map[string]string {
	tags := map[string]string{}
	if id := strings.TrimPrefix(name, podNamePrefix); id != name && id != "" {
		tags[provisioners.TagID] = id
	}
	return tags
}

// List calls GET /pods, walks every page, and applies the filter
// match-all over the tags recovered from each pod name.
//
// v2 has no name filter on GET /pods (v1 had ?name=), so all the
// filtering is client-side. The listing is the account's pods, newest
// first, a thousand to a page, which keeps this one request for any
// account iplane is likely to see.
//
// Only iplane-id ends up encoded on the pod (via the name), so other tags
// (e.g., iplane-operator) would never match -- they silently filter out
// everything. Callers asking for operator-level filtering should consult
// the iplane state file instead, which IS the source of truth for that
// scope.
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	pods, err := listAll(ctx, p.client, "list", "/pods", func(r listPodsResponse) ([]podBody, pagination) {
		return r.Pods, r.Pagination
	})
	if err != nil {
		return nil, err
	}

	refs := make([]*provisionerv1.InstanceRef, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		tags := tagsFromPodName(pod.Name)
		if !matchesFilter(tags, filter) {
			continue
		}
		refs = append(refs, &provisionerv1.InstanceRef{
			ProviderId:    pod.ID,
			ProviderState: pod.Status,
			Tags:          tags,
			HourlyRateUsd: pod.Cost,
			CreatedAt:     parseRunPodTime(pod.CreatedAt),
		})
	}
	return refs, nil
}

// podToInstance assembles the iplane Instance from a pod record.
// resolvedClass and resolvedSKU come from the caller because Spawn
// knows them up-front.
func (p *Provider) podToInstance(pod *podBody, spec *provisionerv1.Spec, resolvedClass, resolvedSKU string, gpuCount int) *provisionerv1.Instance {
	createdAt := parseRunPodTime(pod.CreatedAt)
	now := timestamppb.New(p.clock())
	if resolvedSKU == "" {
		resolvedSKU = pod.gpuSKU()
	}
	if resolvedClass == "" {
		resolvedClass = classifySKU(resolvedSKU)
	}
	if gpuCount <= 0 {
		gpuCount = pod.gpuCountInt()
	}
	vramGB := gpuVRAMGB(resolvedSKU)

	ssh := &provisionerv1.SshTarget{}
	if t := sshTargetFromPod(pod); t != nil {
		ssh = t
	}

	region := pod.DataCenterID
	if region == "" {
		region = spec.GetRegion()
	}
	if createdAt == nil {
		createdAt = now
	}

	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: pod.ID,
		Provider:   p.Name(),
		Spec:       spec,
		Region:     region,
		Hardware: func() *provisionerv1.Hardware {
			hw := &provisionerv1.Hardware{
				GpuSku:    resolvedSKU,
				GpuCount:  int32(gpuCount),
				GpuVramMb: int32(vramGB * 1024), // gpuVRAMGB returns GB; convert to MB
			}
			stampFabric(hw, resolvedSKU)
			return hw
		}(),
		Metadata:      runpodMetadata(resolvedClass),
		HourlyRateUsd: pod.Cost,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:     createdAt,
		ActivatedAt:   now,
		Ssh:           ssh,
	}
}

// runpodMetadata captures the RunPod-specific fields outside the
// Hardware base. The class id stays in metadata because v0.2's Hardware
// base is per-instance hardware, not the iplane class taxonomy (which is
// derivable from gpu_vram_mb).
//
// runpod.machine_id used to ride here too. v2's pod record carries no
// machine id at all, so it is gone rather than left empty.
func runpodMetadata(class string) map[string]*structpb.Value {
	if class == "" {
		return nil
	}
	return map[string]*structpb.Value{"runpod.class": structpb.NewStringValue(class)}
}

// specFromPod reconstructs an approximate Spec for an externally-found
// pod (one Describe returned but we have no local record for). Used
// only by the Service's adopt path. The result is good enough to stamp
// on the Instance, not authoritative.
func specFromPod(pod *podBody, tags map[string]string) *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:        tags[provisioners.TagID],
		Provider:  provisioners.ProviderRunPod,
		Region:    pod.DataCenterID,
		BaseImage: pod.Image,
		Tags:      tags,
		Requirements: &provisionerv1.ResourceRequirements{
			Sku:       pod.gpuSKU(),
			GpuCount:  int32(pod.gpuCountInt()),
			MinVramGb: int32(gpuVRAMGB(pod.gpuSKU())),
		},
	}
}

// matchesFilter applies the post-fetch tag filter. v0.1 only encodes
// iplane-id on the pod (via name), so any filter key beyond iplane-id
// either matches because the value happens to be empty on both sides
// (rare) or filters out the pod entirely. Documented behavior.
func matchesFilter(podTags, want map[string]string) bool {
	for k, v := range want {
		if podTags[k] != v {
			return false
		}
	}
	return true
}

func parseRunPodTime(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return timestamppb.New(t)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return timestamppb.New(t)
	}
	return nil
}

// isWrappedNotFound walks the error chain looking for ErrNotFound.
// Used by Terminate to return nil on already-gone (the contract is
// idempotent).
func isWrappedNotFound(err error) bool {
	for e := err; e != nil; {
		if e == provisioners.ErrNotFound {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// createPodRequest is the JSON body for POST /v2/pods (CreatePodRequest
// in RunPod's OpenAPI). The schema closes the object
// (unevaluatedProperties: false), so every field here must be one it
// names; an unknown field is a 422, not a silently ignored extra.
//
// Mapping from the v1 body this replaced:
//
//	imageName                 -> image
//	gpuTypeIds + gpuCount     -> gpu.id + gpu.count (one type per create)
//	minRAMPerGPU              -> gpu.minRamPerGpu
//	computeType=CPU           -> cpu.id + cpu.vcpuCount (a flavor, see stage.go)
//	cloudType                 -> cloud
//	containerDiskInGb         -> disk
//	networkVolumeId + path    -> mounts.network[{volumeId, path}]
//	volumeInGb                -> mounts.persistent (never used: always 0)
//	dockerStartCmd            -> cmd
//	dockerEntrypoint          -> entrypoint
//	supportPublicIp           -> no equivalent; see the cloud comment above
//	gpuTypePriority           -> no equivalent; createPod walks the list
type createPodRequest struct {
	Name  string `json:"name"`
	Image string `json:"image,omitempty"`
	// Cloud is SECURE or COMMUNITY; RunPod defaults to SECURE. createPod
	// sets it per placement.
	Cloud string     `json:"cloud,omitempty"`
	GPU   *createGPU `json:"gpu,omitempty"`
	CPU   *createCPU `json:"cpu,omitempty"`
	// Disk is the container disk in GB, ephemeral.
	Disk  int               `json:"disk,omitempty"`
	Ports []string          `json:"ports,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	// Cmd REPLACES the image's CMD with these argv tokens (exec form);
	// the image's ENTRYPOINT -- the engine binary -- stays unless
	// Entrypoint is also set.
	Cmd           []string   `json:"cmd,omitempty"`
	Entrypoint    []string   `json:"entrypoint,omitempty"`
	Mounts        *podMounts `json:"mounts,omitempty"`
	DataCenterIDs []string   `json:"dataCenterIds,omitempty"` // preferred datacenters; empty lets the scheduler choose
	StartSSH      bool       `json:"startSsh,omitempty"`      // inject the account's registered keys as PUBLIC_KEY
}

// createGPU is CreateGpuConfig: one GPU type, a count, and placement
// filters on the host.
type createGPU struct {
	ID           string `json:"id"`
	Count        int    `json:"count,omitempty"`
	MinRAMPerGPU int    `json:"minRamPerGpu,omitempty"` // RunPod expresses RAM per-GPU; we convert from per-instance.
}

// createCPU is CreateCpuConfig: a CPU flavor from GET /catalog/cpus and a
// power-of-two vCPU count valid for it.
type createCPU struct {
	ID        string `json:"id"`
	VCPUCount int    `json:"vcpuCount"`
}

// podMounts is the subset of Mounts this adapter attaches: at most one
// network volume (maxItems is 1 today).
type podMounts struct {
	Network []networkMount `json:"network,omitempty"`
}

// networkMount attaches an existing network volume. Path has no default
// in v2 and must be sent.
type networkMount struct {
	VolumeID string `json:"volumeId"`
	Path     string `json:"path"`
}

// podActionRequest is the body for POST /pods/{id}/action.
type podActionRequest struct {
	Action string `json:"action"`
}

// listPodsResponse is GET /pods: the pods wrapped under a key, plus the
// cursor block.
type listPodsResponse struct {
	Pods       []podBody  `json:"pods"`
	Pagination pagination `json:"pagination"`
}

// podBody mirrors the subset of v2's Pod schema we consume. We
// deliberately do not bind every field RunPod returns -- only the ones
// that flow through to the iplane Instance or the cold-start phase
// ladder. Nullable strings (dataCenterId, cudaVersion, startedAt) decode
// to "" when null.
type podBody struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	// Status is PROVISIONING | STARTING | RUNNING | EXITED | ERROR |
	// TERMINATED. It replaces v1's desiredStatus, and like it describes
	// the rental more than the container: an exited container is
	// restarted and the pod keeps reading RUNNING (NOTES.md).
	Status string `json:"status"`
	// Cost is the current rate in USD per hour (0 once EXITED or
	// TERMINATED). v1 called it costPerHr.
	Cost      float64 `json:"cost"`
	CreatedAt string  `json:"createdAt"`
	// StartedAt is when the pod last started. Recorded for the phase
	// trace only. Its v1 predecessor lastStartedAt was stamped at pod
	// creation rather than container start (issue 208), and nothing has
	// shown this one behaves differently, so it is not used as a phase
	// boundary.
	StartedAt string `json:"startedAt"`
	// DataCenterID and CudaVersion are both properties of the host, set
	// once the scheduler has placed the pod. They replace v1's machineId
	// as the host-assignment signal (machinePresent).
	DataCenterID string      `json:"dataCenterId"`
	CudaVersion  string      `json:"cudaVersion"`
	GPU          *podGPU     `json:"gpu"`
	Ports        []string    `json:"ports"`
	SSH          *podSSH     `json:"ssh"`
	Runtime      *podRuntime `json:"runtime"`
}

// podGPU is GpuConfig. Memory is the pod's total SYSTEM RAM in GB, not
// VRAM; v2's pod record does not report VRAM, which comes from the SKU
// catalog instead (gpuVRAMGB).
type podGPU struct {
	ID        string  `json:"id"`
	Count     int     `json:"count"`
	VCPUCount float64 `json:"vcpuCount"`
	Memory    float64 `json:"memory"`
}

// podSSH is PodSsh. Proxy goes through ssh.runpod.io and exists once the
// pod has a machine; Direct is the pod's own sshd on its published
// 22/tcp, null until the running pod has a public port for it.
type podSSH struct {
	Proxy  *podSSHEndpoint `json:"proxy"`
	Direct *podSSHEndpoint `json:"direct"`
}

type podSSHEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

func (p *podBody) gpuSKU() string {
	if p.GPU != nil {
		return p.GPU.ID
	}
	return ""
}

func (p *podBody) gpuCountInt() int {
	if p.GPU != nil {
		return p.GPU.Count
	}
	return 0
}

// directSSH returns the pod's direct SSH endpoint, or an empty host when
// none is published. Falls back to the live port mapping in `runtime`
// (private 22 with a public IP and port), which carries the same fact.
func (p *podBody) directSSH() (host string, port int32, user string) {
	if p.SSH != nil && p.SSH.Direct != nil && p.SSH.Direct.Host != "" {
		port = int32(p.SSH.Direct.Port)
		if port == 0 {
			port = 22
		}
		user = p.SSH.Direct.Username
		if user == "" {
			user = "root"
		}
		return p.SSH.Direct.Host, port, user
	}
	if p.Runtime != nil {
		for _, rp := range p.Runtime.Ports {
			if rp.Private == 22 && rp.IP != "" && rp.Public > 0 {
				return rp.IP, int32(rp.Public), "root"
			}
		}
	}
	return "", 0, ""
}

// gpuVRAMGB is the per-card VRAM for a SKU from the static catalog, or 0
// for a SKU we have not catalogued. v1's pod record carried
// machine.gpuType.memoryInGb; v2's carries no VRAM figure at all.
func gpuVRAMGB(sku string) int {
	if spec := LookupSKU(sku); spec != nil {
		return spec.VRAMGb
	}
	return 0
}

// machinePresent reports whether RunPod has placed the pod on a host.
//
// v1 carried this as machineId (and a `machine` object that stayed {} for
// a whole deploy, issue 208). v2 has neither; what it has are fields only
// a host can supply: the datacenter the scheduler assigned, the host's
// CUDA version, and the SSH proxy endpoint, which is "null until the pod
// has a machine assignment". Any one counts. Present-but-empty values do
// not, for the same reason `machine: {}` did not.
//
// Because v2's create places the pod synchronously (a 400 when it cannot),
// these are often already set on the 201, so runpod:scheduling may be
// near-instant on v2. That is a measurement to make with
// IPLANE_RUNPOD_PHASE_TRACE, not an assumption to build on.
func (p *podBody) machinePresent() bool {
	if p.DataCenterID != "" || p.CudaVersion != "" {
		return true
	}
	return p.SSH != nil && p.SSH.Proxy != nil && p.SSH.Proxy.Host != ""
}

// podRuntime is v2's PodRuntime block. It is null until the container
// starts, which makes it the image-pull -> engine-init boundary (issue
// 208). Field presence is the signal; the values are utilization samples
// we don't use.
type podRuntime struct {
	Uptime int              `json:"uptime"`
	Ports  []podRuntimePort `json:"ports"`
	CPU    *podUtil         `json:"cpu"`
	Memory *podUtil         `json:"memory"`
	GPUs   []podUtil        `json:"gpus"`
}

// podRuntimePort is one live port mapping. Public and IP are null for an
// /http port, which is proxied rather than published.
type podRuntimePort struct {
	Private int    `json:"private"`
	Public  int    `json:"public"`
	Type    string `json:"type"`
	IP      string `json:"ip"`
}

type podUtil struct {
	Util int `json:"util"`
}

// containerStarted reports whether the engine container is actually
// running. Mirrors machinePresent's defensiveness: RunPod has a habit of
// returning a present-but-empty object (the documented create response
// carries "runtime": {}), so an empty runtime block counts as not-started
// rather than started.
func (r *podRuntime) containerStarted() bool {
	if r == nil {
		return false
	}
	return r.Uptime > 0 || len(r.Ports) > 0 || r.CPU != nil || r.Memory != nil || len(r.GPUs) > 0
}

// Compile-time check.
var _ provisioners.Provider = (*Provider)(nil)
