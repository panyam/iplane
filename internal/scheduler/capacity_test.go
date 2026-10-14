package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// replicaEntry is a fakeEntry that reports how many replicas its
// deployment can route to right now.
type replicaEntry struct {
	fakeEntry
	replicas int
}

func (e *replicaEntry) Replicas() int { return e.replicas }

// trackPeak returns a handler that blocks on hold and records the peak
// number of handlers running at once.
func trackPeak(hold <-chan struct{}, inFlight, peak *atomic.Int32) HandlerFunc {
	return func(_ context.Context, _ Entry) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		<-hold
		inFlight.Add(-1)
	}
}

func waitInFlight(inFlight *atomic.Int32, want int32) int32 {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && inFlight.Load() < want {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // let any overshoot show
	return inFlight.Load()
}

// One worker, a per-replica cap of 2 and a three-replica deployment:
// six requests run at once. The worker hands each request off once it
// holds a slot, so the worker count no longer bounds concurrency; the
// deployment's capacity (cap x replicas) does.
func TestInteractiveFirst_ConcurrencyScalesWithReplicasNotWorkers(t *testing.T) {
	const workers, perReplica, replicas = 1, 2, 3
	hold := make(chan struct{})
	var inFlight, peak atomic.Int32
	s := newScheduler(t, workers, perReplica, trackPeak(hold, &inFlight, &peak))
	s.Start(context.Background())
	defer func() {
		close(hold)
		s.Stop()
	}()

	for i := range 10 {
		e := &replicaEntry{fakeEntry: fakeEntry{seq: i, deploy: "d1", priority: LaneInteractive}, replicas: replicas}
		if err := s.Submit(e); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if got := waitInFlight(&inFlight, perReplica*replicas); got != perReplica*replicas {
		t.Fatalf("in flight = %d, want %d (cap %d x %d replicas, independent of %d worker)",
			got, perReplica*replicas, perReplica, replicas, workers)
	}
	if got := peak.Load(); got != perReplica*replicas {
		t.Errorf("peak in flight = %d, want %d (deployment cap must still hold)", got, perReplica*replicas)
	}
}

// With in_flight_cap = 0 there is no per-deployment bound to hand off
// against, so workers still carry each request and k stays the global
// bound. Demo 05 depends on this to make a queue build.
func TestInteractiveFirst_UnlimitedCap_WorkersStillBound(t *testing.T) {
	const workers = 2
	hold := make(chan struct{})
	var inFlight, peak atomic.Int32
	s := newScheduler(t, workers, 0, trackPeak(hold, &inFlight, &peak))
	s.Start(context.Background())
	defer func() {
		close(hold)
		s.Stop()
	}()

	for i := range 6 {
		e := &replicaEntry{fakeEntry: fakeEntry{seq: i, deploy: "d1", priority: LaneInteractive}, replicas: 4}
		_ = s.Submit(e)
	}
	if got := waitInFlight(&inFlight, workers); got != workers {
		t.Fatalf("in flight = %d, want %d (workers bound when cap is unlimited)", got, workers)
	}
}

// Stop waits for handed-off requests, not just for the workers.
func TestInteractiveFirst_Stop_WaitsForHandedOffRequests(t *testing.T) {
	release := make(chan struct{})
	var finished atomic.Bool
	s := newScheduler(t, 1, 1, func(_ context.Context, _ Entry) {
		<-release
		finished.Store(true)
	})
	s.Start(context.Background())
	_ = s.Submit(&fakeEntry{seq: 1, deploy: "d1", priority: LaneInteractive})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && s.Len(LaneInteractive) != 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(release)
	}()
	s.Stop()
	if !finished.Load() {
		t.Fatal("Stop returned before the in-flight request finished")
	}
}

// rejectEntry records whether the scheduler gave up on it.
type rejectEntry struct {
	fakeEntry
	rejected chan error
}

func (e *rejectEntry) Reject(err error) { e.rejected <- err }

// Stop with one entry blocked on a full deployment cap and another
// still in the lane: neither will ever reach the handler, so both must
// be rejected, or whoever is waiting on them (an HTTP client) hangs.
// The slot waiter is rejected without waiting for the in-flight
// request to finish.
func TestInteractiveFirst_Stop_RejectsEntriesThatNeverDispatch(t *testing.T) {
	hold := make(chan struct{})
	running := make(chan struct{}, 1)
	s := newScheduler(t, 1, 1, func(_ context.Context, _ Entry) {
		running <- struct{}{}
		<-hold
	})
	s.Start(context.Background())

	inFlight := &rejectEntry{fakeEntry: fakeEntry{seq: 1, deploy: "d1", priority: LaneInteractive}, rejected: make(chan error, 1)}
	waiter := &rejectEntry{fakeEntry: fakeEntry{seq: 2, deploy: "d1", priority: LaneInteractive}, rejected: make(chan error, 1)}
	queued := &rejectEntry{fakeEntry: fakeEntry{seq: 3, deploy: "d1", priority: LaneBatch}, rejected: make(chan error, 1)}
	_ = s.Submit(inFlight)
	<-running
	_ = s.Submit(waiter)
	if !waitForLen(func() int { return s.Len(LaneInteractive) }, 0) {
		t.Fatal("worker never popped the slot waiter")
	}
	time.Sleep(10 * time.Millisecond) // worker is now parked in acquire
	_ = s.Submit(queued)

	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()

	for _, e := range []*rejectEntry{waiter, queued} {
		select {
		case err := <-e.rejected:
			if err != ErrSchedulerClosed {
				t.Errorf("entry %d rejected with %v, want ErrSchedulerClosed", e.seq, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("entry %d was dropped at Stop without being rejected", e.seq)
		}
	}
	close(hold)
	<-stopped
	select {
	case err := <-inFlight.rejected:
		t.Errorf("the dispatched entry was also rejected: %v", err)
	default:
	}
}
