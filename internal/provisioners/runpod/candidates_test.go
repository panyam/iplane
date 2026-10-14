package runpod

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// secureGPUsFixture is GET /v2/catalog/gpus?include=AVAILABILITY&cloud=SECURE
// in the documented GpuType shape. Prices are per card. Each unavailable
// row is unavailable a different way, so a check on one signal cannot pass
// for a check on all of them.
const secureGPUsFixture = `{"gpus":[
 {"id":"NVIDIA A100 80GB PCIe","name":"A100 PCIe","pool":"AMPERE_80","manufacturer":"NVIDIA","memory":80,
  "secure":true,"community":true,"price":{"secure":1.19,"community":0.89},"maxCount":{"secure":8,"community":4},
  "availability":"LOW","dataCenters":[{"id":"US-KS-2","name":"US Kansas 2","availability":"LOW"}]},
 {"id":"NVIDIA A100-SXM4-80GB","name":"A100 SXM","pool":"AMPERE_80","manufacturer":"NVIDIA","memory":80,
  "secure":true,"community":true,"price":{"secure":1.39,"community":1.09},"maxCount":{"secure":8,"community":8},
  "availability":"MEDIUM","dataCenters":[{"id":"AP-IN-1","name":"India 1","availability":"LOW"},
                                         {"id":"EUR-IS-1","name":"Iceland 1","availability":"MEDIUM"}]},
 {"id":"NVIDIA H100 PCIe","name":"H100 PCIe","pool":"HOPPER_80","manufacturer":"NVIDIA","memory":80,
  "secure":true,"community":true,"price":{"secure":2.39,"community":1.99},"maxCount":{"secure":8,"community":8},
  "availability":"NONE"},
 {"id":"NVIDIA L4","name":"L4","pool":"ADA_24","manufacturer":"NVIDIA","memory":24,
  "secure":true,"community":false,"price":{"secure":0.43,"community":0},"maxCount":{"secure":8,"community":0},
  "availability":"HIGH","dataCenters":[{"id":"EUR-IS-1","name":"Iceland 1","availability":"NONE"},
                                       {"id":"US-MO-1","name":"Missouri 1","availability":"HIGH"}]},
 {"id":"NVIDIA H100 80GB HBM3","name":"H100 SXM","pool":"HOPPER_80","manufacturer":"NVIDIA","memory":80,
  "secure":true,"community":true,"price":{"secure":2.69,"community":2.39},"maxCount":{"secure":8,"community":8}},
 {"id":"NVIDIA H100 NVL","name":"H100 NVL","pool":null,"manufacturer":"NVIDIA","memory":94,
  "secure":true,"community":false,"price":{"secure":0,"community":0},"maxCount":{"secure":8,"community":0},
  "availability":"LOW"},
 {"id":"NVIDIA GeForce RTX 4090","name":"RTX 4090","pool":"ADA_24","manufacturer":"NVIDIA","memory":24,
  "secure":true,"community":true,"price":{"secure":0.44,"community":0.31},"maxCount":{"secure":8,"community":4},
  "availability":"NONE"}
]}`

// communityGPUsFixture is the same read at cloud=COMMUNITY. The 4090 is
// only in stock here; the A100 SXM is in stock on both and must be
// reported once, on SECURE, where a create would put it first.
const communityGPUsFixture = `{"gpus":[
 {"id":"NVIDIA A100-SXM4-80GB","name":"A100 SXM","memory":80,"secure":true,"community":true,
  "price":{"secure":1.39,"community":1.09},"maxCount":{"secure":8,"community":8},
  "availability":"HIGH","dataCenters":[{"id":"US-TX-1","name":"Texas 1","availability":"HIGH"}]},
 {"id":"NVIDIA H100 PCIe","name":"H100 PCIe","memory":80,"secure":true,"community":true,
  "price":{"secure":2.39,"community":1.99},"maxCount":{"secure":8,"community":8},"availability":"NONE"},
 {"id":"NVIDIA GeForce RTX 4090","name":"RTX 4090","memory":24,"secure":true,"community":true,
  "price":{"secure":0.44,"community":0.31},"maxCount":{"secure":8,"community":4},
  "availability":"HIGH","dataCenters":[{"id":"US-TX-1","name":"Texas 1","availability":"HIGH"}]}
]}`

// catalogRequest is one request the catalog fake saw.
type catalogRequest struct {
	method, path string
	query        url.Values
}

// catalogServer answers the catalog reads from the given bodies and
// records every request.
func catalogServer(t *testing.T, secure, community string, seen *[]catalogRequest) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, catalogRequest{r.Method, r.URL.Path, r.URL.Query()})
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/catalog/gpus" && r.URL.Query().Get("cloud") == "COMMUNITY":
			_, _ = w.Write([]byte(community))
		case r.URL.Path == "/v2/catalog/gpus":
			_, _ = w.Write([]byte(secure))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(NewClient("test-api-key", WithBaseURL(srv.URL+"/v2"), WithHTTPClient(srv.Client())))
}

