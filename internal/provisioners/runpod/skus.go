package runpod

import (
	"sort"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// SKUSpec describes what a RunPod gpuTypeId actually delivers: VRAM,
// the system RAM that comes with one GPU on RunPod's default pod
// shape, and a rough price tier (USD/hr at the cheapest cloud type).
//
// Verify against the live catalog (read-only) with:
//
//	curl -s https://api.runpod.io/v2/catalog/gpus -H "Authorization: Bearer $RUNPOD_API_KEY"
//
// which returns {"gpus": [{id, name, memory, price: {secure, community}, ...}]};
// memory is VRAM in GB and price is per card. `make runpod-v2-check` runs
// that and the other read-only checks.
//
// These are pre-Spawn estimates; the actual price (and the actual VRAM
// allocation) is reported by the provider in the Spawn response and
// recorded in the iplane state file.
type SKUSpec struct {
	GpuTypeID          string  // RunPod's literal gpuTypeId string
	VRAMGb             int     // GPU memory per card
	DefaultSystemRAMGb int     // system RAM for a typical single-GPU pod with this SKU
	DefaultDiskGb      int     // typical container-disk default for this tier
	PriceUSDPerHour    float64 // cheapest cloud tier price; for ordering, not authoritative

	// Family maps this SKU onto the cross-provider fabric catalog.
	//
	// RunPod exposes no interconnect field anywhere in its API, so the form
	// factor baked into the gpuTypeId string is the only signal available:
	// "NVIDIA A100-SXM4-80GB" is on an NVSwitch mesh, "NVIDIA A100 80GB PCIe"
	// is on a PCIe root that may or may not carry a bridge. Every datacenter
	// part names its form factor, which is what makes this tractable.
	// Verified against the live gpuTypes catalog 2026-08-09.
	Family fabric.Family
}

// skus is the catalog the resolver iterates. The resolver sorts the
// matching subset by price, and createPod tries the result in that order,
// one POST /pods per SKU, until RunPod places one.
//
// Each GpuTypeID must be an id GET /v2/catalog/gpus lists (the `id`
// field, which is what gpu.id on a create takes). Confirm current values
// against:
//
//	curl -s https://api.runpod.io/v2/catalog/gpus -H "Authorization: Bearer $RUNPOD_API_KEY"
//
// RunPod periodically retires SKUs (e.g., the A100 40GB PCIe was
// dropped sometime before 2026-05) and adds new ones (B200, H200
// variants, Blackwell PRO 6000 line). A retired entry here costs one
// refused create per Spawn and is skipped; the catalog listing is how to
// spot it.
var skus = []SKUSpec{
	// Small (>=24 GB VRAM): consumer + entry datacenter.
	{GpuTypeID: "NVIDIA GeForce RTX 4090", VRAMGb: 24, DefaultSystemRAMGb: 16, DefaultDiskGb: 20, PriceUSDPerHour: 0.39, Family: fabric.FamilyRTX4090},
	{GpuTypeID: "NVIDIA RTX A5000", VRAMGb: 24, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.36, Family: fabric.FamilyA5000},
	{GpuTypeID: "NVIDIA L4", VRAMGb: 24, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.43, Family: fabric.FamilyL4},
	// "NVIDIA A30" was retired from RunPod's v1 REST enum (confirmed
	// 2026-07-27: a create with it in gpuTypeIds 400s with the full enum,
	// A30 absent). Under v1 any min_vram_gb<=24 request included it in the
	// top-5 and failed whole.
	{GpuTypeID: "NVIDIA GeForce RTX 5090", VRAMGb: 32, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.69, Family: fabric.FamilyRTX5090},

	// Medium (>=40 GB VRAM): workstation / mid-datacenter.
	{GpuTypeID: "NVIDIA A40", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.39, Family: fabric.FamilyA40},
	{GpuTypeID: "NVIDIA L40", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.69, Family: fabric.FamilyL40},
	{GpuTypeID: "NVIDIA L40S", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79, Family: fabric.FamilyL40S},
	{GpuTypeID: "NVIDIA RTX A6000", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79, Family: fabric.FamilyA6000},
	{GpuTypeID: "NVIDIA RTX 6000 Ada Generation", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.99, Family: fabric.FamilyRTX6000Ada},

	// Large (>=80 GB VRAM): 70B-class inference territory.
	{GpuTypeID: "NVIDIA A100 80GB PCIe", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.69, Family: fabric.FamilyA100PCIe},
	{GpuTypeID: "NVIDIA A100-SXM4-80GB", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.79, Family: fabric.FamilyA100SXM},
	{GpuTypeID: "NVIDIA H100 PCIe", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.39, Family: fabric.FamilyH100PCIe},
	{GpuTypeID: "NVIDIA H100 80GB HBM3", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.49, Family: fabric.FamilyH100SXM},

	// XL (>=94 GB VRAM): frontier / 400B-class multi-host.
	{GpuTypeID: "NVIDIA H100 NVL", VRAMGb: 94, DefaultSystemRAMGb: 128, DefaultDiskGb: 100, PriceUSDPerHour: 2.99, Family: fabric.FamilyH100NVL},
	{GpuTypeID: "NVIDIA H200", VRAMGb: 141, DefaultSystemRAMGb: 192, DefaultDiskGb: 100, PriceUSDPerHour: 3.99, Family: fabric.FamilyH200SXM},
	{GpuTypeID: "NVIDIA B200", VRAMGb: 192, DefaultSystemRAMGb: 256, DefaultDiskGb: 100, PriceUSDPerHour: 5.99, Family: fabric.FamilyB200},
}

// Class-to-constraint-defaults lives in the service layer
// (internal/provisioners/service.go classDefaults) -- one mapping
// shared across providers means "class=small" resolves to the same
// numeric requirements on RunPod, Lambda, AWS, anywhere. The runpod
// adapter only sees expanded constraints.

// MaxSKUsPerRequest caps how many GPU types one Spawn or Deploy tries.
// Three reasons to cap:
//
//   - Class shorthand has no upper bound today (class=small expands to
//     min_vram_gb=24 with no max). Without a cap, every SKU with
//     VRAM>=24 enters the candidate list -- including B200 at 192 GB.
//     An operator who asked for "small" should not silently land on a
//     frontier GPU because the cheap tier is exhausted; the price would
//     be 10x higher than expected.
//
//   - v2's create places one GPU type and never falls back, so the adapter
//     walks the list itself, one create per entry (twice per entry for a
//     deploy that may also try COMMUNITY). The tail of the list is real
//     fallback, and each step of it is a round trip.
//
//   - Some RunPod accounts are restricted from provisioning specific
//     SKUs (B200, H200, H100 NVL typically require approval). v2 answers
//     a restricted SKU with 403 and the walk skips it; under v1 one
//     restricted entry in gpuTypeIds could fail the whole request.
//
// Capping at top-5 cheapest preserves real fallback (RunPod will try
// each in order if the cheapest is unavailable) without exposing the
// caller to large price-tier jumps. Operators who want a strict ceiling
// pass --gpu-sku for an explicit single-SKU request; a future
// max_vram_gb constraint would let class shorthand carry a real upper
// bound (see ROADMAP for the eventual fix).
const MaxSKUsPerRequest = 5

// MatchSKUs is the per-provider resolver in the (a) constraints /
// (b) resolver / (c) executor model. Given a ResourceRequirements,
// it returns the ordered list of gpuTypeIds that satisfy every
// numeric constraint, cheapest first, capped at MaxSKUsPerRequest.
//
// The returned slice is the order Spawn and Deploy try GPU types in, one
// POST /pods per entry.
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; the caller should surface this as "no matching SKU"
// rather than silently passing an empty list to RunPod.
func MatchSKUs(reqs *provisionerv1.ResourceRequirements) []string {
	if reqs == nil {
		return nil
	}
	var matches []SKUSpec
	for _, sku := range skus {
		if sku.VRAMGb < int(reqs.GetMinVramGb()) {
			continue
		}
		// NOTE: min_disk_gb does NOT filter SKUs. DefaultDiskGb is a typical
		// default, not a per-SKU ceiling -- RunPod provisions container disk
		// as an independent create param (the deployer sizes it from
		// min_disk_gb). Filtering here would wrongly exclude every SKU when
		// a big model asks for more disk than any tier's default (e.g. 150
		// GB for a 72B: max DefaultDiskGb is 100).
		if int(reqs.GetMinRamGb()) > 0 && sku.DefaultSystemRAMGb < int(reqs.GetMinRamGb()) {
			continue
		}
		// Fabric filter. RunPod supplies no measurement, so every verdict here
		// is the declared tier: SXM/NVL parts pass, cards with no NVLink at
		// all are rejected, and bridge-capable PCIe parts resolve to UNKNOWN
		// and fail closed. Dropping the unknowns is the point -- a silent pass
		// rents a pool and bills for it before reporting that the fabric was
		// never there.
		if !fabric.Satisfies(
			fabric.Resolve(fabric.Observation{Family: sku.Family}),
			reqs.GetFabricScope(), reqs.GetMinFabricGbps(),
		) {
			continue
		}
		matches = append(matches, sku)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].PriceUSDPerHour < matches[j].PriceUSDPerHour
	})
	if len(matches) > MaxSKUsPerRequest {
		matches = matches[:MaxSKUsPerRequest]
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.GpuTypeID
	}
	return out
}

