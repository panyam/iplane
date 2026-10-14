package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/inference-book/inference-plane/internal/stores/queue"
)

// InteractiveFirst is the v0.2 default Scheduler impl. Strict
// priority across lanes (interactive preferred); weighted
// fair-share within a lane (per-tenant sub-queues + weighted
// lottery on each tryPop).
//
// # Worker loop
//
// Each worker round:
//
//  1. Try interactive lane (weighted lottery across non-empty
//     tenants). On hit, dispatch and loop.
//  2. Try batch lane (same). On hit, dispatch and loop.
//  3. Both empty: sleep briefly (exponential backoff capped at
//     idleMaxBackoff). On the next tick, try again.
//
// Direct queue inspection (rather than a feeder-channel design)
// guarantees strict priority deterministically: the worker always
// looks at the interactive lane FIRST before considering batch.
//
// # Fair-share (v0.2 ch7-beat2.5)
//
// Within a lane, each tenant has its own bounded sub-queue. tryPop
// runs a weighted lottery over tenants whose sub-queues are
// non-empty: pick a uniformly-random integer in [0, totalWeight)
// and select the tenant whose cumulative-weight interval contains
// it. Under sustained saturation the dispatch ratio converges to
// the configured weight ratio. Tenants without an entry in the
// operator-supplied Weights map get DefaultTenantWeight (= 1).
//
// # In-flight cap and who holds a request
//
// InFlightCap is per replica, mirroring the engine's max-num-seqs,
// which is a per-engine setting. A deployment's concurrency bound is
// InFlightCap x the replicas its entries report (ReplicaCounter), so
// it grows when the deployment scales and shrinks when a replica is
// quarantined.
//
// When cap > 0 a worker only dispatches: it pops an entry, waits for
// one of the deployment's slots, then runs the handler on its own
// goroutine and goes back to the queue. The slot, not the worker, is
// held for the engine round trip. This is what keeps the worker count
// k from becoming a global ceiling. A worker that held each request
// for its whole round trip R capped the daemon at k/R requests per
// second across every deployment (Little's law), so adding replicas
// behind the router did nothing once k requests were in flight.
//
// When cap is 0 (unlimited) there is no per-deployment bound to hand
// off against, so the worker runs the handler inline and k is the one
// global concurrency bound. That is the deliberate small-window mode
// Demo 05 uses to make a queue build.
//
// Either way, strict priority and fair share act at the pop, which
// happens only when a worker is free and, with a cap, only after the
// previous entry got its slot. Queued entries wait in the lanes, so
// an interactive arrival still overtakes waiting batch work.
//
// # Starvation
//
// Strict cross-lane priority CAN starve batch under sustained
// interactive load. This is the chapter narrative ("interactive
// cuts ahead of batch") and the v0.2 demo 05 talking point.
// Starvation prevention (WRR / DWRR / aging across lanes) is filed
// as #132 and would land as a sibling Scheduler impl.
type InteractiveFirst struct {
	workers     int
	handler     HandlerFunc
	inFlightCap int

	interactive *laneState
	batch       *laneState

	// observer (optional) receives push/pop notifications so the
	// router-layer metrics package can emit queue-depth + wait-time
	// histograms without coupling this package to internal/metrics.
	observer Observer

	// Lifecycle. Start sets rootCtx + spawns goroutines; Stop
	// cancels rootCtx, closes lane sub-queues, waits.
	mu         sync.Mutex
	rootCtx    context.Context
	rootCancel context.CancelFunc
	started    bool
	wg         sync.WaitGroup

	// Per-deployment in-flight slots. Lazily created on first
	// dispatch to a given deployment id. When inFlightCap == 0
	// this map stays empty and the workers skip the acquire path.
	slotsMu sync.Mutex
	slots   map[string]*deploymentSlots
}

// idle backoff bounds. Workers poll the queues directly; when both
// lanes are empty they sleep `idleStartBackoff` and double up to
// idleMaxBackoff. Active workloads never reach the cap.
const (
	idleStartBackoff = time.Millisecond
	idleMaxBackoff   = 10 * time.Millisecond
)

// Observer is the optional hook the scheduler invokes when an
// entry enters or exits a sub-queue. Implementations record
// queue-depth + wait-time metrics; the scheduler stays decoupled
// from the metrics package by accepting this interface.
//
// All methods must be cheap and non-blocking -- they run inline on
// the Submit / dispatch path. nil-safe via the noOpObserver
// fallback (set by NewInteractiveFirst when no observer is
// supplied).
type Observer interface {
	// OnPush is called after a successful queue push. lane is one
	// of LaneInteractive / LaneBatch; tenantID is the operator-
	// asserted tenant; depth is the post-push sub-queue depth for
	// (lane, tenantID).
	OnPush(lane, tenantID string, depth int)
	// OnPop is called after a successful pop. waitDuration is the
	// time the entry spent in the queue (now - enqueuedAt).
	OnPop(lane, tenantID, deploymentID string, depth int, waitDuration time.Duration)
}

// noOpObserver is the default when no observer is configured.
// Keeps the dispatch path free of nil-checks.
type noOpObserver struct{}

