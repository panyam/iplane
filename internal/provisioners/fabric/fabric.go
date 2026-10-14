// Package fabric holds the cross-provider knowledge of what GPU
// interconnect a card is capable of, and the rules for deciding whether a
// candidate satisfies an operator's fabric requirement.
//
// It lives outside the adapters because "does an RTX A6000 support NVLink"
// is a fact about NVIDIA's product line, not about RunPod or Vast. Each
// adapter maps its own SKU vocabulary onto Family, and the answers come back
// vendor-neutral (provisionerv1.FabricScope), so provider-specific naming
// stays in the adapter exactly as the rest of the provisioner seams do.
//
// The design rationale, and the live provider probes the tier rules are
// derived from, are in docs/design/0006-ch10-provider-reality-and-control-channel.md.
package fabric

import (
	"fmt"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Capability is what a GPU model is physically able to do, before any
// particular host is considered. It is the catalog axis: three states, not
// two, because "this card can be bridged but the provider does not say
// whether this one is" is a genuinely different answer from either yes or no.
type Capability int

const (
	// CapabilityNone means the card has no intra-node fabric at all. An
	// RTX 4090 or an L40S: cards that talk over PCIe and nothing else.
	// A zero bandwidth reading on one of these is real information.
	CapabilityNone Capability = iota

	// CapabilityAlways means the fabric is part of the board. SXM and NVL
	// parts are soldered onto an NVSwitch mesh or a bridged pair; there is
	// no configuration in which an SXM4 A100 lacks NVLink.
	CapabilityAlways

	// CapabilityOptional means the card supports a bridge that the host
	// operator may or may not have installed. A100 PCIe, H100 PCIe, RTX
	// A5000/A6000. This is the state that forces FABRIC_SOURCE_UNKNOWN:
	// the SKU name cannot answer the question and most providers do not.
	CapabilityOptional
)

// Family is a vendor-neutral GPU model identifier. Adapters normalize their
// own SKU strings onto these (RunPod's "NVIDIA A100-SXM4-80GB" and Vast's
// "A100 SXM4" are both FamilyA100SXM), so the catalog is written once.
type Family string

// Known GPU families. The list covers what our providers actually offer;
// an unrecognized family resolves to UNKNOWN rather than to a guess.
const (
	FamilyA100SXM    Family = "a100-sxm"
	FamilyA100PCIe   Family = "a100-pcie"
	FamilyH100SXM    Family = "h100-sxm"
	FamilyH100PCIe   Family = "h100-pcie"
	FamilyH100NVL    Family = "h100-nvl"
	FamilyH200SXM    Family = "h200-sxm"
	FamilyH200NVL    Family = "h200-nvl"
	FamilyB200       Family = "b200"
	FamilyB300       Family = "b300"
	FamilyMI300X     Family = "mi300x"
	FamilyA10        Family = "a10"
	FamilyGH200      Family = "gh200"
	FamilyA6000      Family = "rtx-a6000"
	FamilyA5000      Family = "rtx-a5000"
	FamilyL40S       Family = "l40s"
	FamilyL40        Family = "l40"
	FamilyL4         Family = "l4"
	FamilyA40        Family = "a40"
	FamilyRTX6000Ada Family = "rtx-6000-ada"
	FamilyRTX3090    Family = "rtx-3090"
	FamilyRTX4090    Family = "rtx-4090"
	FamilyRTX5090    Family = "rtx-5090"
	FamilyV100SXM    Family = "v100-sxm"
	FamilyV100PCIe   Family = "v100-pcie"
)

// Spec is the catalog entry for one GPU family.
type Spec struct {
	// Capability is what the silicon can do.
	Capability Capability
	// Technology names the vendor's fabric ("nvlink", "xgmi"). Diagnostic
	// only; it reaches Hardware.fabric_technology and nothing branches on it.
	Technology string
	// PeakGbps is the card's aggregate per-GPU fabric bandwidth in GIGABITS
	// per second when the fabric is present, taken from the vendor's
	// BIDIRECTIONAL per-GPU figure times 8 (an A100 SXM's published 600 GB/s
	// is 4800 here). 0 when the family has no fabric.
	PeakGbps int32
	// DomainSize is the most cards one fabric on this family joins, so a
	// group wider than it is islands rather than one fabric. 8 for an
	// NVSwitch or xGMI board, 2 for a bridge, which joins pairs. 0 when the
	// family has no fabric.
	DomainSize int32
}

// catalog is the declared tier: what each family is capable of, independent
// of any host. Bandwidth figures are NVIDIA/AMD published per-GPU
// bidirectional aggregates converted to gigabits.
//
// This table answers "could this card have a fabric", never "does this
// particular rented machine have one". Only a provider reading answers that,
// which is why Resolve takes both.
//
// Treat cross-source bandwidth comparisons as coarse. Vast's own readings are
// not consistent about direction: probing live on 2026-08-09 it reported 300
// for A100 SXM4 (unidirectional, against a 600 GB/s published figure) but 900
// for H100 SXM (bidirectional). So min_fabric_gbps is dependable for
// separating fabric tiers and not for fine thresholds near a card's rating.
var catalog = map[Family]Spec{
	// Board-integrated fabric. No configuration lacks it, and one NVSwitch
	// (or, on an A100 HGX, a direct mesh) joins all eight cards. The V100
	// SXM board is a hybrid cube-mesh rather than all-to-all, but its eight
	// cards share one NVLink domain that NCCL rings run across.
	FamilyA100SXM: {CapabilityAlways, "nvlink", 4800, 8},
	FamilyH100SXM: {CapabilityAlways, "nvswitch", 7200, 8},
	FamilyH200SXM: {CapabilityAlways, "nvswitch", 7200, 8},
	FamilyB200:    {CapabilityAlways, "nvswitch", 14400, 8},
	FamilyB300:    {CapabilityAlways, "nvswitch", 14400, 8},
	FamilyV100SXM: {CapabilityAlways, "nvlink", 2400, 8},
	// MI300X's fabric is xGMI, not NVLink. It is INTRA_NODE all the same,
	// and it is the case a vendor-named enum could never have expressed.
	FamilyMI300X: {CapabilityAlways, "xgmi", 8192, 8},

	// GH200 superchips carry NVLink between GPUs, two per node at most (NVL2). Note the catalog describes
	// the CARD, not the rented shape: Lambda only sells gpu_1x_gh200, where
	// there is no second card to link to. Requiring a fabric on a one-GPU
	// instance is rejected up front (see the service-layer validation) rather
	// than resolved here.
	FamilyGH200: {CapabilityAlways, "nvlink", 7200, 2},

	// NVL parts are bridged pairs from the factory, so four of them are two
	// islands of two (Ch 10, Fig 10.9). The H200 NVL also ships with a 4-way
	// bridge, but nothing a provider publishes says which one a host has, so
	// past a pair it is not vouched for and fails closed.
	FamilyH100NVL: {CapabilityAlways, "nvlink", 4800, 2},
	FamilyH200NVL: {CapabilityAlways, "nvlink", 4800, 2},

	// Bridge-capable: the card supports NVLink, the host may not have
	// installed it, and the provider does not say. Probing Vast live,
	// 3 of 24 "A100 PCIE" offers reported 275-300 GB/s, so treating these
	// as NONE would wrongly exclude real working machines. Their bridges
	// join two cards, so a reading can vouch for a pair and never for more.
	FamilyA100PCIe: {CapabilityOptional, "nvlink", 4800, 2},
	FamilyH100PCIe: {CapabilityOptional, "nvlink", 4800, 2},
	FamilyA6000:    {CapabilityOptional, "nvlink", 896, 2},
	// The 3090 is the one consumer card with an NVLink connector; the 4090
	// and 5090 dropped it. Bridge-capable, so a Vast reading can rescue it.
	FamilyRTX3090: {CapabilityOptional, "nvlink", 896, 2},
	FamilyA5000:   {CapabilityOptional, "nvlink", 896, 2},

	// No intra-node fabric at all. A zero reading on these is a fact.
	FamilyA10:        {CapabilityNone, "", 0, 0},
	FamilyL40S:       {CapabilityNone, "", 0, 0},
	FamilyL40:        {CapabilityNone, "", 0, 0},
	FamilyL4:         {CapabilityNone, "", 0, 0},
	FamilyA40:        {CapabilityNone, "", 0, 0},
	FamilyRTX6000Ada: {CapabilityNone, "", 0, 0},
	FamilyRTX4090:    {CapabilityNone, "", 0, 0},
	FamilyRTX5090:    {CapabilityNone, "", 0, 0},
	FamilyV100PCIe:   {CapabilityNone, "", 0, 0},
}

// Observation is what an adapter learned about one candidate: which family
// it is, and what the provider reported about its fabric, if anything.
type Observation struct {
	// Family is the normalized GPU model. Empty or unrecognized yields an
	// UNKNOWN result rather than a default.
	Family Family

	// MeasuredGbps is a provider-reported fabric bandwidth in gigabits per
	// second. Only Vast supplies one today (bw_nvlink, converted from GB/s).
	//
	// HasMeasurement, not a zero check, is what marks this as present. Vast
	// reports 0.0 both for "this machine has no NVLink" and for "we never
	// measured it", so the zero has to be interpreted against the catalog
	// rather than trusted, which is what Resolve does.
	MeasuredGbps   int32
	HasMeasurement bool

	// GpuCount is how many cards the group spans. An intra-node fabric has to
	// join all of them, so a count wider than the family's DomainSize splits
	// the verdict (see Resolve). 0 means not stated, and the verdict is about
	// the card alone, which is what every caller got before counts were read.
	GpuCount int32
}

// Result is the resolved fabric of a candidate, ready to be stamped onto
// Hardware or compared against a requirement.
type Result struct {
	Scope      provisionerv1.FabricScope
	Source     provisionerv1.FabricSource
	Gbps       int32
	Technology string

	// Split is set, with the reason, when the cards carry a fabric that does
	// not join all of them: two bridged pairs on a four-card host. Scope is
	// then UNSPECIFIED, so the group satisfies neither INTRA_NODE (no one
	// fabric spans it) nor NONE (its pairs are linked).
	Split string
}

// Resolve turns a catalog entry plus an optional provider reading into a
// fabric verdict, applying the three rules docs/design/0006 Part 2 derives
// from live provider data:
//
//  1. An unknown fabric stays UNKNOWN and (via Satisfies) fails closed.
//  2. A zero reading means "no fabric" only when the catalog agrees the card
//     has none. On a card that should have had one it means "not measured",
//     because Vast under-reported roughly a quarter of SXM machines and the
//     payload cannot distinguish the two.
//  3. A real measurement outranks the catalog, since a bridged card named
//     "PCIe" is a machine a name-only catalog would wrongly exclude.
//
// A fourth rule sits over all three: a fabric only counts for the group if it
// joins every card in it. When obs.GpuCount exceeds the family's DomainSize,
// a verdict that would have been INTRA_NODE comes back split instead.
//
// Resolve never reports INTER_NODE: no provider we have exposes a cross-node
// fabric on a single rented instance. That scope arrives with the group
// provisioning capability (issue 212).
func Resolve(obs Observation) Result {
	res := resolveCard(obs)
	if res.Scope != provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE {
		return res
	}
	if reason := SplitReason(obs.Family, obs.GpuCount); reason != "" {
		return Result{
			Scope:      provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source:     res.Source,
			Technology: res.Technology,
			Split:      reason,
		}
	}
	return res
}

// SplitReason says why a group of gpuCount cards of this family is not one
// fabric, or returns "" when it is, when the family has no fabric at all, or
// when the count is not stated.
func SplitReason(family Family, gpuCount int32) string {
	spec, known := catalog[family]
	if !known || spec.DomainSize <= 0 || gpuCount <= spec.DomainSize {
		return ""
	}
	return fmt.Sprintf("%s fabric joins at most %d cards, so %d cards would be islands rather than one intra-node fabric",
		family, spec.DomainSize, gpuCount)
}

// resolveCard is the verdict about the card, before the group's width is
// considered.
func resolveCard(obs Observation) Result {
	spec, known := catalog[obs.Family]
	if !known {
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		}
	}

	if obs.HasMeasurement {
		if obs.MeasuredGbps > 0 {
			// Rule 3: a positive reading is the strongest evidence there is,
			// including on a card the catalog only calls bridge-capable.
			return Result{
				Scope:      provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
				Source:     provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
				Gbps:       obs.MeasuredGbps,
				Technology: spec.Technology,
			}
		}
		// Rule 2: zero is only trustworthy when the catalog agrees there was
		// never a link to find.
		if spec.Capability == CapabilityNone {
			return Result{
				Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
				Source: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
			}
		}
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		}
	}

	switch spec.Capability {
	case CapabilityAlways:
		return Result{
			Scope:      provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			Source:     provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED,
			Gbps:       spec.PeakGbps,
			Technology: spec.Technology,
		}
	case CapabilityNone:
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED,
		}
	default: // CapabilityOptional, and no provider reading to settle it.
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		}
	}
}

