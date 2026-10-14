package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// fakeRunPod is an httptest handler that dispatches on (method, path)
// and records the last request so tests can assert what we sent. Paths
// carry the /v2 prefix, because tests point WithBaseURL at <srv>/v2 the
// way production points it at https://api.runpod.io/v2.
type fakeRunPod struct {
	t          *testing.T
	lastMethod string
	lastPath   string
	lastQuery  string
	lastBody   []byte
	respond    func(method, path string, body []byte) (status int, response string)
}

func (f *fakeRunPod) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			f.t.Errorf("missing Bearer auth header, got %q", got)
		}
		f.lastMethod = r.Method
		f.lastPath = r.URL.Path
		f.lastQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		f.lastBody = b
		status, body := f.respond(r.Method, r.URL.Path, b)
		if status >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	})
}

// serve stands the fake up and returns a client pointed at its /v2 root.
func (f *fakeRunPod) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return NewClient("test-api-key", WithBaseURL(srv.URL+"/v2"), WithHTTPClient(srv.Client()))
}

func newFake(t *testing.T, respond func(method, path string, body []byte) (int, string)) (*fakeRunPod, *Provider) {
	t.Helper()
	f := &fakeRunPod{t: t, respond: respond}
	return f, New(f.serve(t))
}

// problem renders a v2 problem+json error body.
func problem(status int, title, detail string, errs ...string) string {
	b, _ := json.Marshal(map[string]any{"title": title, "status": status, "detail": detail, "errors": errs})
	return string(b)
}

// v2Pod renders a v2 Pod record the way POST /v2/pods (201) and
// GET /v2/pods/{id} return it, with the host already assigned, no direct
// SSH endpoint yet, and the container not started.
func v2Pod(id, name, gpuID string, cost float64) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "name": name, "image": "runpod/pytorch:2.4.0",
		"status": "PROVISIONING", "actions": []string{"stop", "terminate"},
		"cost": cost, "cloud": "SECURE", "dataCenterId": "US-CA-1", "cudaVersion": "12.8",
		"createdAt": "2026-05-11T18:22:11Z", "startedAt": nil,
		"gpu":     map[string]any{"id": gpuID, "count": 1, "vcpuCount": 9, "memory": 50},
		"ports":   []string{"22/tcp", "8000/http"},
		"ssh":     map[string]any{"proxy": map[string]any{"host": "ssh.runpod.io", "port": 22, "username": id + "-64411eb2", "command": "ssh " + id + "-64411eb2@ssh.runpod.io"}, "direct": nil},
		"runtime": nil,
	})
	return string(b)
}

func okSpec() *provisionerv1.Spec {
	// The Service is responsible for expanding class shorthand into
	// constraints before the adapter sees the spec; in tests we set
	// the expanded constraints directly so the adapter call site
	// gets exercised at face value.
	return &provisionerv1.Spec{
		Id:       "my-pod",
		Provider: provisioners.ProviderRunPod,
		Region:   "US-CA-1",
		Requirements: &provisionerv1.ResourceRequirements{
			MinVramGb: 24,
			MinDiskGb: 20,
			MinRamGb:  16,
		},
	}
}

// postedCreate is the (gpu.id, cloud) of one POST /v2/pods body.
type postedCreate struct {
	gpu, cloud string
}

func TestProvider_Name(t *testing.T) {
	if got := New(nil).Name(); got != provisioners.ProviderRunPod {
		t.Errorf("Name() = %q, want %q", got, provisioners.ProviderRunPod)
	}
}

