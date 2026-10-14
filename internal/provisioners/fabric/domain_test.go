package fabric

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// A fabric that reaches only some of the cards is not an intra-node fabric
// for the group. Ch 10's Fig 10.9 is the case: a four-card host built from
// two bridged pairs is two islands of two, and a tensor-parallel all-reduce
// across it crosses PCIe on every step. Asking for intra-node on four cards
// has to mean all four share it.
func TestIntraNodeMustSpanEveryCardRequested(t *testing.T) {
	tests := []struct {
		name string
		obs  Observation
		want bool
	}{
		{"8x NVSwitch board is one domain", Observation{Family: FamilyH100SXM, GpuCount: 8}, true},
		{"8x B200 board is one domain", Observation{Family: FamilyB200, GpuCount: 8}, true},
		{"8x MI300X xGMI mesh is one domain", Observation{Family: FamilyMI300X, GpuCount: 8}, true},
		{"one NVL bridged pair asked for two", Observation{Family: FamilyH100NVL, GpuCount: 2}, true},
		{"measured bridged PCIe pair asked for two",
			Observation{Family: FamilyA100PCIe, GpuCount: 2, HasMeasurement: true, MeasuredGbps: 2400}, true},

		{"4x NVL is two islands of two", Observation{Family: FamilyH100NVL, GpuCount: 4}, false},
		{"4x H200 NVL is not vouched for past a pair", Observation{Family: FamilyH200NVL, GpuCount: 4}, false},
		{"measured bridged PCIe box of four is two pairs",
			Observation{Family: FamilyA100PCIe, GpuCount: 4, HasMeasurement: true, MeasuredGbps: 2400}, false},
		{"measured bridged A6000 box of eight is four pairs",
			Observation{Family: FamilyA6000, GpuCount: 8, HasMeasurement: true, MeasuredGbps: 448}, false},
		{"no board is wider than eight", Observation{Family: FamilyH100SXM, GpuCount: 16}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Satisfies(Resolve(tt.obs), scopeIntra, 0)
			if got != tt.want {
				t.Errorf("Satisfies(Resolve(%+v), INTRA_NODE) = %v, want %v", tt.obs, got, tt.want)
			}
		})
	}
}

// A split group is not a fabric-free group either: the pairs are linked, so
// an A/B control arm asking for NONE must not land on one.
func TestSplitGroupDoesNotSatisfyNone(t *testing.T) {
	if Satisfies(Resolve(Observation{Family: FamilyH100NVL, GpuCount: 4}), scopeNone, 0) {
		t.Error("4x H100 NVL satisfied fabric_scope=NONE; its pairs carry NVLink")
	}
}

// An absent count is the old behaviour, a statement about the card, so every
// caller that has not been told a width keeps working.
func TestAbsentCountKeepsTheCardVerdict(t *testing.T) {
	if !Satisfies(Resolve(Observation{Family: FamilyH100NVL}), scopeIntra, 0) {
		t.Error("H100 NVL with no count stated no longer resolves intra-node")
	}
}

func TestSplitReasonNamesTheDomain(t *testing.T) {
	reason := SplitReason(FamilyH100NVL, 4)
	if reason == "" {
		t.Fatal("4x H100 NVL produced no split reason")
	}
	for _, want := range []string{"h100-nvl", "2", "4"} {
		if !strings.Contains(reason, want) {
			t.Errorf("split reason %q does not mention %q", reason, want)
		}
	}
	if r := SplitReason(FamilyH100SXM, 8); r != "" {
		t.Errorf("8x H100 SXM reported a split: %q", r)
	}
	if r := SplitReason(FamilyH100NVL, 2); r != "" {
		t.Errorf("2x H100 NVL reported a split: %q", r)
	}
	if r := SplitReason(FamilyL40S, 4); r != "" {
		t.Errorf("a card with no fabric reported a split rather than none: %q", r)
	}
}

func TestCouldSatisfyRefusesAGroupNoReadingCanRescue(t *testing.T) {
	if CouldSatisfy(FamilyA100PCIe, scopeIntra, 4) {
		t.Error("4x A100 PCIe searched for intra-node; bridges join pairs, so no reading can rescue it")
	}
	if !CouldSatisfy(FamilyA100PCIe, scopeIntra, 2) {
		t.Error("2x A100 PCIe excluded from an intra-node search; a reading could rescue it")
	}
}

// Every family that can carry a fabric says how many cards one domain joins.
func TestEveryFabricFamilyStatesADomainSize(t *testing.T) {
	for family, spec := range catalog {
		if spec.Capability == CapabilityNone {
			if spec.DomainSize != 0 {
				t.Errorf("%q has no fabric but declares a domain of %d", family, spec.DomainSize)
			}
			continue
		}
		if spec.DomainSize < 2 {
			t.Errorf("%q can carry a fabric but declares a domain of %d", family, spec.DomainSize)
		}
	}
}

// SplitHint is the suffix the adapters append to a no-match error. It names
// every family refused for width, once, and says nothing for a request it
// did not narrow.
func TestSplitHintNamesEveryFamilyRefusedForWidth(t *testing.T) {
	families := []Family{FamilyH100NVL, FamilyH100SXM, FamilyA100PCIe, FamilyH100NVL}
	intra := provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE

	hint := SplitHint(families, intra, 4)
	if !strings.HasPrefix(hint, "; refused for fabric: ") {
		t.Fatalf("hint %q should read as an error suffix", hint)
	}
	if !strings.Contains(hint, string(FamilyH100NVL)) || !strings.Contains(hint, string(FamilyA100PCIe)) {
		t.Errorf("hint %q should name both pair-only families", hint)
	}
	if strings.Count(hint, string(FamilyH100NVL)) != 1 {
		t.Errorf("hint %q repeats a family", hint)
	}
	if strings.Contains(hint, string(FamilyH100SXM)) {
		t.Errorf("hint %q names a family whose fabric spans eight cards", hint)
	}
	if h := SplitHint(families, intra, 2); h != "" {
		t.Errorf("2 cards: hint %q, want none", h)
	}
	if h := SplitHint(families, provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED, 4); h != "" {
		t.Errorf("no fabric asked for: hint %q, want none", h)
	}
}