// Satisfies reports whether a resolved candidate meets a requirement.
//
// Rule 1 lives here: an UNKNOWN source never satisfies a set requirement, so
// a candidate we cannot vouch for is dropped rather than rented and measured
// afterwards. The operator's override is ResourceRequirements.sku, which
// skips resolution entirely; there is no separate "allow unknown" flag,
// because one would make the guarantee optional and therefore worthless.
//
// A zero wantScope means the operator expressed no fabric preference and
// everything satisfies it, including UNKNOWN. That is the Ch 6-9 default and
// it must stay free.
func Satisfies(got Result, wantScope provisionerv1.FabricScope, minGbps int32) bool {
	if wantScope == provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED {
		return true
	}
	if got.Source == provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN {
		return false
	}
	if got.Scope != wantScope {
		return false
	}
	return minGbps <= 0 || got.Gbps >= minGbps
}

// CouldSatisfy reports whether a family is worth searching at all for a given
// requirement. It is the pre-filter for providers that supply a measurement,
// and it is deliberately more permissive than Satisfies.
//
// The distinction matters and is easy to get backwards. On a provider with no
// readings (RunPod, Lambda) the catalog is the last word, so a bridge-capable
// card resolves to UNKNOWN and is dropped. On a marketplace that measures
// (Vast), dropping it at the catalog stage would discard exactly the machines
// worth having: 3 of 24 "A100 PCIE" offers in the 2026-08-09 probe reported a
// real 275-300 GB/s link. So the catalog filters only what is IMPOSSIBLE, and
// the provider query settles what is merely uncertain.
//
// The group's width is the exception to deferring to a reading: a bridge joins
// a pair however well it measures, so a family whose DomainSize is below
// gpuCount is impossible for INTRA_NODE and is dropped here. 0 means not
// stated.
//
// Callers must still run Satisfies on the resolved result. CouldSatisfy
// narrows a search; it never admits anything.
func CouldSatisfy(family Family, wantScope provisionerv1.FabricScope, gpuCount int32) bool {
	if wantScope == provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED {
		return true
	}
	spec, known := catalog[family]
	if !known {
		return false
	}
	switch wantScope {
	case provisionerv1.FabricScope_FABRIC_SCOPE_NONE:
		return spec.Capability == CapabilityNone
	case provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE:
		// Always AND Optional: the second is the one a reading can rescue.
		return spec.Capability != CapabilityNone && SplitReason(family, gpuCount) == ""
	default:
		// INTER_NODE is not derivable from a single instance's card.
		return false
	}
}

// Stamp resolves an observation and writes the four fabric fields onto a
// Hardware record. Every adapter did this identically, and the failure it
// prevents is a partial stamp: scope set without source is a claim with no
// provenance, and Satisfies would then read a DECLARED-looking verdict off a
// zero value. Resolving and writing in one call keeps the four fields
// consistent with each other by construction.
func Stamp(hw *provisionerv1.Hardware, obs Observation) {
	if hw == nil {
		return
	}
	if obs.GpuCount == 0 {
		obs.GpuCount = hw.GetGpuCount()
	}
	res := Resolve(obs)
	hw.FabricScope = res.Scope
	hw.FabricSource = res.Source
	hw.FabricGbps = res.Gbps
	hw.FabricTechnology = res.Technology
}

// GbpsFromGBps converts a gigaBYTES-per-second reading, which is how vendors
// and Vast quote NVLink, into the gigaBITS this package and the proto use.
// Exists as a named function rather than an inline *8 because the two units
// differ by exactly the factor most likely to pass code review unnoticed.
func GbpsFromGBps(gbytes float64) int32 {
	return int32(gbytes * 8)
}