func TestSpawn_HappyPath(t *testing.T) {
	var sentBody map[string]any
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			_ = json.Unmarshal(body, &sentBody)
			return 201, v2Pod("rp-7c8e", "iplane-my-pod", "NVIDIA RTX A5000", 0.39)
		}
		// The 201 is the full pod record, so there is no follow-up GET.
		t.Errorf("unexpected request %s %s", method, path)
		return 500, "{}"
	})

	inst, err := p.Spawn(context.Background(), okSpec())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetProviderId() != "rp-7c8e" {
		t.Errorf("ProviderId = %q, want rp-7c8e", inst.GetProviderId())
	}
	if inst.GetHourlyRateUsd() != 0.39 {
		t.Errorf("HourlyRateUsd = %v, want 0.39 (the pod's cost)", inst.GetHourlyRateUsd())
	}
	if inst.GetHardware().GetGpuSku() != "NVIDIA RTX A5000" {
		t.Errorf("GpuSku = %q, want cheapest 24GB SKU (A5000)", inst.GetHardware().GetGpuSku())
	}
	// v2's pod record carries no VRAM; the figure comes off the catalog.
	if inst.GetHardware().GetGpuVramMb() != 24*1024 {
		t.Errorf("GpuVramMb = %d, want %d (24 GB)", inst.GetHardware().GetGpuVramMb(), 24*1024)
	}
	if inst.GetRegion() != "US-CA-1" {
		t.Errorf("Region = %q, want US-CA-1", inst.GetRegion())
	}
	if inst.GetCreatedAt() == nil {
		t.Error("CreatedAt not parsed off the create response")
	}

	// Verify what we sent, field by field against CreatePodRequest.
	if name, _ := sentBody["name"].(string); name != "iplane-my-pod" {
		t.Errorf("name = %q, want iplane-my-pod", name)
	}
	if img, _ := sentBody["image"].(string); img != defaultBaseImage {
		t.Errorf("image = %q, want the default base image", img)
	}
	gpu, _ := sentBody["gpu"].(map[string]any)
	if gpu["id"] != "NVIDIA RTX A5000" || gpu["count"] != float64(1) {
		t.Errorf("gpu = %v, want {id: NVIDIA RTX A5000, count: 1}", gpu)
	}
	if gpu["minRamPerGpu"] != float64(16) {
		t.Errorf("gpu.minRamPerGpu = %v, want 16", gpu["minRamPerGpu"])
	}
	if sentBody["disk"] != float64(defaultContainerDiskGB) {
		t.Errorf("disk = %v, want %d", sentBody["disk"], defaultContainerDiskGB)
	}
	dcs, _ := sentBody["dataCenterIds"].([]any)
	if len(dcs) != 1 || dcs[0] != "US-CA-1" {
		t.Errorf("dataCenterIds = %v, want [US-CA-1] (spec set region=US-CA-1)", dcs)
	}
	// Spawn has to be SSH-reachable: secure cloud, keys injected, 22/tcp.
	if sentBody["cloud"] != cloudSecure {
		t.Errorf("cloud = %v, want SECURE", sentBody["cloud"])
	}
	if sentBody["startSsh"] != true {
		t.Errorf("startSsh = %v, want true", sentBody["startSsh"])
	}
	// No v1 field may leak into a v2 body: the schema is closed and RunPod
	// answers an unknown field with 422.
	for _, v1 := range []string{"imageName", "gpuTypeIds", "gpuTypePriority", "gpuCount", "cloudType",
		"computeType", "containerDiskInGb", "volumeInGb", "supportPublicIp", "dockerStartCmd", "minRAMPerGPU"} {
		if _, ok := sentBody[v1]; ok {
			t.Errorf("POST body carries v1 field %q", v1)
		}
	}
}

// v2 places one GPU type per create and never falls back, so the adapter
// walks the cheapest-first list itself. A 400 is RunPod saying this
// combination could not be placed.
func TestSpawn_WalksTheSKUListOnCapacityRefusals(t *testing.T) {
	var tried []postedCreate
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/v2/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			tried = append(tried, postedCreate{req.GPU.ID, req.Cloud})
			if len(tried) == 1 {
				return 400, problem(400, "Bad Request", "There are no longer any instances available with the requested specifications.")
			}
			return 201, v2Pod("rp-xyz", "iplane-my-pod", req.GPU.ID, 1.79)
		}
		t.Errorf("unexpected request %s %s", method, path)
		return 500, "{}"
	})
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{MinVramGb: 80, MinDiskGb: 60, MinRamGb: 64}

	inst, err := p.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	want := MatchSKUs(spec.GetRequirements())
	if len(tried) != 2 || tried[0].gpu != want[0] || tried[1].gpu != want[1] {
		t.Fatalf("tried %v, want the first two of %v in order", tried, want)
	}
	if inst.GetHardware().GetGpuSku() != want[1] {
		t.Errorf("GpuSku = %q, want the SKU that was placed (%q)", inst.GetHardware().GetGpuSku(), want[1])
	}
}