// LookupSKU returns the catalog entry for a known gpuTypeId, or nil
// if the SKU is operator-supplied and not in our table. The runpod
// Provider uses this on the Describe path to surface a class label
// when the SKU is known.
func LookupSKU(gpuTypeID string) *SKUSpec {
	for i := range skus {
		if skus[i].GpuTypeID == gpuTypeID {
			return &skus[i]
		}
	}
	return nil
}

// classifySKU returns the class a SKU belongs to, derived from its
// VRAM (not a hardcoded reverse table). An RTX 4090 at 24 GB is
// "small" because it falls in the [24, 40) VRAM band, full stop.
// Unknown SKUs return "" -- the operator-supplied --gpu-sku case
// where we have no opinion about classification.
func classifySKU(gpuTypeID string) string {
	sku := LookupSKU(gpuTypeID)
	if sku == nil {
		return ""
	}
	switch {
	case sku.VRAMGb >= 96:
		return provisioners.GPUClassXLarge
	case sku.VRAMGb >= 80:
		return provisioners.GPUClassLarge
	case sku.VRAMGb >= 40:
		return provisioners.GPUClassMedium
	default:
		return provisioners.GPUClassSmall
	}
}

// knownClasses lists class shorthand keys for error messages.
func knownClasses() []string {
	return []string{
		provisioners.GPUClassSmall,
		provisioners.GPUClassMedium,
		provisioners.GPUClassLarge,
		provisioners.GPUClassXLarge,
	}
}

