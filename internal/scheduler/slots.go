package scheduler

import (
	"context"
	"sync"
)

// ReplicaCounter is the optional Entry side-band that tells the
// scheduler how many replicas the entry's deployment can route to
// right now. The in-flight cap is per replica, so a deployment's
// concurrency bound is InFlightCap x Replicas(). Entries that do not
// implement it, or report fewer than one, count as one replica.
type ReplicaCounter interface {
	Replicas() int
}

func replicasFromEntry(e Entry) int {
	if rc, ok := e.(ReplicaCounter); ok {
		if n := rc.Replicas(); n > 1 {
			return n
		}
	}
	return 1
}

// deploymentSlots counts one deployment's in-flight requests against a
// limit that can change between acquisitions. A buffered channel
// cannot be resized, and the limit moves whenever the deployment
// scales or a replica is quarantined, so each acquire passes the limit
// it read from the deployment record it is about to route against.
//
// Shrinking the limit never preempts anything already running; new
// acquires wait until the count drains below the new limit.
type deploymentSlots struct {
	mu    sync.Mutex
	inUse int
	freed chan struct{} // closed and replaced on every release
}

func newDeploymentSlots() *deploymentSlots {
	return &deploymentSlots{freed: make(chan struct{})}
}

// acquire blocks until fewer than limit slots are in use and takes
// one. Returns false if ctx ends first.
func (d *deploymentSlots) acquire(ctx context.Context, limit int) bool {
	for {
		d.mu.Lock()
		if d.inUse < limit {
			d.inUse++
			d.mu.Unlock()
			return true
		}
		wake := d.freed
		d.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return false
		}
	}
}

func (d *deploymentSlots) release() {
	d.mu.Lock()
	d.inUse--
	close(d.freed)
	d.freed = make(chan struct{})
	d.mu.Unlock()
}