func (noOpObserver) OnPush(string, string, int)                              {}
func (noOpObserver) OnPop(string, string, string, int, time.Duration)         {}

// EnqueueTimestamper is implemented by entries that need to be
// stamped with their enqueue time at Submit so the dispatcher can
// compute wait duration on Pop. The router's queueEntry
// implements this; tests can opt out by not implementing it (in
// which case waitDuration is reported as 0).
type EnqueueTimestamper interface {
	StampEnqueued(t time.Time)
	EnqueuedAt() time.Time
}

// InteractiveFirstConfig configures the scheduler at construction.
type InteractiveFirstConfig struct {
	// Workers is k: the number of dispatcher goroutines. With
	// InFlightCap > 0 they only pop and hand off, so k bounds how
	// many deployments can be waiting on a full cap at once, not how
	// many requests are in flight. With InFlightCap == 0 each worker
	// carries its request to completion and k is the global bound.
	Workers int

	// InteractiveCapacity / BatchCapacity bound each PER-TENANT
	// sub-queue's waiting room. Total lane capacity scales with
	// the number of distinct tenants seen.
	InteractiveCapacity int
	BatchCapacity       int

	// InFlightCap is the concurrent in-flight bound per replica; a
	// deployment gets InFlightCap x its replica count. 0 means
	// unlimited, leaving Workers as the only bound.
	InFlightCap int

	// Weights is the per-tenant fair-share table. Tenants not
	// listed get DefaultTenantWeight on first Submit. Nil means
	// "default-weighted everyone" (= unweighted RR).
	Weights Weights

	// Handler is invoked for every dispatched entry.
	Handler HandlerFunc

	// Observer receives push/pop notifications for the metric
	// emission path. Optional; nil installs a no-op observer.
	Observer Observer
}

// NewInteractiveFirst constructs a scheduler with the given config.
// Panics on invalid input -- programmer errors.
func NewInteractiveFirst(cfg InteractiveFirstConfig) *InteractiveFirst {
	if cfg.Handler == nil {
		panic("scheduler: handler must not be nil")
	}
	if cfg.Workers <= 0 {
		panic("scheduler: workers must be > 0")
	}
	if cfg.InteractiveCapacity <= 0 {
		panic("scheduler: interactive capacity must be > 0")
	}
	if cfg.BatchCapacity <= 0 {
		panic("scheduler: batch capacity must be > 0")
	}
	if cfg.InFlightCap < 0 {
		panic("scheduler: in-flight cap must be >= 0 (0 = unlimited)")
	}
	weights := cfg.Weights
	if weights == nil {
		weights = Weights{}
	}
	obs := cfg.Observer
	if obs == nil {
		obs = noOpObserver{}
	}
	return &InteractiveFirst{
		workers:     cfg.Workers,
		handler:     cfg.Handler,
		inFlightCap: cfg.InFlightCap,
		interactive: newLaneState(LaneInteractive, weights, cfg.InteractiveCapacity),
		batch:       newLaneState(LaneBatch, weights, cfg.BatchCapacity),
		observer:    obs,
		slots:       map[string]*deploymentSlots{},
	}
}

// Submit routes the entry to (lane, tenant) sub-queue based on the
// entry's Priority() + DeploymentID() / tenant. Pulls the tenant
// from the Entry via the optional TenantIdentifier interface; falls
// back to "default" if the entry doesn't expose a tenant
// (preserves backward compat with bare Entry impls).
func (s *InteractiveFirst) Submit(e Entry) error {
	tenantID := tenantFromEntry(e)
	lane := s.laneFor(e.Priority())
	// Stamp enqueue time before push so wait-duration is measurable
	// at pop. Entries that don't implement EnqueueTimestamper
	// produce zero waitDuration; the metric histogram filters
	// those out at the recorder layer.
	if t, ok := e.(EnqueueTimestamper); ok {
		t.StampEnqueued(time.Now())
	}
	if err := lane.push(tenantID, e); err != nil {
		return err
	}
	s.observer.OnPush(lane.name, tenantID, lane.lenForTenant(tenantID))
	return nil
}

// TenantIdentifier is the optional Entry side-band for the
// scheduler to learn a tenant id without re-parsing the entry's
// payload. Routers attach this when they want per-tenant fair-share.
type TenantIdentifier interface {
	Tenant() string
}

// tenantFromEntry pulls the tenant from a TenantIdentifier-shaped
// entry; falls back to "default" for entries that don't expose one.
// Keeps the scheduler usable with simple Entry impls (e.g., the
// recordingScheduler test no-op) that don't care about tenant.
func tenantFromEntry(e Entry) string {
	if t, ok := e.(TenantIdentifier); ok {
		if v := t.Tenant(); v != "" {
			return v
		}
	}
	return "default"
}

// laneFor maps Entry.Priority() to the matching laneState. Unknown
// priorities default to interactive (the safer default).
func (s *InteractiveFirst) laneFor(p string) *laneState {
	if p == LaneBatch {
		return s.batch
	}
	return s.interactive
}