// isActiveProviderState reports whether a RunPod pod status counts as
// "the instance is up and idempotency-adoptable." See the
// ActiveStateChecker discussion in internal/provisioners/service.go
// for why this lives in the adapter package rather than centrally.
//
// v2's statuses are PROVISIONING, STARTING, RUNNING, EXITED, ERROR and
// TERMINATED. The first three are a rental in progress or in service, the
// counterpart of v1's CREATED / RUNNING / RESTARTING desiredStatus. EXITED
// is a stopped pod that bills nothing for compute, and ERROR is RunPod's
// "unrecoverable"; neither is something to adopt.
func isActiveProviderState(state string) bool {
	switch strings.ToUpper(state) {
	case "PROVISIONING", "STARTING", "RUNNING":
		return true
	}
	return false
}

// stampFabric fills the fabric fields on a Hardware record for a resolved
// RunPod SKU. RunPod reports no measurement, so this is always the declared
// tier, and an operator-supplied --gpu-sku outside our catalog resolves to
// UNKNOWN rather than to a guess.
func stampFabric(hw *provisionerv1.Hardware, gpuTypeID string) {
	var family fabric.Family
	if spec := LookupSKU(gpuTypeID); spec != nil {
		family = spec.Family
	}
	res := fabric.Resolve(fabric.Observation{Family: family})
	hw.FabricScope = res.Scope
	hw.FabricSource = res.Source
	hw.FabricGbps = res.Gbps
	hw.FabricTechnology = res.Technology
}
