package vast

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	skhttp "github.com/panyam/servicekit/http"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engineagent"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/enginewait"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// Deploy satisfies provisioners.Deployer: it rents an instance whose
// container IS the engine, the way the RunPod adapter does.
//
// # Why this provider is image-native
//
// Vast rents containers, not machines. The rent call takes an image and a
// startup command, which is the same shape as a RunPod pod and nothing like
// a VM. The previous design treated it as a VM: rent a container, SSH in,
// install docker, and run the engine image inside it. That is
// docker-in-docker on a host that will not grant the privileges, and it
// failed at the docker install every time.
//
// # What Vast does with the startup command, measured
//
// Probed on a rented box, because the answer decides the whole design:
//
//	pid 1  = /bin/sh -c while [ ! -e /.launch ]; do sleep 1; done; bash /.launch
//	         (Vast's own launcher)
//	child  = bash /root/onstart.sh   (our command)
//	also   = sshd
//
// Two consequences. Vast owns pid 1 and **never runs the image's
// entrypoint**, so the deployment must supply the whole engine command
// rather than only its arguments; that is what Deployment.engine_entrypoint
// is for. For a vllm/vllm-openai image EngineCommand fills it in; for any
// other image it is required, and the deploy refuses before renting. And
// sshd keeps running alongside, so `iplane instance ssh` still works for
// debugging even though nothing in the deploy path uses it.
func (p *Provider) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	if dep == nil || inst == nil {
		return fmt.Errorf("vast.Deploy: deployment and instance are required")
	}
	if dep.GetImage() == "" {
		return deployFailed(emit, "validate", fmt.Errorf("deployment.image is required for the image-as-container model"))
	}
	engineCmd, err := EngineCommand(dep.GetImage(), dep.GetEngineEntrypoint())
	if err != nil {
		return deployFailed(emit, "validate", err)
	}

	enginePort := dep.GetEnginePort()
	if enginePort == 0 {
		enginePort = 8000
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		Phase:           "vast:find-offer",
		ProgressMessage: fmt.Sprintf("searching for capacity for image %s", dep.GetImage()),
	})

	reqs := inst.GetSpec().GetRequirements()
	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := int(reqs.GetMinDiskGb())
	if diskGB <= 0 {
		diskGB = defaultDiskGB
	}
	skuName := reqs.GetSku()
	if skuName == "" {
		if matches := MatchSKUs(reqs); len(matches) > 0 {
			skuName = matches[0]
		} else {
			return deployFailed(emit, "vast:find-offer", fmt.Errorf(
				"no vast SKU satisfies the requirements (min_vram_gb=%d)%s", reqs.GetMinVramGb(), splitHint(reqs)))
		}
	}
	offer, err := p.findOffer(ctx, skuName, gpuCount, diskGB, reqs)
	if err != nil {
		return deployFailed(emit, "vast:find-offer", err)
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		Phase:           "vast:rent",
		ProgressMessage: fmt.Sprintf("renting offer %d (%s x%d)", offer.ID, skuName, gpuCount),
	})

	rented, err := p.rentEngine(ctx, offer.ID, dep, engineCmd, enginePort, diskGB)
	if err != nil {
		return deployFailed(emit, "vast:rent", err)
	}
	contractID := strconv.Itoa(rented.NewContract)

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		Phase:           phaseScheduling,
		ProgressMessage: "instance rented; waiting for the host to create the container",
		ContainerID:     contractID,
	})

	endpoint, err := p.waitForEngineReadyWithin(ctx, rented.NewContract, enginePort, p.engineReadyTimeoutFor(diskGB), emit)
	if err != nil {
		return deployFailed(emit, "vast:engine-ready", withDiskFullHint(err, diskGB, reqs.GetMinDiskGb() > 0))
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:           "engine:serving",
		ProgressMessage: "engine /health is 2xx",
		ContainerID:     contractID,
		EngineEndpoint:  endpoint,
	})
	return nil
}