// 403 means the account cannot use this pool (B200, H200 and H100 NVL
// typically need approval). Under v1 one restricted SKU in gpuTypeIds
// failed the whole request; now it is skipped.
func TestSpawn_SkipsAPoolTheAccountCannotUse(t *testing.T) {
	posts := 0
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		posts++
		if posts == 1 {
			return 403, problem(403, "Forbidden", "access denied")
		}
		return 201, v2Pod("rp-ok", "iplane-my-pod", "NVIDIA RTX 4090", 0.39)
	})
	if _, err := p.Spawn(context.Background(), okSpec()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if posts != 2 {
		t.Errorf("POSTs = %d, want 2 (skip the forbidden pool, take the next)", posts)
	}
}

// 422 is a malformed body, 402 is no balance, 401 is a bad key. None is
// fixed by trying another card, so the walk stops at the first one.
func TestSpawn_StopsOnFailuresNoCandidateCanFix(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{422, problem(422, "Unprocessable Entity", "Request validation failed.", "$.gpu.id: value must be one of ..."), "$.gpu.id: value must be one of"},
		{402, problem(402, "Payment Required", "insufficient balance"), "insufficient balance"},
		{401, problem(401, "Unauthorized", "missing bearer token"), "auth failed"},
		{500, problem(500, "Internal Server Error", "upstream timeout"), "upstream timeout"},
	}
	for _, tc := range cases {
		posts := 0
		_, p := newFake(t, func(method, path string, body []byte) (int, string) {
			posts++
			return tc.status, tc.body
		})
		spec := okSpec()
		spec.Requirements = &provisionerv1.ResourceRequirements{MinVramGb: 80}
		_, err := p.Spawn(context.Background(), spec)
		if err == nil {
			t.Fatalf("%d: Spawn should fail", tc.status)
		}
		if posts != 1 {
			t.Errorf("%d: POSTs = %d, want 1 (no fallback)", tc.status, posts)
		}
		var pe *provisioners.ProviderError
		if !errors.As(err, &pe) || pe.HTTP != tc.status {
			t.Errorf("%d: want *ProviderError with HTTP %d, got %#v", tc.status, tc.status, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d: error %q lost the detail %q", tc.status, err, tc.want)
		}
	}
}

// When every candidate refuses, the operator needs RunPod's own words for
// the last refusal, since a rule violation fails the same way everywhere
// and only the detail says which it was.
func TestSpawn_EveryCandidateRefusedKeepsTheLastDetail(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 400, problem(400, "Bad Request", "no GPU available in datacenter US-CA-1")
	})
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{MinVramGb: 80}
	_, err := p.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("expected an error when every placement is refused")
	}
	if !strings.Contains(err.Error(), "no GPU available in datacenter US-CA-1") {
		t.Errorf("error lost RunPod's detail: %q", err)
	}
	if !strings.Contains(err.Error(), "no capacity for any of") {
		t.Errorf("error does not say every candidate was tried: %q", err)
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) || pe.HTTP != 400 {
		t.Errorf("want *ProviderError carrying the 400, got %#v", err)
	}
}

func TestSpawn_NoMatchingSKU(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		t.Error("provider should reject before HTTP")
		return 500, "{}"
	})
	// Asking for more VRAM than any SKU in the runpod catalog provides
	// (no GPU in the table has > 96 GB VRAM).
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{MinVramGb: 200}
	_, err := p.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("Spawn should reject when no SKU satisfies the constraint")
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) {
		t.Errorf("expected *ProviderError, got %T", err)
	}
}

