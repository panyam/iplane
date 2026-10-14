package runpod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

func okDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:         "my-llama",
		InstanceId: "my-pod",
		Image:      "vllm/vllm-openai:v0.7.0",
		Model:      "Qwen/Qwen2.5-1.5B-Instruct",
		EnginePort: 8000,
		EngineArgs: []string{"--gpu-memory-utilization", "0.9"},
		Env: map[string]string{
			"HF_HUB_DISABLE_TELEMETRY": "1",
		},
	}
}

func okInst() *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:         "my-pod",
		Provider:   "runpod",
		ProviderId: "rp-base",
		Gpu: &provisionerv1.GpuInfo{
			Sku:    "NVIDIA RTX A5000",
			Class:  "small",
			Count:  1,
			VramGb: 24,
		},
	}
}

// proxyServer stands in for proxy.runpod.net during tests. The
// deployer's health probe dials <base>/<pod-id>-<port>/health (see
// waitForEngineReady's proxyBaseURL branch); this server matches that
// path shape and returns the configured status code.
func proxyServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// e.g. /rp-engine-1-8000/health
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDeploy_HappyPath_GoesToRUNNING(t *testing.T) {
	proxyURL := proxyServer(t, http.StatusOK)
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			if req.Image != "vllm/vllm-openai:v0.7.0" {
				t.Errorf("POST body image = %q, want vllm/vllm-openai:v0.7.0", req.Image)
			}
			if req.GPU == nil || req.GPU.ID != "NVIDIA RTX A5000" || req.GPU.Count != 1 {
				t.Errorf("POST body gpu = %+v, want the instance's A5000 x1", req.GPU)
			}
			if req.Cloud != cloudSecure {
				t.Errorf("first placement cloud = %q, want SECURE", req.Cloud)
			}
			joined := strings.Join(req.Cmd, " ")
			if !strings.Contains(joined, "--model Qwen/Qwen2.5-1.5B-Instruct") {
				t.Errorf("cmd missing --model: %v", req.Cmd)
			}
			if !strings.Contains(joined, "--gpu-memory-utilization") {
				t.Errorf("cmd missing operator engine-args: %v", req.Cmd)
			}
			if req.Env["HF_HUB_DISABLE_TELEMETRY"] != "1" {
				t.Errorf("env not propagated: %+v", req.Env)
			}
			// Default deploy is proxy-only: no SSH keys, no /tcp ports
			// other than the engine /http.
			if req.StartSSH {
				t.Errorf("startSsh=true on default deploy; want false (proxy-only is the cost-aware default)")
			}
			if len(req.Ports) != 1 || req.Ports[0] != "8000/http" {
				t.Errorf("Ports = %v, want [8000/http]", req.Ports)
			}
			return 201, `{"id":"rp-engine-1","status":"PROVISIONING"}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), okInst(), nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
	if c.lastEndpoint() == "" {
		t.Error("expected engine endpoint in final update")
	}
	if c.lastContainer() != "rp-engine-1" {
		t.Errorf("container id = %q, want rp-engine-1", c.lastContainer())
	}
}

func TestDeploy_PostFailure_GoesToFAILED(t *testing.T) {
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			return 500, problem(500, "Internal Server Error", "upstream failure")
		}
		t.Errorf("unexpected %s %s after POST failure", method, path)
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client)

	c := &collector{}
	err := p.Deploy(context.Background(), okDep(), okInst(), nil, c.emit)
	if err == nil {
		t.Fatal("expected error when POST /pods fails")
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDeploy_NoSKU_OnInstance_GoesToFAILED(t *testing.T) {
	// Instance has no resolved GPU SKU -- the deployer should refuse
	// before issuing any HTTP call.
	called := atomic.Int32{}
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		called.Add(1)
		return 200, "{}"
	}}
	client := f.serve(t)
	p := New(client)

	inst := okInst()
	inst.Gpu = nil // no resolved GPU

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), inst, nil, c.emit); err == nil {
		t.Fatal("expected error when instance has no GPU SKU")
	}
	if got := called.Load(); got != 0 {
		t.Errorf("expected zero HTTP calls when validation fails; got %d", got)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDeploy_SKUFromRequirements_AutoProvisioned(t *testing.T) {
	// Auto-provisioned instance: a PENDING shell with no resolved GPU,
	// only Spec.Requirements. The deployer resolves the SKU from the
	// requirements (here an explicit Sku) and stamps it into the POST.
	proxyURL := proxyServer(t, http.StatusOK)

	var sawSKU string
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			if req.GPU != nil {
				sawSKU = req.GPU.ID
				if req.GPU.Count != 2 {
					t.Errorf("gpu.count = %d, want 2 (from requirements)", req.GPU.Count)
				}
			}
			return 201, `{"id":"rp-auto-1"}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	inst := okInst()
	inst.Gpu = nil // PENDING shell, GPU not yet resolved
	inst.Spec = &provisionerv1.Spec{
		Requirements: &provisionerv1.ResourceRequirements{
			Sku:      "NVIDIA RTX A5000",
			GpuCount: 2,
		},
	}

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), inst, nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if sawSKU != "NVIDIA RTX A5000" {
		t.Errorf("POST gpu.id = %q, want SKU resolved from requirements", sawSKU)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
}