func candidateProvider(t *testing.T, seen *[]catalogRequest) *Provider {
	return catalogServer(t, secureGPUsFixture, communityGPUsFixture, seen)
}

// A type with no stock at the width, no availability reading, or no price
// on the cloud cannot be had, and putting it in front of an operator gives
// them a row they cannot act on.
func TestCandidatesSkipUnavailableTypes(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	unavailable := map[string]string{
		"NVIDIA H100 PCIe":      "availability NONE on both clouds",
		"NVIDIA H100 80GB HBM3": "no availability reported",
		"NVIDIA H100 NVL":       "stock reported but no price",
	}
	for _, c := range got {
		if why, bad := unavailable[c.GetSku()]; bad {
			t.Errorf("offered %q as a candidate (%s): %+v", c.GetSku(), why, c)
		}
	}
	if len(got) == 0 {
		t.Fatal("no candidates; the fixture has two available 80 GB types")
	}
}

// Availability is a property of a card AT a width, not of a card. Probing live,
// 35 of 48 types were obtainable as one GPU and 11 of 48 as eight, so a query
// that hardcoded the count would report capacity that is not there.
func TestCandidatesAskAtTheRequestedWidth(t *testing.T) {
	var seen []catalogRequest
	p := candidateProvider(t, &seen)

	if _, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80, GpuCount: 8}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	clouds := map[string]bool{}
	for _, r := range seen {
		if r.path != "/v2/catalog/gpus" {
			continue
		}
		clouds[r.query.Get("cloud")] = true
		if r.query.Get("count") != "8" {
			t.Errorf("gpu catalog read at count=%q, want 8", r.query.Get("count"))
		}
		// count and cloud are only honoured alongside include=AVAILABILITY,
		// which in turn requires a product.
		if r.query.Get("include") != "AVAILABILITY" || r.query.Get("product") != "POD" {
			t.Errorf("gpu catalog query %v lacks include=AVAILABILITY&product=POD", r.query)
		}
	}
	if !clouds[cloudSecure] || !clouds[cloudCommunity] || len(clouds) != 2 {
		t.Errorf("clouds read = %v, want SECURE and COMMUNITY", clouds)
	}
}

// An absent gpu_count means one, matching the rest of the provisioning path.
func TestCandidatesDefaultToOneGPU(t *testing.T) {
	var seen []catalogRequest
	if _, err := candidateProvider(t, &seen).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, r := range seen {
		if r.path == "/v2/catalog/gpus" && r.query.Get("count") != "1" {
			t.Errorf("gpu catalog read at count=%q, want 1", r.query.Get("count"))
		}
	}
}

// RunPod's live catalog carries 48 types against our 16, and the extra ones are
// shapes we have never validated a deploy against. Offering something a create
// would then refuse is the failure this shares with the other two adapters.
func TestCandidatesFilterThroughTheSameResolverAsSpawn(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range got {
		if LookupSKU(c.GetSku()) == nil {
			t.Errorf("candidate %q is not in our catalog", c.GetSku())
		}
		if c.GetVramGbPerGpu() < 80 {
			t.Errorf("candidate %q has %d GB VRAM, below the stated floor", c.GetSku(), c.GetVramGbPerGpu())
		}
	}
}

// RunPod rents a GPU type and picks the datacenter itself, so there is no host
// to identify, no offer to name, and no region to record. Empty is the honest
// answer; a synthesized id would point at something that was never a place.
func TestCandidatesLeaveHostOfferAndRegionEmpty(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range got {
		if c.GetHostId() != "" || c.GetOfferId() != "" || c.GetRegion() != "" {
			t.Errorf("HostID=%q OfferID=%q Region=%q, want all empty on a provider with none of those concepts",
				c.GetHostId(), c.GetOfferId(), c.GetRegion())
		}
	}
}

// The live price is half the reason to make the call. The fixture's 1.39
// disagrees with the static catalog's 1.79 on purpose.
func TestCandidatesUseTheLivePriceNotTheCatalog(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range got {
		if c.GetPriceUsdPerHour() != 1.39 {
			t.Errorf("%s price = %v, want the live secure 1.39", c.GetRegion(), c.GetPriceUsdPerHour())
		}
	}
	if catalog := LookupSKU("NVIDIA A100-SXM4-80GB"); catalog != nil &&
		got[0].GetPriceUsdPerHour() == catalog.PriceUSDPerHour {
		t.Error("price came from the static catalog; the network call bought us nothing")
	}
}