func TestSpawn_SkuOverridesClass(t *testing.T) {
	var tried []string
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		var req createPodRequest
		_ = json.Unmarshal(body, &req)
		tried = append(tried, req.GPU.ID)
		return 400, problem(400, "Bad Request", "no capacity")
	})
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{Sku: "NVIDIA H100 NVL"}
	if _, err := p.Spawn(context.Background(), spec); err == nil {
		t.Fatal("expected the refusal to surface")
	}
	// An explicit SKU is a pin: no fallback to a different card.
	if len(tried) != 1 || tried[0] != "NVIDIA H100 NVL" {
		t.Errorf("tried %v, want [NVIDIA H100 NVL] only", tried)
	}
}

func TestSpawn_EmptyId(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 201, `{"id": "", "status": "PROVISIONING"}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("expected error when RunPod returns empty id")
	}
	if !strings.Contains(err.Error(), "empty pod id") {
		t.Errorf("error should mention the empty pod id, got %q", err.Error())
	}
}

func TestSpawn_ReturnsImmediately_NoSSH(t *testing.T) {
	// Spawn does not wait for SSH. The create response has ssh.direct
	// null (only the proxy endpoint), so the Instance has empty Ssh.
	// Callers that need SSH call WaitForSSHReady afterward (the "Join"
	// pattern).
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 201, v2Pod("rp-async", "iplane-my-pod", "NVIDIA RTX A5000", 0.36)
	})
	inst, err := p.Spawn(context.Background(), okSpec())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("Spawn should mark ACTIVE even without SSH; got %v", inst.GetState())
	}
	// The proxy endpoint is not an SSH target: it carries an interactive
	// shell only.
	if ssh := inst.GetSsh(); ssh != nil && ssh.GetHost() != "" {
		t.Errorf("expected empty SSH right after Spawn; got host=%q", ssh.GetHost())
	}
}

// podWithDirectSSH is a running pod whose 22/tcp is published.
const podWithDirectSSH = `{"id":"rp-natted","name":"iplane-x","status":"RUNNING","dataCenterId":"US-CA-1",
	"gpu":{"id":"NVIDIA RTX A5000","count":1},
	"ssh":{"proxy":{"host":"ssh.runpod.io","port":22,"username":"rp-natted-1"},
	       "direct":{"host":"1.2.3.4","port":22085,"username":"root","command":"ssh root@1.2.3.4 -p 22085"}},
	"runtime":{"uptime":30,"ports":[{"private":22,"public":22085,"type":"tcp","ip":"1.2.3.4"}]}}`

func sshFake(t *testing.T, respond func(method, path string, body []byte) (int, string), opts ...Option) *Provider {
	t.Helper()
	f := &fakeRunPod{t: t, respond: respond}
	return New(f.serve(t), append([]Option{WithSSHProbe(noopSSHProbe)}, opts...)...)
}

func TestWaitForSSHReady_HonorsTheDirectEndpointsPort(t *testing.T) {
	// RunPod NATs the container's port 22 to a public-side port (e.g.
	// 22085). sshTargetFromPod must surface THAT port -- otherwise the
	// operator's ssh / the deployment executor's dial lands on the wrong
	// service or times out.
	p := sshFake(t, func(method, path string, body []byte) (int, string) {
		if path != "/v2/pods/rp-natted" {
			t.Errorf("unexpected %s %s", method, path)
		}
		return 200, podWithDirectSSH
	})
	ssh, err := p.WaitForSSHReady(context.Background(), "rp-natted")
	if err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if ssh.GetHost() != "1.2.3.4" || ssh.GetPort() != 22085 || ssh.GetUser() != "root" {
		t.Errorf("ssh = %+v, want root@1.2.3.4:22085 (NATed external)", ssh)
	}
}

func TestWaitForSSHReady_FallsBackToTheRuntimePortMapping(t *testing.T) {
	// ssh.direct null but runtime already shows 22 published: the same
	// fact from the other field.
	p := sshFake(t, func(method, path string, body []byte) (int, string) {
		return 200, `{"id":"rp-rt","ssh":{"proxy":null,"direct":null},
			"runtime":{"ports":[{"private":8000,"public":null,"type":"http","ip":null},
			                    {"private":22,"public":40022,"type":"tcp","ip":"5.6.7.8"}]}}`
	})
	ssh, err := p.WaitForSSHReady(context.Background(), "rp-rt")
	if err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if ssh.GetHost() != "5.6.7.8" || ssh.GetPort() != 40022 {
		t.Errorf("ssh = %+v, want 5.6.7.8:40022 from runtime.ports", ssh)
	}
}

func TestWaitForSSHReady_PollsUntilTheDirectEndpointAppears(t *testing.T) {
	// Models the real timing: the first GET has only the proxy endpoint;
	// a follow-up GET has the direct one. WaitForSSHReady should bridge
	// from the empty state to the populated one.
	var getCalls atomic.Int32
	p := sshFake(t, func(method, path string, body []byte) (int, string) {
		if method == "GET" && path == "/v2/pods/rp-ssh-test" {
			if getCalls.Add(1) == 1 {
				return 200, v2Pod("rp-ssh-test", "iplane-x", "NVIDIA RTX A5000", 0.36)
			}
			return 200, `{"id":"rp-ssh-test","ssh":{"direct":{"host":"1.2.3.4","port":22,"username":"root"}}}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}, WithSSHReadyWait(2*time.Second, 5*time.Millisecond))

	ssh, err := p.WaitForSSHReady(context.Background(), "rp-ssh-test")
	if err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if ssh == nil || ssh.GetHost() != "1.2.3.4" || ssh.GetPort() != 22 || ssh.GetUser() != "root" {
		t.Errorf("expected root@1.2.3.4:22; got %+v", ssh)
	}
	if got := getCalls.Load(); got < 2 {
		t.Errorf("expected at least 2 GETs (empty then populated); got %d", got)
	}
}