// Start spawns `workers` dispatcher goroutines. Idempotent guard:
// calling Start twice panics.
func (s *InteractiveFirst) Start(parent context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		panic("scheduler: Start called twice")
	}
	s.started = true
	s.rootCtx, s.rootCancel = context.WithCancel(parent)
	s.mu.Unlock()

	for range s.workers {
		s.wg.Add(1)
		go s.workerLoop()
	}
}

// workerLoop is the strict-priority dispatch loop. Each round
// directly inspects the lane states: interactive first, then batch.
// When both are empty, exponential backoff up to idleMaxBackoff.
func (s *InteractiveFirst) workerLoop() {
	defer s.wg.Done()
	interactiveOpen, batchOpen := true, true
	backoff := idleStartBackoff
	for interactiveOpen || batchOpen {
		select {
		case <-s.rootCtx.Done():
			return
		default:
		}

		// Phase 1: strict-priority try interactive.
		if interactiveOpen {
			item, err := s.interactive.tryPop(s.rootCtx)
			if err == nil {
				backoff = idleStartBackoff
				s.dispatch(LaneInteractive, item)
				continue
			}
			if errors.Is(err, queue.ErrClosed) {
				interactiveOpen = false
			}
		}

		// Phase 2: try batch.
		if batchOpen {
			item, err := s.batch.tryPop(s.rootCtx)
			if err == nil {
				backoff = idleStartBackoff
				s.dispatch(LaneBatch, item)
				continue
			}
			if errors.Is(err, queue.ErrClosed) {
				batchOpen = false
			}
		}

		// Both empty (or closed): idle backoff.
		if !interactiveOpen && !batchOpen {
			return
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > idleMaxBackoff {
				backoff = idleMaxBackoff
			}
		case <-s.rootCtx.Done():
			return
		}
	}
}

// dispatch fires the observer's OnPop with wait-duration computed
// from the entry's enqueued-at timestamp, then runs the handler. With
// a cap it first takes one of the deployment's slots and hands the
// request to its own goroutine, so the worker is free for the next
// pop while the engine works; see "In-flight cap and who holds a
// request" on InteractiveFirst.
func (s *InteractiveFirst) dispatch(lane string, e Entry) {
	tenantID := tenantFromEntry(e)
	deploymentID := e.DeploymentID()
	var waitDur time.Duration
	if t, ok := e.(EnqueueTimestamper); ok {
		if ts := t.EnqueuedAt(); !ts.IsZero() {
			waitDur = time.Since(ts)
		}
	}
	// Observer fires AFTER the pop, BEFORE the per-deployment
	// slot wait -- the wait-time histogram should reflect
	// queue dwell time only, not in-flight-cap throttling.
	depth := s.laneFor(lane).lenForTenant(tenantID)
	s.observer.OnPop(lane, tenantID, deploymentID, depth, waitDur)

	if s.inFlightCap == 0 {
		s.handler(s.rootCtx, e)
		return
	}
	slots := s.slotsFor(deploymentID)
	if !slots.acquire(s.rootCtx, s.inFlightCap*replicasFromEntry(e)) {
		return
	}
	// Tracked on wg so Stop drains handed-off requests as well as
	// the workers. Add from a running worker is safe against a
	// concurrent Wait because the worker's own count is still held.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer slots.release()
		s.handler(s.rootCtx, e)
	}()
}

// slotsFor lazily creates the per-deployment in-flight counter.
func (s *InteractiveFirst) slotsFor(deployID string) *deploymentSlots {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	if existing, ok := s.slots[deployID]; ok {
		return existing
	}
	d := newDeploymentSlots()
	s.slots[deployID] = d
	return d
}

// Stop closes both lane states (which cascades to every per-tenant
// sub-queue), cancels the root context, and waits for workers to
// exit. Idempotent.
func (s *InteractiveFirst) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.interactive.close()
	s.batch.close()
	s.rootCancel()
	s.wg.Wait()
}

// Len returns the current waiting depth on the named lane (sum
// across all tenants). LaneInteractive / LaneBatch return their
// respective totals; other names return -1.
func (s *InteractiveFirst) Len(lane string) int {
	switch lane {
	case LaneInteractive:
		return s.interactive.totalLen()
	case LaneBatch:
		return s.batch.totalLen()
	default:
		return -1
	}
}

// LenForTenant returns the depth of one tenant's sub-queue in the
// named lane. Useful for tests + per-tenant metric callbacks.
// Unknown lane name returns -1; unknown tenant returns 0.
func (s *InteractiveFirst) LenForTenant(lane, tenantID string) int {
	switch lane {
	case LaneInteractive:
		return s.interactive.lenForTenant(tenantID)
	case LaneBatch:
		return s.batch.lenForTenant(tenantID)
	default:
		return -1
	}
}

// Tenants returns the set of tenants observed in the named lane.
// Used by async metric callbacks that iterate per-tenant depth.
func (s *InteractiveFirst) Tenants(lane string) []string {
	switch lane {
	case LaneInteractive:
		return s.interactive.snapshotTenants()
	case LaneBatch:
		return s.batch.snapshotTenants()
	default:
		return nil
	}
}
