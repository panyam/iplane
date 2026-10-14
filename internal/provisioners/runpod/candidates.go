package runpod

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
	skhttp "github.com/panyam/servicekit/http"
)

// The catalog reads behind Candidates, all on REST v2:
//
//   - GET /catalog/gpus?include=AVAILABILITY&product=POD&count=N&cloud=C
//     answers price and stock per GPU type at a width. It replaces
//     GraphQL's gpuTypes { lowestPrice(gpuCount) }, which until 2026-10
//     was the only catalog RunPod published (REST v1 had none).
//
// The width argument is the whole reason this is worth a network call.
// Availability is not a property of a card, it is a property of a card AT
// a width: probing live on 2026-08-15, 35 of 48 types were obtainable as a
// single GPU and only 11 of 48 as eight. A catalog cannot express that and
// neither can --dry-run.
//
// Two things v2's catalog does not carry, and what the adapter does about
// each:
//
//   - A whole-shape price. GraphQL's lowestPrice priced the shape at the
//     requested gpuCount; v2 publishes a list price for ONE card per cloud
//     and says the rate for a unit is that figure times the count. So the
//     candidate price is price x count. The billed rate is still whatever
//     the pod reports as `cost` once rented.
//   - Any interruptible tier. GraphQL had minimumBidPrice (which equalled
//     the on-demand price on every shape measured, see NOTES.md); v2 has
//     no bid price in the catalog and no bid or interruptible field on
//     the create. A reclaimable search therefore gets no RunPod
//     candidates, which is what it effectively got before.

// gpuCatalogResponse is GET /catalog/gpus with include=AVAILABILITY.
type gpuCatalogResponse struct {
	GPUs []gpuCatalogEntry `json:"gpus"`
}

// gpuCatalogEntry is the subset of v2's GpuType this adapter reads.
type gpuCatalogEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Memory    int    `json:"memory"` // VRAM in GB
	Secure    bool   `json:"secure"`
	Community bool   `json:"community"`
	// Price is the list price for a SINGLE card of this type, per cloud.
	Price struct {
		Secure    float64 `json:"secure"`
		Community float64 `json:"community"`
	} `json:"price"`
	// Availability is NONE / LOW / MEDIUM / HIGH for the requested cloud,
	// product and count. Absent or NONE means the type cannot be had at
	// this width, and it must stay distinguishable from a present-but-low
	// reading.
	Availability string `json:"availability"`
}

// catalogDatacenter is DataCenterAvailability, as the CPU catalog reports
// it per datacenter.
type catalogDatacenter struct {
	ID           string `json:"id"`
	Availability string `json:"availability"`
}

// inStock reports whether a catalog availability level means "can be had".
// NONE and an absent field both mean no.
func inStock(level string) bool {
	switch strings.ToUpper(level) {
	case "LOW", "MEDIUM", "HIGH":
		return true
	}
	return false
}

// stockLabel renders a v2 availability level the way the stock_status attr
// has always read ("Low", "Medium", "High"), so `iplane capacity` output
// and the recorded capacity samples stay comparable across the migration.
func stockLabel(level string) string {
	l := strings.ToLower(level)
	if l == "" {
		return ""
	}
	return strings.ToUpper(l[:1]) + l[1:]
}

// fetchGPUCatalog reads the GPU catalog with availability at gpuCount on
// one cloud.
func (p *Provider) fetchGPUCatalog(ctx context.Context, gpuCount int, cloud string) ([]gpuCatalogEntry, error) {
	q := url.Values{}
	q.Set("include", "AVAILABILITY")
	q.Set("product", "POD")
	q.Set("count", strconv.Itoa(gpuCount))
	q.Set("cloud", cloud)
	req, err := p.client.newReq("GET", "/catalog/gpus", q, nil)
	if err != nil {
		return nil, wrapErr("candidates", err)
	}
	resp, err := skhttp.Call[gpuCatalogResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("candidates", err)
	}
	return resp.GPUs, nil
}