func TestWaitForSSHReady_TimesOut(t *testing.T) {
	// The direct endpoint never arrives. Caller gets an error so they can
	// decide whether to retry, fail loudly, or fall back to a different
	// transport.
	p := sshFake(t, func(method, path string, body []byte) (int, string) {
		return 200, v2Pod("rp-no-ip", "iplane-x", "NVIDIA RTX A5000", 0.36)
	}, WithSSHReadyWait(15*time.Millisecond, 5*time.Millisecond))

	ssh, err := p.WaitForSSHReady(context.Background(), "rp-no-ip")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if ssh != nil {
		t.Errorf("expected nil ssh on timeout; got %+v", ssh)
	}
}

// noopSSHProbe disables the tcp reachability check so tests that only
// exercise the endpoint polling don't need a real listener on the fake
// IP. Tests that DO want to exercise the port-probe path pass their own
// probe function.
func noopSSHProbe(ctx context.Context, host string, port int32) error { return nil }

func TestWaitForSSHReady_RetriesUntilSSHPortReachable(t *testing.T) {
	// The endpoint is published on the first GET; the TCP probe fails
	// the first two times (container booting / sshd starting up) and
	// succeeds on the third.
	var probeCalls atomic.Int32
	flakyProbe := func(ctx context.Context, host string, port int32) error {
		if probeCalls.Add(1) < 3 {
			return errors.New("connect: connection refused")
		}
		return nil
	}
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		return 200, podWithDirectSSH
	}}
	p := New(f.serve(t), WithSSHReadyWait(2*time.Second, 5*time.Millisecond), WithSSHProbe(flakyProbe))

	ssh, err := p.WaitForSSHReady(context.Background(), "rp-natted")
	if err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if ssh == nil || ssh.GetHost() != "1.2.3.4" {
		t.Errorf("expected populated ssh.host; got %+v", ssh)
	}
	if got := probeCalls.Load(); got < 3 {
		t.Errorf("expected at least 3 probe calls; got %d", got)
	}
}

