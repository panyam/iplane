package runpod

import (
	"context"
	"net/url"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// RunPod network-volume surface (REST v2 /network-volumes; v1 spelled it
// /networkvolumes). A network volume is datacenter-locked persistent
// storage; iplane uses it as a shared warm model cache (many models under
// one HF layout). These back the VolumeManager capability the warm-cache
// pin flow drives.

// networkVolumeCreate is the POST /network-volumes request body. size is
// GB (v2 range 10-4096); dataCenter pins the volume's datacenter. The
// optional storage `type` is left to the datacenter's default.
type networkVolumeCreate struct {
	Name       string `json:"name"`
	Size       int    `json:"size"`
	DataCenter string `json:"dataCenter"`
}

// networkVolume is the API's volume record. id is the handle passed as
// mounts.network[].volumeId when attaching the volume to a pod.
type networkVolume struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int    `json:"size"`
	DataCenter string `json:"dataCenter"`
}

// listNetworkVolumesResponse is GET /network-volumes: the volumes wrapped
// under a key, plus the cursor block.
type listNetworkVolumesResponse struct {
	NetworkVolumes []networkVolume `json:"networkVolumes"`
	Pagination     pagination      `json:"pagination"`
}

func (p *Provider) createNetworkVolume(ctx context.Context, name, dataCenterID string, sizeGB int) (networkVolume, error) {
	req, err := p.client.newReq("POST", "/network-volumes", nil, networkVolumeCreate{
		Name:       name,
		Size:       sizeGB,
		DataCenter: dataCenterID,
	})
	if err != nil {
		return networkVolume{}, wrapErr("create network volume", err)
	}
	vol, err := skhttp.Call[networkVolume](ctx, req, p.client.callOpts()...)
	if err != nil {
		return networkVolume{}, wrapErr("create network volume", err)
	}
	return vol, nil
}

func (p *Provider) listNetworkVolumes(ctx context.Context) ([]networkVolume, error) {
	return listAll(ctx, p.client, "list network volumes", "/network-volumes",
		func(r listNetworkVolumesResponse) ([]networkVolume, pagination) {
			return r.NetworkVolumes, r.Pagination
		})
}

func (p *Provider) deleteNetworkVolume(ctx context.Context, id string) error {
	req, err := p.client.newReq("DELETE", "/network-volumes/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return wrapErr("delete network volume", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		return wrapErr("delete network volume", err)
	}
	return nil
}

// EnsureVolume finds a same-named volume in the region or creates one,
// satisfying provisioners.VolumeManager. Idempotent: pinning a second
// model to a region reuses the first pin's volume instead of creating a
// duplicate cache.
func (p *Provider) EnsureVolume(ctx context.Context, spec provisioners.VolumeSpec) (provisioners.VolumeRef, error) {
	existing, err := p.listNetworkVolumes(ctx)
	if err != nil {
		return provisioners.VolumeRef{}, err
	}
	for _, v := range existing {
		if v.Name == spec.Name && v.DataCenter == spec.Region {
			return volumeRef(v), nil
		}
	}
	created, err := p.createNetworkVolume(ctx, spec.Name, spec.Region, spec.SizeGB)
	if err != nil {
		return provisioners.VolumeRef{}, err
	}
	return volumeRef(created), nil
}

// ListVolumes returns the account's network volumes as VolumeRefs.
func (p *Provider) ListVolumes(ctx context.Context) ([]provisioners.VolumeRef, error) {
	vols, err := p.listNetworkVolumes(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]provisioners.VolumeRef, 0, len(vols))
	for _, v := range vols {
		refs = append(refs, volumeRef(v))
	}
	return refs, nil
}

// DeleteVolume destroys a network volume (and everything staged on it).
func (p *Provider) DeleteVolume(ctx context.Context, volumeID string) error {
	return p.deleteNetworkVolume(ctx, volumeID)
}

func volumeRef(v networkVolume) provisioners.VolumeRef {
	return provisioners.VolumeRef{ID: v.ID, Name: v.Name, Region: v.DataCenter, SizeGB: v.Size}
}

var _ provisioners.VolumeManager = (*Provider)(nil)
