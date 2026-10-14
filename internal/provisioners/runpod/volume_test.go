package runpod

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// stageWithFastPoll runs StageModel with a sub-millisecond poll cadence
// so tests exercise the poll/terminate loop without sleeping between
// ticks.
func stageWithFastPoll(t *testing.T, p *Provider, spec provisioners.StageSpec) error {
	t.Helper()
	p.stageInterval = time.Millisecond
	p.stageBudget = 5 * time.Second
	return p.StageModel(context.Background(), spec)
}

func TestEnsureVolume_CreatesWhenAbsent(t *testing.T) {
	var createBody networkVolumeCreate
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "GET" && path == "/v2/network-volumes":
			return 200, `{"networkVolumes":[],"pagination":{"nextCursor":null,"hasNextPage":false}}`
		case method == "POST" && path == "/v2/network-volumes":
			_ = json.Unmarshal(body, &createBody)
			return 201, `{"id":"vol-new","name":"iplane-cache-EU-RO-1","size":100,"dataCenter":"EU-RO-1","type":"STANDARD"}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	})
	ref, err := p.EnsureVolume(context.Background(), provisioners.VolumeSpec{
		Name: "iplane-cache-EU-RO-1", Region: "EU-RO-1", SizeGB: 100,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if ref.ID != "vol-new" || ref.Region != "EU-RO-1" {
		t.Errorf("ref = %+v, want id vol-new / region EU-RO-1", ref)
	}
	if createBody.Name != "iplane-cache-EU-RO-1" || createBody.Size != 100 || createBody.DataCenter != "EU-RO-1" {
		t.Errorf("create body = %+v, want name/size/dc set", createBody)
	}
	_ = f
}

func TestEnsureVolume_ReusesExistingByNameAndRegion(t *testing.T) {
	posted := false
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "GET" && path == "/v2/network-volumes":
			return 200, `{"networkVolumes":[
				{"id":"vol-old","name":"iplane-cache-EU-RO-1","size":100,"dataCenter":"EU-RO-1","type":"STANDARD"},
				{"id":"vol-other","name":"iplane-cache-EU-RO-1","size":50,"dataCenter":"US-CA-1","type":"STANDARD"}],
				"pagination":{"nextCursor":null,"hasNextPage":false}}`
		case method == "POST" && path == "/v2/network-volumes":
			posted = true
			return 201, `{"id":"vol-dup"}`
		}
		return 500, "{}"
	})
	ref, err := p.EnsureVolume(context.Background(), provisioners.VolumeSpec{
		Name: "iplane-cache-EU-RO-1", Region: "EU-RO-1", SizeGB: 100,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if ref.ID != "vol-old" {
		t.Errorf("ref.ID = %q, want vol-old (region match); vol-other is the wrong region", ref.ID)
	}
	if posted {
		t.Error("EnsureVolume created a duplicate volume instead of reusing the existing one")
	}
}

func TestStageModel_PollsLogsForDoneThenDeletesPod(t *testing.T) {
	var stageCmd string
	var deleted string
	var createdFlavors []string
	polls := 0
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "GET" && path == "/v2/catalog/cpus":
			return 200, cpuCatalogFixture
		case method == "POST" && path == "/v2/pods":
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			stageCmd = strings.Join(req.Cmd, " ")
			if req.Mounts == nil || len(req.Mounts.Network) != 1 ||
				req.Mounts.Network[0] != (networkMount{VolumeID: "vol-1", Path: "/models"}) {
				t.Errorf("stage pod mounts = %+v, want vol-1 at /models", req.Mounts)
			}
			if req.GPU != nil || req.CPU == nil || req.CPU.VCPUCount != stageVCPUs {
				t.Errorf("stage pod compute gpu=%+v cpu=%+v, want a %d-vCPU CPU pod", req.GPU, req.CPU, stageVCPUs)
			}
			createdFlavors = append(createdFlavors, req.CPU.ID)
			return 201, `{"id":"stage-pod","status":"PROVISIONING"}`
		case method == "GET" && path == "/v2/pods/stage-pod/logs":
			polls++
			if polls < 2 {
				// container still downloading -- marker not printed yet.
				return 200, `data: {"source":"container","line":"downloading shards..."}`
			}
			return 200, `data: {"source":"container","line":"` + stageSentinelDone + `"}`
		case method == "DELETE" && path == "/v2/pods/stage-pod":
			deleted = "stage-pod"
			return 204, ""
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	})
	// Zero the poll interval so the test doesn't sleep 10s between polls.
	p2 := p
	err := stageWithFastPoll(t, p2, provisioners.StageSpec{
		VolumeID: "vol-1", Region: "EU-RO-1", Model: "Qwen/Qwen2.5-32B-Instruct-AWQ", MountPath: "/models",
	})
	if err != nil {
		t.Fatalf("StageModel: %v", err)
	}
	if !strings.Contains(stageCmd, "hf download Qwen/Qwen2.5-32B-Instruct-AWQ") {
		t.Errorf("stage cmd missing download: %q", stageCmd)
	}
	if !strings.Contains(stageCmd, stageSentinelDone) || !strings.Contains(stageCmd, stageSentinelFail) {
		t.Errorf("stage cmd missing completion markers: %q", stageCmd)
	}
	if deleted != "stage-pod" {
		t.Error("staging pod was not torn down after completion")
	}
	// The cheapest flavor in stock in the volume's datacenter at 4 vCPUs:
	// cpu3c is cheaper but out of stock in EU-RO-1, cpu3g has none here,
	// cpu5x cannot take 4 vCPUs.
	if len(createdFlavors) != 1 || createdFlavors[0] != "cpu5c-2-4" {
		t.Errorf("staging flavors tried = %v, want [cpu5c-2-4]", createdFlavors)
	}
}

// cpuCatalogFixture is GET /v2/catalog/cpus?include=AVAILABILITY in the
// documented shape, built so exactly one flavor qualifies for EU-RO-1.
const cpuCatalogFixture = `{"cpus":[
 {"id":"cpu3c-2-4","name":"Compute-Optimized","group":"Gen 3","vcpu":{"min":2,"max":32},"ramGbPerVcpu":2,
  "price":{"securePerVcpu":0.02,"serverlessPerVcpu":0.03},"availability":"LOW",
  "dataCenters":[{"id":"EU-RO-1","name":"Europe Romania 1","availability":"NONE"},{"id":"US-KS-2","name":"US Kansas 2","availability":"HIGH"}]},
 {"id":"cpu3g-2-8","name":"General Purpose","group":"Gen 3","vcpu":{"min":2,"max":32},"ramGbPerVcpu":4,
  "price":{"securePerVcpu":0.03,"serverlessPerVcpu":0.04},"availability":"HIGH",
  "dataCenters":[{"id":"US-KS-2","name":"US Kansas 2","availability":"HIGH"}]},
 {"id":"cpu5x-8-16","name":"Big","group":"Gen 5","vcpu":{"min":8,"max":64},"ramGbPerVcpu":2,
  "price":{"securePerVcpu":0.01,"serverlessPerVcpu":0.02},"availability":"HIGH",
  "dataCenters":[{"id":"EU-RO-1","name":"Europe Romania 1","availability":"HIGH"}]},
 {"id":"cpu5c-2-4","name":"Compute-Optimized","group":"Gen 5","vcpu":{"min":2,"max":32},"ramGbPerVcpu":2.5,
  "price":{"securePerVcpu":0.04,"serverlessPerVcpu":0.03},"availability":"MEDIUM",
  "dataCenters":[{"id":"EU-RO-1","name":"Europe Romania 1","availability":"MEDIUM"}]}
]}`

// A staging pod nobody can place is reported before anything is rented.
func TestStageModel_NoCPUFlavorInTheRegion(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method == "GET" && path == "/v2/catalog/cpus" {
			return 200, cpuCatalogFixture
		}
		t.Errorf("unexpected %s %s: nothing should be created", method, path)
		return 500, "{}"
	})
	err := stageWithFastPoll(t, p, provisioners.StageSpec{VolumeID: "vol-1", Region: "AP-IN-1", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "no CPU flavor") || !strings.Contains(err.Error(), "AP-IN-1") {
		t.Errorf("StageModel err = %v, want no-flavor error naming AP-IN-1", err)
	}
}

// Volumes past the first page are still volumes: EnsureVolume must see
// them, or it creates a duplicate cache.
func TestListVolumes_FollowsThePaginationCursor(t *testing.T) {
	pages := 0
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		pages++
		if pages == 1 {
			return 200, `{"networkVolumes":[{"id":"v1","name":"a","size":10,"dataCenter":"EU-RO-1","type":"STANDARD"}],
				"pagination":{"nextCursor":"c2","hasNextPage":true}}`
		}
		return 200, `{"networkVolumes":[{"id":"v2","name":"b","size":20,"dataCenter":"US-KS-2","type":"STANDARD"}],
			"pagination":{"nextCursor":null,"hasNextPage":false}}`
	}}
	p := New(f.serve(t))
	refs, err := p.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(refs) != 2 || refs[1].ID != "v2" || refs[1].Region != "US-KS-2" || refs[1].SizeGB != 20 {
		t.Errorf("refs = %+v, want both pages", refs)
	}
	if f.lastQuery != "cursor=c2" {
		t.Errorf("last query = %q, want cursor=c2", f.lastQuery)
	}
}

func TestDeleteVolume(t *testing.T) {
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 204, ""
	})
	if err := p.DeleteVolume(context.Background(), "vol-9"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if f.lastMethod != "DELETE" || f.lastPath != "/v2/network-volumes/vol-9" {
		t.Errorf("sent %s %s, want DELETE /v2/network-volumes/vol-9", f.lastMethod, f.lastPath)
	}
}

func TestStageModel_FailMarkerErrors(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "GET" && path == "/v2/catalog/cpus":
			return 200, cpuCatalogFixture
		case method == "POST" && path == "/v2/pods":
			return 201, `{"id":"stage-pod","status":"PROVISIONING"}`
		case method == "GET" && path == "/v2/pods/stage-pod/logs":
			return 200, `data: {"source":"container","line":"` + stageSentinelFail + `"}`
		case method == "DELETE" && path == "/v2/pods/stage-pod":
			return 204, ""
		}
		return 500, "{}"
	})
	err := stageWithFastPoll(t, p, provisioners.StageSpec{VolumeID: "vol-1", Region: "EU-RO-1", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "failure") {
		t.Errorf("StageModel err = %v, want a staging-failure error", err)
	}
}

func TestStageSignal(t *testing.T) {
	done, failed := stageSignal([]byte("pip install ...\n" + stageSentinelDone + "\n"))
	if !done || failed {
		t.Errorf("done marker: done=%v failed=%v, want true/false", done, failed)
	}
	done, failed = stageSignal([]byte("Traceback ...\n" + stageSentinelFail + "\n"))
	if done || !failed {
		t.Errorf("fail marker: done=%v failed=%v, want false/true", done, failed)
	}
	// Failure wins if somehow both appear, so a failed run is never
	// mistaken for success.
	done, failed = stageSignal([]byte(stageSentinelDone + " " + stageSentinelFail))
	if done || !failed {
		t.Errorf("both markers: done=%v failed=%v, want false/true", done, failed)
	}
	if done, failed := stageSignal([]byte("still downloading shards")); done || failed {
		t.Errorf("no marker: done=%v failed=%v, want false/false", done, failed)
	}
}