func TestWaitForSSHReady_TimesOutWhenSSHPortNeverOpens(t *testing.T) {
	failProbe := func(ctx context.Context, host string, port int32) error {
		return errors.New("connect: connection refused")
	}
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		return 200, podWithDirectSSH
	}}
	p := New(f.serve(t), WithSSHReadyWait(15*time.Millisecond, 5*time.Millisecond), WithSSHProbe(failProbe))

	ssh, err := p.WaitForSSHReady(context.Background(), "rp-natted")
	if err == nil {
		t.Fatal("expected timeout error when the ssh port never opens")
	}
	// The endpoint came back; the partial SshTarget should be returned so
	// callers can see what they got.
	if ssh == nil || ssh.GetHost() != "1.2.3.4" {
		t.Errorf("expected partial ssh target carrying the published host; got %+v", ssh)
	}
}

func TestWaitForSSHReady_ImmediateHit(t *testing.T) {
	var getCalls atomic.Int32
	p := sshFake(t, func(method, path string, body []byte) (int, string) {
		getCalls.Add(1)
		return 200, podWithDirectSSH
	})
	if _, err := p.WaitForSSHReady(context.Background(), "rp-natted"); err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if got := getCalls.Load(); got != 1 {
		t.Errorf("expected exactly 1 GET (immediate hit); got %d", got)
	}
}

func TestTerminate_HappyPath(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "DELETE" || path != "/v2/pods/rp-7c8e" {
			t.Errorf("got %s %s, want DELETE /v2/pods/rp-7c8e", method, path)
		}
		return 204, ""
	})
	if err := p.Terminate(context.Background(), "rp-7c8e"); err != nil {
		t.Errorf("Terminate: %v", err)
	}
}

func TestTerminate_NotFound_IsIdempotent(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 404, problem(404, "Not Found", "pod not found")
	})
	if err := p.Terminate(context.Background(), "rp-gone"); err != nil {
		t.Errorf("Terminate of not-found pod should be idempotent, got %v", err)
	}
}

func TestTerminate_EmptyID(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		t.Error("provider should reject empty id before HTTP")
		return 500, "{}"
	})
	if err := p.Terminate(context.Background(), ""); err == nil {
		t.Error("Terminate should reject empty providerID")
	}
}

// The action endpoint takes {"action": ...}; start/stop/restart answer
// 200 with the updated pod and terminate answers 204 with no body.
func TestPodAction(t *testing.T) {
	var sent []string
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "POST" || path != "/v2/pods/rp-1/action" {
			t.Errorf("got %s %s, want POST /v2/pods/rp-1/action", method, path)
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if len(req) != 1 {
			t.Errorf("action body = %s, want exactly {\"action\": ...}", body)
		}
		action, _ := req["action"].(string)
		sent = append(sent, action)
		switch action {
		case "stop":
			return 200, `{"id":"rp-1","status":"EXITED","cost":0,"actions":["start","terminate"]}`
		case "terminate":
			return 204, ""
		}
		return 409, problem(409, "Conflict", "action not valid for current pod status")
	})

	pod, err := p.podAction(context.Background(), "rp-1", "stop")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if pod.Status != "EXITED" || pod.Cost != 0 {
		t.Errorf("stop returned %+v, want the updated EXITED pod at cost 0", pod)
	}
	if _, err := p.podAction(context.Background(), "rp-1", "terminate"); err != nil {
		t.Fatalf("terminate (204, no body): %v", err)
	}
	_, err = p.podAction(context.Background(), "rp-1", "restart")
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) || pe.HTTP != 409 || !strings.Contains(err.Error(), "action not valid") {
		t.Errorf("restart on a stopped pod: want the 409 with its detail, got %v", err)
	}
	if strings.Join(sent, ",") != "stop,terminate,restart" {
		t.Errorf("actions sent = %v", sent)
	}
	_ = f
}