// Destroy terminates the rented contract.
//
// Idempotent: an already-gone contract settles as terminated rather than
// erroring, so a retried teardown does not strand the deployment in
// TERMINATING.
//
// The contract id is read from the deployment's container_id first and the
// instance's provider_id second, mirroring the RunPod adapter. The two
// differ by deployment shape: the singular path stamps the deployment, the
// multi-replica path stamps each instance, and reading only one of them is
// how issue 228 leaked every non-slot-0 machine.
func (p *Provider) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	contractID := dep.GetContainerId()
	if contractID == "" && inst != nil {
		contractID = inst.GetProviderId()
	}
	if contractID == "" {
		emit(provisioners.DeployStateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
			Phase:           "vast:terminate",
			ProgressMessage: "no contract id on record; nothing to terminate",
		})
		return nil
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
		Phase:           "vast:terminate",
		ProgressMessage: fmt.Sprintf("terminating contract %s", contractID),
	})
	if err := p.Terminate(ctx, contractID); err != nil {
		return deployFailed(emit, "vast:terminate", err)
	}
	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		Phase:           "vast:terminate",
		ProgressMessage: "contract terminated",
	})
	return nil
}

// rentEngine rents an offer whose container is the engine itself.
//
// Two fields carry the design. onstart is the engine command, because Vast
// runs it in place of the image's entrypoint. And the port mapping is
// requested through the env map using docker's own "-p" spelling, which is
// Vast's convention for passing run options: there is no typed ports field.
func (p *Provider) rentEngine(ctx context.Context, offerID int, dep *provisionerv1.Deployment, engineCmd []string, enginePort int32, diskGB int) (*rentResponse, error) {
	env := map[string]string{
		fmt.Sprintf("-p %d:%d", enginePort, enginePort): "1",
	}
	for k, v := range dep.GetEnv() {
		env[k] = v
	}

	body := map[string]any{
		"client_id": "me",
		"image":     dep.GetImage(),
		"disk":      diskGB,
		"label":     instanceLabelPrefix + dep.GetId(),
		// ssh keeps sshd running alongside the workload, which costs
		// nothing and preserves `iplane instance ssh` for debugging a
		// deployment that will not start.
		"runtype": "ssh",
		"onstart": onstartScript(engineCmd, dep, enginePort),
		"env":     env,
	}
	req, err := p.client.newReq(http.MethodPut, pathAskPrefix+strconv.Itoa(offerID)+"/", nil, body)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[rentResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	if !resp.Success || resp.NewContract == 0 {
		return nil, fmt.Errorf("rent failed: %s", resp.Msg)
	}
	return &resp, nil
}

// onstartScript builds the shell Vast runs as the container's workload.
//
// The engine argv is quoted rather than interpolated: the model name and the
// operator's engine args are arbitrary strings that a shell would otherwise
// interpret, and this text becomes a script on a machine we are paying for.
func onstartScript(engineCmd []string, dep *provisionerv1.Deployment, enginePort int32) string {
	argv := append([]string{}, engineCmd...)
	argv = append(argv, "--model", dep.GetModel(), "--host", "0.0.0.0", "--port", strconv.Itoa(int(enginePort)))
	argv = append(argv, dep.GetEngineArgs()...)

	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return agentPrelude(dep) + "exec " + strings.Join(quoted, " ")
}

// agentPrelude returns the shell that starts the registration agent beside
// the engine, or "" to leave the startup script exactly as it was.
//
// Vast is the cheapest of the three delivery paths and the last to get one.
// RunPod needs an entrypoint wrapper because the image's own entrypoint is
// what runs; the SSH path needs a sidecar container. Here the provider hands
// us the startup script outright, so the agent is simply two blocks above the
// engine in a file we already write. No wrapper, no sidecar.
//
// The env the agent reads is already arriving: rentEngine forwards
// dep.GetEnv() into the container, and the deploy path stamps the identity
// there. Only the launch was missing.
//
// "" is the safe answer and the common one. Both conditions have to hold
// before the script is touched: the deploy path stamped an identity, so there
// is something to register as, and a binary URL resolves, so there is an
// agent to fetch. A dev build publishes no artifact and deliberately has no
// URL, so this is empty on every untagged build and the deploy goes out
// exactly as it did before. Vast needs no engine-entrypoint check -- unlike
// RunPod it cannot deploy without one, because the provider never runs the
// image's own entrypoint.
func agentPrelude(dep *provisionerv1.Deployment) string {
	env := dep.GetEnv()
	if env[engineagent.EnvEngineID] == "" || env[engineagent.EnvServiceURL] == "" {
		return ""
	}
	url, ok := engineagent.BinaryURL("amd64")
	if !ok {
		return ""
	}
	prelude, err := engineagent.AgentPrelude(url)
	if err != nil {
		return ""
	}
	return prelude + "\n"
}

// waitForEngineReady polls until the engine answers /health, returning the
// endpoint it answered on.
//
// The endpoint cannot be derived up front the way RunPod's proxy URL can.
// Vast publishes the host's public address and the high port docker mapped
// the engine onto only once the container is running, so this waits for the
// mapping to appear and then for the engine behind it to serve.
func (p *Provider) waitForEngineReady(ctx context.Context, contractID int, enginePort int32, emit func(provisioners.DeployStateUpdate)) (string, error) {
	return p.waitForEngineReadyWithin(ctx, contractID, enginePort, p.engineReadyTimeout, emit)
}

// engineReadyTimeoutFor scales the engine-ready deadline with the container
// disk, which is sized from the checkpoint when the model store knows it.
//
// A fixed 30 minutes holds a small model and not a frontier one: a 474 GB
// checkpoint at the 1000 Mbps floor is over an hour of download alone. The
// scale is ten minutes for the image and the load plus the disk at
// 100 MB/s, a rate the bandwidth floor clears, so 200 GB waits about 43
// minutes and 570 GB about 105. A host too slow for even that is caught by
// the slow-download abort, which projects the measured rate against this
// deadline, rather than by the deadline itself.
//
// Only the built-in default scales. A timeout the operator set through
// IPLANE_ENGINE_READY_TIMEOUT is used as given.
func (p *Provider) engineReadyTimeoutFor(diskGB int) time.Duration {
	base := p.engineReadyTimeout
	if base <= 0 {
		base = defaultEngineReadyTimeout
	}
	if p.engineReadyTimeoutSet || base != defaultEngineReadyTimeout {
		return base
	}
	scaled := 10*time.Minute + time.Duration(diskGB)*10*time.Second
	return max(base, scaled)
}

func (p *Provider) waitForEngineReadyWithin(ctx context.Context, contractID int, enginePort int32, timeout time.Duration, emit func(provisioners.DeployStateUpdate)) (string, error) {
	if timeout <= 0 {
		timeout = defaultEngineReadyTimeout
	}
	client := &http.Client{Timeout: 5 * time.Second}
	id := strconv.Itoa(contractID)

	return enginewait.Wait(ctx, enginewait.Config{
		Timeout:     timeout,
		Interval:    p.sshReadyInterval,
		ContainerID: id,
		Ladder: enginewait.Ladder{
			Ordinal:     enginePhaseOrdinal,
			Description: enginePhaseDescription,
		},
		TimeoutHint: engineReadyTimeoutHint,
		Observe: func(ctx context.Context, _ string) enginewait.Observation {
			api, derr := p.describeContract(ctx, contractID)
			if derr != nil {
				// Not fatal on its own. Vast's control API goes slow in
				// bursts and recovers; a `context deadline exceeded
				// (awaiting headers)` here was observed resolving itself
				// mid-deploy, so it is reported as progress rather than
				// treated as a dead host.
				return enginewait.Observation{
					Phase:  phaseScheduling,
					Detail: fmt.Sprintf("describe contract %d: %v", contractID, derr),
				}
			}
			// Give up as soon as the provider says the container will not
			// run. Polling past this point cannot succeed and bills the
			// rest of the engine-ready timeout for the privilege.
			//
			// Routed through the shared capability rather than the local
			// policy with the record just fetched, which costs one extra
			// GET per tick. That is the deliberate price of the behaviour
			// living in one place, and it is what this hoist makes
			// reachable by every provider that implements it (#268).
			if dead, why := provisioners.TerminalFailure(ctx, p, id); dead {
				return enginewait.Observation{
					Fatal: fmt.Errorf("contract %d will not start: %s", contractID, why),
				}
			}
			endpoint, _ := endpointFromInstance(api, enginePort)
			// The record the loop already fetches carries the host's
			// account of the pull. Reading it here is what turns one
			// opaque wait into a phase an operator can watch (#259).
			return enginewait.Observation{
				Endpoint: endpoint,
				Phase:    classifyEnginePhase(api.ActualStatus, endpoint != ""),
				Detail:   pullProgress(api.StatusMsg),
			}
		},
		Probe: func(ctx context.Context, endpoint string) (bool, string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
			if err != nil {
				return false, err.Error()
			}
			resp, derr := client.Do(req)
			if derr != nil {
				return false, fmt.Sprintf("%s/health -> %v", endpoint, derr)
			}
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code >= 200 && code < 300 {
				return true, ""
			}
			return false, fmt.Sprintf("%s/health -> %d", endpoint, code)
		},
		Emit: emit,
	})
}

// engineEndpoint returns the externally reachable base URL for the engine,
// or "" plus a human-readable reason while it is not yet knowable.
func (p *Provider) engineEndpoint(ctx context.Context, contractID int, enginePort int32) (string, string) {
	api, err := p.describeContract(ctx, contractID)
	if err != nil {
		return "", fmt.Sprintf("describe contract %d: %v", contractID, err)
	}
	return endpointFromInstance(api, enginePort)
}

// endpointFromInstance is the pure half of engineEndpoint. Split out so the
// readiness loop can inspect the same instance record for a terminal failure
// without paying a second API call per tick.
func endpointFromInstance(api *apiInstance, enginePort int32) (string, string) {
	if api.PublicIPAddr == "" {
		return "", "waiting for the host's public address"
	}
	binds, ok := api.Ports[fmt.Sprintf("%d/tcp", enginePort)]
	if !ok || len(binds) == 0 || binds[0].HostPort == "" {
		return "", fmt.Sprintf("waiting for port %d to be mapped", enginePort)
	}
	return fmt.Sprintf("http://%s:%s", api.PublicIPAddr, binds[0].HostPort), ""
}

// defaultEngineReadyTimeout bounds the wait for the engine to serve. Longer
// than the SSH wait because it covers the image pull and the model load, and
// a large model's weights dominate both.
const defaultEngineReadyTimeout = 30 * time.Minute

// engineReadyTimeoutHint names the setting behind the engine-ready deadline.
// It is the daemon's, so the CLI's --timeout cannot extend it.
const engineReadyTimeoutHint = "this deadline is the control plane's (30m, scaled up with the container disk past 120 GB); " +
	"raise IPLANE_ENGINE_READY_TIMEOUT where it runs, since the CLI's --timeout does not extend it"

// withDiskFullHint adds the remedy to an engine that died because the
// container disk filled. The engine's own words ("No space left on device")
// arrive in the logs appended to err, and say what happened without saying
// which flag fixes it or whether the size was chosen or defaulted.
func withDiskFullHint(err error, diskGB int, sized bool) error {
	if !strings.Contains(strings.ToLower(err.Error()), "no space left on device") {
		return err
	}
	if sized {
		return fmt.Errorf("%w\nthe %d GB container disk filled: raise --min-disk-gb above %d", err, diskGB, diskGB)
	}
	return fmt.Errorf("%w\nthe %d GB container disk filled. That is vast's default, used because the model store "+
		"could not report this checkpoint's size: pass --min-disk-gb with room for the weights plus about 40 GB for the engine image", err, diskGB)
}

// deployFailed emits a terminal failure and returns the error, so callers
// report the same reason the state file records.
func deployFailed(emit func(provisioners.DeployStateUpdate), phase string, err error) error {
	emit(provisioners.DeployStateUpdate{
		State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
		Phase:         phase,
		FailureReason: err.Error(),
	})
	return fmt.Errorf("%s: %w", phase, err)
}
