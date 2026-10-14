//go:build runpod_readonly

// Read-only checks of the RunPod adapter against the live REST v2 API.
// Every call is a GET: the pod list, the GPU and datacenter catalogs, the
// account's SSH keys, and the network volumes. Nothing is created,
// started, stopped or modified, so this costs nothing.
//
// It has its own build tag, separate from smoke_runpod, because
// `make smoke-runpod` rents a pod on every run; this file can never be
// compiled alongside that test. As a second guard the client's transport
// refuses any method other than GET before it reaches the network.
//
// Run:
//
//	export RUNPOD_API_KEY=...
//	make runpod-v2-check
//
// What it proves: the adapter's v2 decoders read what RunPod actually
// returns. The unit tests prove the same against fixtures written from
// RunPod's OpenAPI document, which is a claim about the document rather
// than about the service.
package smoke_runpod

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	smokeproviders "github.com/inference-book/inference-plane/tests/smoke-providers"
)

// getOnly fails any request that is not a GET, so a mistake in this file
// (or in the adapter) cannot turn a read-only check into a rental.
type getOnly struct{ inner http.RoundTripper }

func (g getOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("read-only check refused %s %s", r.Method, r.URL)
	}
	return g.inner.RoundTrip(r)
}

func readOnlyProvider(t *testing.T) *runpod.Provider {
	t.Helper()
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set; skipping (real-API read-only check)")
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: getOnly{http.DefaultTransport}}
	return runpod.New(runpod.NewClient(apiKey, runpod.WithHTTPClient(hc)))
}

// GET /v2/pods, every page.
func TestRunPodReadOnly_Pods(t *testing.T) {
	smokeproviders.RunList(t, readOnlyProvider(t))
}

// GET /v2/catalog/gpus at one and eight cards, both clouds, plus
// GET /v2/catalog/datacenters.
func TestRunPodReadOnly_Catalog(t *testing.T) {
	p := readOnlyProvider(t)
	for _, n := range []int32{1, 8} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		got, err := p.Candidates(ctx, &provisionerv1.ResourceRequirements{MinVramGb: 80, GpuCount: n})
		cancel()
		if err != nil {
			t.Fatalf("Candidates(%d x >=80 GB): %v", n, err)
		}
		t.Logf("%d x >=80 GB: %d candidates", n, len(got))
		for i, c := range got {
			if i >= 8 {
				t.Logf("  ... %d more", len(got)-8)
				break
			}
			t.Logf("  %-24s %-10s $%7.2f/h  stock=%-6s cloud=%-9s storage=%s",
				c.GetSku(), c.GetRegion(), c.GetPriceUsdPerHour(),
				c.GetAttrs()["stock_status"], c.GetAttrs()["cloud"], c.GetAttrs()["datacenter_storage"])
			if c.GetPriceUsdPerHour() <= 0 || c.GetVramGbPerGpu() < 80 {
				t.Errorf("implausible candidate %+v: a decode is reading a field RunPod does not send", c)
			}
		}
	}
}

// GET /v2/account/ssh-keys.
func TestRunPodReadOnly_SSHKeys(t *testing.T) {
	p := readOnlyProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	blob, err := p.FetchUserPubKey(ctx)
	if err != nil {
		t.Fatalf("FetchUserPubKey: %v", err)
	}
	t.Logf("account has %d registered SSH key line(s)", strings.Count(blob, "\n"))
	for _, line := range strings.Split(strings.TrimSpace(blob), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			comment := ""
			if len(f) > 2 {
				comment = strings.Join(f[2:], " ")
			}
			t.Logf("  %s ...%s %s", f[0], f[1][max(0, len(f[1])-8):], comment)
		}
	}
}

// GET /v2/network-volumes, every page.
func TestRunPodReadOnly_Volumes(t *testing.T) {
	p := readOnlyProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vols, err := p.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	t.Logf("account has %d network volume(s)", len(vols))
	for _, v := range vols {
		t.Logf("  %s %q in %s, %d GB", v.ID, v.Name, v.Region, v.SizeGB)
	}
}