func TestDescribe_HappyPath(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "GET" || path != "/v2/pods/rp-natted" {
			t.Errorf("expected GET /v2/pods/rp-natted, got %s %s", method, path)
		}
		return 200, strings.Replace(podWithDirectSSH, `"name":"iplane-x"`, `"name":"iplane-my-pod","cost":0.39`, 1)
	})
	inst, err := p.Describe(context.Background(), "rp-natted")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if inst.GetSsh().GetHost() != "1.2.3.4" || inst.GetSsh().GetPort() != 22085 {
		t.Errorf("Ssh = %+v, want 1.2.3.4:22085", inst.GetSsh())
	}
	if inst.GetHardware().GetGpuVramMb() != 24*1024 {
		t.Errorf("Hardware.GpuVramMb = %d, want %d (24 GB)", inst.GetHardware().GetGpuVramMb(), 24*1024)
	}
	if inst.GetHourlyRateUsd() != 0.39 {
		t.Errorf("HourlyRateUsd = %v, want 0.39", inst.GetHourlyRateUsd())
	}
	if inst.GetRegion() != "US-CA-1" {
		t.Errorf("Region = %q, want US-CA-1", inst.GetRegion())
	}
	if inst.GetSpec().GetTags()[provisioners.TagID] != "my-pod" {
		t.Errorf("name-decoded tag iplane-id = %q, want my-pod", inst.GetSpec().GetTags()[provisioners.TagID])
	}
}

func TestDescribe_NotFound(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 404, problem(404, "Not Found", "pod not found")
	})
	_, err := p.Describe(context.Background(), "rp-gone")
	if err == nil {
		t.Fatal("Describe of missing pod should error")
	}
	if !errors.Is(err, provisioners.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "pod not found") {
		t.Errorf("error lost the problem detail: %v", err)
	}
}

// v2 wraps the list ({"pods": [...]}) and has no name filter, so the
// adapter sends no query and filters client-side.
func TestList_ReadsTheWrappedListAndFiltersClientSide(t *testing.T) {
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "GET" || path != "/v2/pods" {
			t.Errorf("expected GET /v2/pods, got %s %s", method, path)
		}
		return 200, `{"pods":[
			{"id":"rp-7c8e","name":"iplane-my-pod","cost":0.39,"createdAt":"2026-05-11T18:22:11Z","status":"RUNNING"},
			{"id":"rp-else","name":"iplane-other","cost":0.36,"createdAt":"2026-05-11T18:22:11Z","status":"RUNNING"}
		],"pagination":{"nextCursor":null,"hasNextPage":false}}`
	})
	refs, err := p.List(context.Background(), map[string]string{provisioners.TagID: "my-pod"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if f.lastQuery != "" {
		t.Errorf("sent query %q; v2 has no name filter to send", f.lastQuery)
	}
	r := refs[0]
	if r.GetProviderId() != "rp-7c8e" || r.GetProviderState() != "RUNNING" || r.GetHourlyRateUsd() != 0.39 {
		t.Errorf("ref = %+v, want rp-7c8e RUNNING at 0.39", r)
	}
	if r.GetTags()[provisioners.TagID] != "my-pod" {
		t.Errorf("name-decoded tag iplane-id missing")
	}
	if r.GetCreatedAt() == nil {
		t.Error("CreatedAt not parsed")
	}
}

// Every page, not just the first: a pod past the first page is still a
// pod the account is paying for.
func TestList_FollowsThePaginationCursor(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		cursors = append(cursors, c)
		w.Header().Set("Content-Type", "application/json")
		if c == "" {
			_, _ = w.Write([]byte(`{"pods":[{"id":"p1","name":"iplane-a","status":"RUNNING"}],
				"pagination":{"nextCursor":"page-2","hasNextPage":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"pods":[{"id":"p2","name":"iplane-b","status":"RUNNING"}],
			"pagination":{"nextCursor":null,"hasNextPage":false}}`))
	}))
	t.Cleanup(srv.Close)
	p := New(NewClient("k", WithBaseURL(srv.URL+"/v2"), WithHTTPClient(srv.Client())))

	refs, err := p.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want both pages' pods", len(refs))
	}
	if strings.Join(cursors, ",") != ",page-2" {
		t.Errorf("cursors sent = %q, want first page then page-2", cursors)
	}
}