func TestDeploy_WalksSKUsAndCloudsWhenAutoProvisioned(t *testing.T) {
	// Class-only / min-vram requirements (no explicit SKU): v2 places one
	// GPU type in one cloud per create, so the deployer walks the
	// cheapest-first match list itself, each SKU on SECURE then COMMUNITY,
	// moving on when RunPod refuses for capacity. Pinning matches[0] would
	// fail whenever the cheapest is saturated.
	proxyURL := proxyServer(t, http.StatusOK)

	var tried []postedCreate
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			tried = append(tried, postedCreate{req.GPU.ID, req.Cloud})
			if len(tried) < 3 {
				return 400, problem(400, "Bad Request", "There are no longer any instances available with the requested specifications.")
			}
			return 201, `{"id":"rp-multi-1","status":"PROVISIONING"}`
		}
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	inst := okInst()
	inst.Gpu = nil
	inst.Spec = &provisionerv1.Spec{
		Requirements: &provisionerv1.ResourceRequirements{
			MinVramGb: 24,
			GpuCount:  1,
		},
	}

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), inst, nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	m := MatchSKUs(inst.GetSpec().GetRequirements())
	want := []postedCreate{{m[0], cloudSecure}, {m[0], cloudCommunity}, {m[1], cloudSecure}}
	if len(tried) != len(want) {
		t.Fatalf("tried %v, want %v", tried, want)
	}
	for i := range want {
		if tried[i] != want[i] {
			t.Errorf("placement %d = %v, want %v (all: %v)", i, tried[i], want[i], tried)
		}
	}
}

func TestDeploy_DebugShell_OptsIntoSSH(t *testing.T) {
	// debug_shell=true is the operator opt-in: expose sshd on a
	// NAT-mapped 22/tcp with the account's keys injected, and stay on the
	// SECURE cloud where pods get a public IP. Default flow is proxy-only;
	// this verifies the opt-in flips all three on the POST body.
	proxyURL := proxyServer(t, http.StatusOK)

	var sawPorts []string
	var sawStartSSH bool
	var clouds []string
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			sawPorts = req.Ports
			sawStartSSH = req.StartSSH
			clouds = append(clouds, req.Cloud)
			return 201, `{"id":"rp-debug-1"}`
		}
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	dep := okDep()
	dep.DebugShell = true

	c := &collector{}
	if err := p.Deploy(context.Background(), dep, okInst(), nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !sawStartSSH {
		t.Error("startSsh=false on debug_shell=true; want true")
	}
	if len(clouds) != 1 || clouds[0] != cloudSecure {
		t.Errorf("clouds tried = %v, want [SECURE] only", clouds)
	}
	wantPorts := map[string]bool{"8000/http": true, "22/tcp": true}
	got := map[string]bool{}
	for _, p := range sawPorts {
		got[p] = true
	}
	for p := range wantPorts {
		if !got[p] {
			t.Errorf("Ports missing %q; got %v", p, sawPorts)
		}
	}
}

func TestDeploy_HealthTimeout_GoesToFAILED(t *testing.T) {
	// Proxy never returns 2xx -- stays at 503.
	proxyURL := proxyServer(t, http.StatusServiceUnavailable)

	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			return 201, `{"id":"rp-engine-2"}`
		}
		return 500, "{}"
	}}
	client := f.serve(t)
	// Tight timeout so the test finishes fast.
	p := New(client,
		WithSSHReadyWait(30*time.Millisecond, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	c := &collector{}
	err := p.Deploy(context.Background(), okDep(), okInst(), nil, c.emit)
	if err == nil {
		t.Fatal("expected error when /health never returns 2xx")
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDestroy_HappyPath(t *testing.T) {
	var deleteCalls atomic.Int32
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "DELETE" && path == "/v2/pods/rp-engine-9" {
			deleteCalls.Add(1)
			return 204, ""
		}
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client)

	dep := okDep()
	dep.ContainerId = "rp-engine-9"

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, okInst(), nil, c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	if got := deleteCalls.Load(); got != 1 {
		t.Errorf("DELETE calls = %d, want 1", got)
	}
}

func TestDestroy_AlreadyGone_StillTERMINATED(t *testing.T) {
	// No container id on record -- treat as "nothing to do, already
	// terminal." Mirrors sshdocker's idempotent destroy.
	called := atomic.Int32{}
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		called.Add(1)
		return 500, "{}"
	}}
	client := f.serve(t)
	p := New(client)

	dep := okDep()
	dep.ContainerId = ""

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, okInst(), nil, c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	if got := called.Load(); got != 0 {
		t.Errorf("DELETE should not fire when no container id; got %d calls", got)
	}
}

// collector mirrors the sshdocker test helper -- captures every emit
// so tests can assert on the sequence.
type collector struct {
	updates []provisioners.DeployStateUpdate
}

func (c *collector) emit(u provisioners.DeployStateUpdate) { c.updates = append(c.updates, u) }

func (c *collector) lastState() provisionerv1.DeploymentState {
	if len(c.updates) == 0 {
		return provisionerv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	}
	return c.updates[len(c.updates)-1].State
}

func (c *collector) lastEndpoint() string {
	if len(c.updates) == 0 {
		return ""
	}
	return c.updates[len(c.updates)-1].EngineEndpoint
}

func (c *collector) lastContainer() string {
	if len(c.updates) == 0 {
		return ""
	}
	return c.updates[len(c.updates)-1].ContainerID
}