// v2 lists a price for ONE card; the rate for a pod is that times the
// count. GraphQL priced the whole shape, so forgetting the multiply would
// quote an eight-card pod at a single card's rate.
func TestCandidatesPriceTheWholeShape(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB", GpuCount: 8})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range got {
		if math.Abs(c.GetPriceUsdPerHour()-8*1.39) > 1e-9 {
			t.Errorf("%s price = %v, want 8 x 1.39", c.GetRegion(), c.GetPriceUsdPerHour())
		}
		if c.GetGpuCount() != 8 {
			t.Errorf("gpu count = %d, want 8", c.GetGpuCount())
		}
	}
}

// Read-only by contract, and on REST the method says so.
func TestCandidatesNeverMutate(t *testing.T) {
	var seen []catalogRequest
	if _, err := candidateProvider(t, &seen).Candidates(context.Background(), &provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no requests made")
	}
	for _, r := range seen {
		if r.method != http.MethodGet || !strings.HasPrefix(r.path, "/v2/catalog/") {
			t.Errorf("Candidates sent %s %s; only catalog GETs are allowed", r.method, r.path)
		}
	}
}

// The stock level stays in Attrs, in the casing the attr
// always had ("Low" rather than v2's "LOW"), so capacity samples recorded
// before and after the migration compare.
func TestCandidatesCarryStockStatusAsAProviderAttr(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].GetAttrs()["stock_status"] != "Medium" {
		t.Errorf("stock_status = %q, want Medium", got[0].GetAttrs()["stock_status"])
	}
	if got[0].GetAttrs()["cloud"] != cloudSecure {
		t.Errorf("cloud attr = %q, want SECURE", got[0].GetAttrs()["cloud"])
	}
}

// v2 publishes no bid price and the create takes no interruptible flag, so
// there is nothing RunPod can offer a reclaimable search, and asking the
// catalog would only spend a request to find that out.
func TestCandidatesOfferNothingReclaimable(t *testing.T) {
	var seen []catalogRequest
	got, err := candidateProvider(t, &seen).Candidates(context.Background(), &provisionerv1.ResourceRequirements{
		MinVramGb:     80,
		ReclaimPolicy: provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED,
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d reclaimable candidates, want none: %+v", len(got), got)
	}
	if len(seen) != 0 {
		t.Errorf("made %d catalog reads for a question the catalog cannot answer", len(seen))
	}
}

// The exact figure is the same literal all three adapters assert for an
// A100 80GB, so drift shows up here rather than as a silent disagreement
// between vendors about one physical card (#323).
func TestCandidatesCarryExactCardCapacity(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	c := got[0]
	if c.GetVramGbPerGpu() != 80 {
		t.Errorf("advertised VRAM = %d, want the API's 80", c.GetVramGbPerGpu())
	}
	if want := int64(85_899_345_920); c.GetVramBytesPerGpu() != want {
		t.Errorf("exact VRAM = %d, want %d (80 GiB)", c.GetVramBytesPerGpu(), want)
	}
}

// A type out of stock on SECURE but in stock on COMMUNITY is a candidate,
// at the community price, because the deploy path tries COMMUNITY next.
func TestCandidatesFallBackToTheCommunityCloud(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA GeForce RTX 4090"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want the 4090 once: %+v", len(got), got)
	}
	c := got[0]
	if c.GetPriceUsdPerHour() != 0.31 || c.GetAttrs()["cloud"] != cloudCommunity {
		t.Errorf("got %s at $%v on %s, want COMMUNITY 0.31",
			c.GetSku(), c.GetPriceUsdPerHour(), c.GetAttrs()["cloud"])
	}
}

// The response example RunPod's OpenAPI publishes for GET /v2/catalog/gpus,
// verbatim. If the decode drifts from the vendor's shape, this is where it
// shows, rather than in a fixture written to match the decoder.
func TestCandidatesParseTheDocumentedCatalogExample(t *testing.T) {
	const documented = `{"gpus": [{"id": "NVIDIA GeForce RTX 4090", "name": "RTX 4090", "pool": "ADA_24", "manufacturer": "NVIDIA", "memory": 24, "secure": true, "community": true, "price": {"secure": 0.44, "community": 0.31, "serverless": 1.1}, "maxCount": {"secure": 8, "community": 4}, "availability": "HIGH", "dataCenters": [{"id": "US-KS-2", "name": "US Kansas 2", "availability": "HIGH"}]}]}`
	p := catalogServer(t, documented, `{"gpus":[]}`, nil)

	got, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA GeForce RTX 4090"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.GetSku() != "NVIDIA GeForce RTX 4090" || c.GetRegion() != "" ||
		c.GetPriceUsdPerHour() != 0.44 || c.GetVramGbPerGpu() != 24 {
		t.Errorf("parsed %+v, want the 4090 at 0.44 with 24 GB and no region", c)
	}
	want := map[string]string{
		"stock_status": "High", "cloud": "SECURE", "secure_cloud": "true",
		"community_cloud": "true",
	}
	for k, v := range want {
		if c.GetAttrs()[k] != v {
			t.Errorf("attr %s = %q, want %q", k, c.GetAttrs()[k], v)
		}
	}
}