func TestIsActiveProviderState(t *testing.T) {
	p := New(nil)
	cases := []struct {
		state string
		want  bool
	}{
		{"PROVISIONING", true},
		{"STARTING", true},
		{"RUNNING", true},
		{"running", true},
		{"EXITED", false},
		{"ERROR", false},
		{"TERMINATED", false},
		{"", false},
	}
	for _, c := range cases {
		if got := p.IsActiveProviderState(c.state); got != c.want {
			t.Errorf("IsActiveProviderState(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestMatchSKUs(t *testing.T) {
	// VRAM constraints should narrow to the SKUs that satisfy them,
	// cheapest first.
	cases := []struct {
		name         string
		reqs         *provisionerv1.ResourceRequirements
		wantFirst    string // expected first SKU (cheapest matching)
		wantNonEmpty bool
	}{
		{"24GB VRAM", &provisionerv1.ResourceRequirements{MinVramGb: 24}, "NVIDIA RTX A5000", true},
		{"80GB VRAM", &provisionerv1.ResourceRequirements{MinVramGb: 80}, "NVIDIA A100 80GB PCIe", true},
		{"200GB VRAM (above all SKUs)", &provisionerv1.ResourceRequirements{MinVramGb: 200}, "", false},
		{"40GB+128RAM (H100 PCIe is cheapest with 128 GB system RAM)", &provisionerv1.ResourceRequirements{MinVramGb: 40, MinRamGb: 128}, "NVIDIA H100 PCIe", true},
	}
	for _, c := range cases {
		got := MatchSKUs(c.reqs)
		if c.wantNonEmpty && len(got) == 0 {
			t.Errorf("%s: got empty, want at least one match", c.name)
			continue
		}
		if !c.wantNonEmpty && len(got) != 0 {
			t.Errorf("%s: got %v, want empty", c.name, got)
			continue
		}
		if c.wantNonEmpty && got[0] != c.wantFirst {
			t.Errorf("%s: first SKU = %q, want %q (cheapest matching)", c.name, got[0], c.wantFirst)
		}
	}
}

func TestMatchSKUs_CappedAtMaxSKUsPerRequest(t *testing.T) {
	// class=small expands to min_vram_gb=24, which matches >10 SKUs in
	// the catalog. The cap bounds how many creates one Spawn can issue
	// and keeps a small request from walking up into frontier prices.
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 24})
	if len(got) > MaxSKUsPerRequest {
		t.Errorf("got %d SKUs (%v), want <= %d (cap)", len(got), got, MaxSKUsPerRequest)
	}
	for _, sku := range got {
		if sku == "NVIDIA B200" || sku == "NVIDIA H200" {
			t.Errorf("class=small returned frontier SKU %q; cap should keep only cheap matches", sku)
		}
	}
}

func TestParseErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "problem+json detail",
			body: `{"title":"Not Found","status":404,"detail":"pod not found"}`,
			want: "pod not found",
		},
		{
			name: "problem+json validation errors follow the detail",
			body: `{"title":"Unprocessable Entity","status":422,"detail":"Request validation failed.","errors":["$: additional properties 'bogus' not allowed","$.gpu.count: must be >= 1"]}`,
			want: "Request validation failed.: $: additional properties 'bogus' not allowed; $.gpu.count: must be >= 1",
		},
		{
			name: "problem+json with only a title",
			body: `{"title":"Too Many Requests","status":429}`,
			want: "Too Many Requests",
		},
		{
			name: "v1-era message key still reads",
			body: `{"message":"rate limited"}`,
			want: "rate limited",
		},
		{
			name: "unrecognized shape returns raw body",
			body: `<html>500 Internal Server Error</html>`,
			want: "<html>500 Internal Server Error</html>",
		},
	}
	for _, c := range cases {
		if got := parseErrorMessage([]byte(c.body)); got != c.want {
			t.Errorf("%s:\n  body: %s\n  got:  %q\n  want: %q", c.name, c.body, got, c.want)
		}
	}
}

func TestClassifySKU(t *testing.T) {
	if got := classifySKU("NVIDIA GeForce RTX 4090"); got != provisioners.GPUClassSmall {
		t.Errorf("classifySKU(4090) = %q, want small", got)
	}
	if got := classifySKU("Custom Unknown GPU"); got != "" {
		t.Errorf("classifySKU(unknown) = %q, want empty", got)
	}
}
