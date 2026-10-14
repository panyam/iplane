package provisioners_test

import (
	"context"
	"errors"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

func TestDiskFloorGB(t *testing.T) {
	cases := []struct {
		bytes int64
		want  int32
	}{
		{0, 0},
		{-1, 0},
		{145_000_000_000, 200}, // Qwen2.5-72B-Instruct at BF16
		{474_000_000_000, 570}, // cyankiwi/GLM-5.2-AWQ-INT4
		{16_400_000_000, 60},   // an 8B
	}
	for _, c := range cases {
		if got := provisioners.DiskFloorGB(c.bytes); got != c.want {
			t.Errorf("DiskFloorGB(%d) = %d, want %d", c.bytes, got, c.want)
		}
	}
}

// checkpointStore reports a download size, or an error when err is set.
type checkpointStore struct {
	bytes int64
	err   error
}

func (c *checkpointStore) Resolve(_ context.Context, spec string) (modelstores.Resolved, error) {
	return modelstores.Resolved{EngineModelArg: spec}, nil
}

func (c *checkpointStore) Checkpoint(context.Context, *provisionerv1.DescribeModelRequest) (*provisionerv1.ModelCheckpoint, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &provisionerv1.ModelCheckpoint{DownloadBytes: c.bytes}, nil
}

// diskDeploy creates a deployment and returns the min_disk_gb each replica
// was recorded with, which is what its rental is sized from.
func diskDeploy(t *testing.T, store modelstores.ModelStore, minDisk int32) []int32 {
	t.Helper()
	st, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{local.New()}, st, "default", provisioners.WithModelStore(store))
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Wait: true,
		Deployment: &provisionerv1.Deployment{
			Id: "big", Model: "Qwen/Qwen2.5-72B-Instruct", Image: "vllm/vllm-openai:v0.27.1",
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider: provisioners.ProviderLocal, Replicas: 1,
			Requirements: &provisionerv1.ResourceRequirements{GpuCount: 1, MinDiskGb: minDisk},
		}},
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	var got []int32
	for _, rs := range resp.GetDeployment().GetReplicaSpecs() {
		got = append(got, rs.GetRequirements().GetMinDiskGb())
	}
	return got
}

// A 72B's 145 GB download filled Vast's 40 GB default partway through, after
// the rental had started. The hub knows the size, so the rental is sized
// from it.
func TestDeploySizesTheDiskFromTheCheckpoint(t *testing.T) {
	got := diskDeploy(t, &checkpointStore{bytes: 145_000_000_000}, 0)
	if len(got) != 1 || got[0] != 200 {
		t.Errorf("rented with min_disk_gb %v, want [200]", got)
	}
}

func TestDeployKeepsALargerDiskTheOperatorAskedFor(t *testing.T) {
	got := diskDeploy(t, &checkpointStore{bytes: 145_000_000_000}, 700)
	if len(got) != 1 || got[0] != 700 {
		t.Errorf("rented with min_disk_gb %v, want [700]", got)
	}
}

func TestDeployLeavesTheDiskAloneWhenTheSizeIsUnknown(t *testing.T) {
	for name, store := range map[string]modelstores.ModelStore{
		"no capability": modelstores.Passthrough{},
		"hub error":     &checkpointStore{err: errors.New("hub unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			got := diskDeploy(t, store, 0)
			if len(got) != 1 || got[0] != 0 {
				t.Errorf("rented with min_disk_gb %v, want [0] so the provider default applies", got)
			}
		})
	}
}
