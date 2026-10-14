package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// runClosedLoop pushes total requests through a queued router fronting
// `replicas` slow fake engines, from `clients` concurrent callers, and
// returns the wall time and the peak number of requests in flight
// across all engines.
func runClosedLoop(t *testing.T, replicas, workers, perReplicaCap, clients, total int, engineDelay time.Duration) (time.Duration, int32) {
	t.Helper()
	var inFlight, peak atomic.Int32
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(engineDelay)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	var backing []*provisionerv1.ReplicaBacking
	for i := range replicas {
		eng := httptest.NewServer(slow)
		t.Cleanup(eng.Close)
		backing = append(backing, &provisionerv1.ReplicaBacking{
			InstanceIds:    []string{fmt.Sprintf("r%d", i)},
			EngineEndpoint: eng.URL,
		})
	}
	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:       "d",
				State:    provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				Replicas: backing,
			}}, nil
		},
	}, nil, WithQueue(workers, 256), WithInFlightCap(perReplicaCap))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Shutdown()
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	var next atomic.Int32
	var wg sync.WaitGroup
	start := time.Now()
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int32(total) {
				resp, err := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
				if err != nil {
					t.Errorf("post: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("status = %d, want 200", resp.StatusCode)
				}
			}
		}()
	}
	wg.Wait()
	return time.Since(start), peak.Load()
}

// Little's law: a dispatcher that holds each request for its whole
// engine round trip R caps throughput at workers/R however many
// replicas sit behind it. With the per-replica in-flight cap sizing
// each deployment's concurrency, four replicas must deliver close to
// four times the throughput of one even when the worker pool is
// smaller than the deployment's capacity.
func TestRouter_Queued_ThroughputScalesWithReplicas(t *testing.T) {
	const (
		workers       = 2
		perReplicaCap = 2
		clients       = 16
		total         = 48
		delay         = 40 * time.Millisecond
	)
	one, peakOne := runClosedLoop(t, 1, workers, perReplicaCap, clients, total, delay)
	four, peakFour := runClosedLoop(t, 4, workers, perReplicaCap, clients, total, delay)

	speedup := float64(one) / float64(four)
	t.Logf("1 replica: %v (peak %d), 4 replicas: %v (peak %d), speedup %.2fx", one, peakOne, four, peakFour, speedup)
	if speedup < 2.5 {
		t.Errorf("4 replicas ran %.2fx faster than 1, want close to 4x; the dispatch pool is capping throughput", speedup)
	}
	if peakOne > perReplicaCap {
		t.Errorf("1 replica: peak in flight %d exceeds the per-replica cap %d", peakOne, perReplicaCap)
	}
	if peakFour > 4*perReplicaCap {
		t.Errorf("4 replicas: peak in flight %d exceeds cap x replicas = %d", peakFour, 4*perReplicaCap)
	}
}