// Candidates satisfies provisioners.CandidateSource.
//
// RunPod is not a marketplace, so like Lambda there is no host to identify
// and no offer to name. A RunPod candidate is a GPU type, at the live
// per-cloud price, with RunPod's stock reading. The price and
// the stock are facts the static catalog cannot hold, and the stock level
// is the closest thing any of our providers gives to a direct answer to
// "can I actually have this right now".
//
// Clouds are read in the order a create tries them (SECURE, then
// COMMUNITY), and a type is reported on the first cloud that has it in
// stock, at that cloud's price. That keeps a listing from quoting the
// community price for a shape the create would place on secure.
func (p *Provider) Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]*provisioners.Candidate, error) {
	if reqs == nil {
		reqs = &provisionerv1.ResourceRequirements{}
	}
	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}

	// No catalog read can answer a reclaimable request (see the header
	// comment), so do not make one.
	if reqs.GetReclaimPolicy() == provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED {
		return nil, nil
	}

	// The same resolver the rent path uses decides which types are eligible,
	// so a listing cannot offer something a create would then refuse. RunPod's
	// live catalog carries 48 types against our 16, and the extra 32 are ones
	// we have never validated a deploy against.
	var eligible map[string]bool
	if s := reqs.GetSku(); s != "" {
		eligible = map[string]bool{s: true}
	} else {
		eligible = map[string]bool{}
		for _, id := range MatchSKUs(reqs) {
			eligible[id] = true
		}
	}

	byCloud := map[string][]gpuCatalogEntry{}
	for _, cloud := range anyCloud {
		gpus, err := p.fetchGPUCatalog(ctx, gpuCount, cloud)
		if err != nil {
			return nil, err
		}
		byCloud[cloud] = gpus
	}

	var out []*provisioners.Candidate
	seen := map[string]bool{}
	for _, cloud := range anyCloud {
		for _, g := range byCloud[cloud] {
			if !eligible[g.ID] || seen[g.ID] {
				continue
			}
			offered, perCard := g.Secure, g.Price.Secure
			if cloud == cloudCommunity {
				offered, perCard = g.Community, g.Price.Community
			}
			// A type with no stock at this width, or no price on this cloud,
			// is not something an operator can act on, which is the same
			// call Lambda's empty-capacity case makes. It may still turn up
			// on the next cloud.
			if !offered || perCard <= 0 || !inStock(g.Availability) {
				continue
			}
			seen[g.ID] = true
			out = append(out, candidateRow(g, cloud, perCard*float64(gpuCount), gpuCount))
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GetPriceUsdPerHour() < out[j].GetPriceUsdPerHour()
	})
	return out, nil
}

// candidateRow renders one in-stock GPU type as a candidate.
func candidateRow(g gpuCatalogEntry, cloud string, price float64, gpuCount int) *provisioners.Candidate {
	// vramGb stays the live API figure, which is what an operator
	// filters on. exactBytes comes off the catalog row instead,
	// because the two want opposite error directions (#323).
	var (
		family     fabric.Family
		exactBytes int64
	)
	if spec := LookupSKU(g.ID); spec != nil {
		family = spec.Family
		exactBytes = skucatalog.ExactVRAMBytes(spec.VRAMGb)
	}
	res := fabric.Resolve(fabric.Observation{Family: family, GpuCount: int32(gpuCount)})

	return &provisioners.Candidate{
		// No HostID, no OfferID, and no Region. RunPod rents a GPU type
		// and picks the datacenter itself, so there is nothing here to
		// name that would still mean anything an hour later.
		Sku:              g.ID,
		PriceUsdPerHour:  price,
		GpuCount:         int32(gpuCount),
		VramGbPerGpu:     int32(g.Memory),
		VramBytesPerGpu:  exactBytes,
		FabricScope:      res.Scope,
		FabricSource:     res.Source,
		FabricGbps:       res.Gbps,
		FabricTechnology: res.Technology,
		Attrs: map[string]string{
			"stock_status":    stockLabel(g.Availability),
			"cloud":           cloud,
			"secure_cloud":    strconv.FormatBool(g.Secure),
			"community_cloud": strconv.FormatBool(g.Community),
		},
	}
}
