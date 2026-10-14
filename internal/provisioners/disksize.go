package provisioners

import (
	"context"
	"log/slog"
	"math"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
)

// Disk a cold deploy needs beyond the weights. The vLLM image unpacks to
// roughly 20-25 GB, and the engine writes compile and kernel caches beside
// it, so 40 GB covers the image with room left over.
const engineImageDiskGB = 40

// checkpointDiskHeadroom covers what the hub's file listing does not:
// tokenizer and config files, partial-download temporaries, and filesystem
// overhead on the rented disk.
const checkpointDiskHeadroom = 1.1

// DiskFloorGB is the container disk a cold deploy of a checkpoint this size
// needs: the weights with 10% headroom plus the engine image, rounded up to
// the next 10 GB. 0 when the size is unknown.
//
// A 72B at BF16 downloads 145 GB and gets 200; cyankiwi/GLM-5.2-AWQ-INT4 at
// 474 GB gets 570.
func DiskFloorGB(downloadBytes int64) int32 {
	if downloadBytes <= 0 {
		return 0
	}
	gb := float64(downloadBytes)*checkpointDiskHeadroom/1e9 + engineImageDiskGB
	return int32(math.Ceil(gb/10) * 10)
}

// sizeDiskForCheckpoint raises each auto-provisioned replica's min_disk_gb
// to what the model's download needs, when the model store can say.
//
// The providers each had a fixed default (Vast 40 GB, RunPod 20 GB) that
// holds a small model and not a 70B, so a deploy that did not say
// --min-disk-gb filled its disk partway through the download and failed
// after the rental had started. The pre-flight already talks to the hub, and
// the hub knows the answer.
//
// Raises and never lowers, the same floor semantics a --class has: an
// operator who asked for more keeps it. Skipped for a warm mount, where the
// weights are on a volume rather than the container disk, and for an
// attached engine, where nothing is rented. A store that cannot report a
// size leaves the request alone and each provider's default applies.
func (s *Service) sizeDiskForCheckpoint(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) {
	dep := req.GetDeployment()
	if isExternalDeploy(req) || len(dep.GetMounts()) > 0 {
		return
	}
	var reqs []*provisionerv1.ResourceRequirements
	for _, spec := range req.GetReplicasSpec() {
		if r := spec.GetRequirements(); r != nil {
			reqs = append(reqs, r)
		}
	}
	if len(reqs) == 0 {
		return
	}
	src, ok := s.modelStore.(modelstores.CheckpointSource)
	if !ok {
		return
	}
	cp, err := src.Checkpoint(ctx, &provisionerv1.DescribeModelRequest{ModelSpec: dep.GetModel()})
	if err != nil {
		slog.Info("no download size for this model; the provider's default container disk applies",
			"deployment", dep.GetId(), "model", dep.GetModel(), "reason", err)
		return
	}
	floor := DiskFloorGB(cp.GetDownloadBytes())
	if floor == 0 {
		return
	}
	for _, r := range reqs {
		if r.GetMinDiskGb() < floor {
			slog.Info("sizing the container disk from the checkpoint",
				"deployment", dep.GetId(), "model", dep.GetModel(),
				"requested_gb", r.GetMinDiskGb(), "sized_gb", floor)
			r.MinDiskGb = floor
		}
	}
}
