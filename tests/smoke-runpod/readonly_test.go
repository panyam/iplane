//go:build runpod_readonly

// Read-only checks of the RunPod adapter against the live REST v2 API.
// Every call is a GET: the pod list and the account's SSH keys. Nothing
// is created, started, stopped or modified, so this costs nothing.
//
// It has its own build tag, separate from smoke_runpod, because
// `make smoke-runpod` rents a pod on every run; this file can never be
// compiled alongside that test. As a second guard the client's transport
// refuses any method other than GET before it reaches the network.
//
// Run:
//
//	export RUNPOD_API_KEY=...
//	make runpod-v2-check
//
// What it proves: the adapter's v2 decoders read what RunPod actually
// returns. The unit tests prove the same against fixtures written from
// RunPod's OpenAPI document, which is a claim about the document rather
// than about the service.
package smoke_runpod

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
)

// getOnly fails any request that is not a GET, so a mistake in this file
// (or in the adapter) cannot turn a read-only check into a rental.
type getOnly struct{ inner http.RoundTripper }

func (g getOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("read-only check refused %s %s", r.Method, r.URL)
	}
	return g.inner.RoundTrip(r)
}

func readOnlyProvider(t *testing.T) *runpod.Provider {
	t.Helper()
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set; skipping (real-API read-only check)")
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: getOnly{http.DefaultTransport}}
	return runpod.New(runpod.NewClient(apiKey, runpod.WithHTTPClient(hc)))
}

// GET /v2/pods, every page.
func TestRunPodReadOnly_Pods(t *testing.T) {
	p := readOnlyProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refs, err := p.List(ctx, map[string]string{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("List returned %d pods on this account", len(refs))
	for i, r := range refs {
		if i >= 5 {
			t.Logf("  ... %d more", len(refs)-5)
			break
		}
		t.Logf("  [%d] provider_id=%s provider_state=%s tags=%v",
			i, r.GetProviderId(), r.GetProviderState(), r.GetTags())
	}
}

// GET /v2/account/ssh-keys.
func TestRunPodReadOnly_SSHKeys(t *testing.T) {
	p := readOnlyProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	blob, err := p.FetchUserPubKey(ctx)
	if err != nil {
		t.Fatalf("FetchUserPubKey: %v", err)
	}
	t.Logf("account has %d registered SSH key line(s)", strings.Count(blob, "\n"))
	for _, line := range strings.Split(strings.TrimSpace(blob), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			comment := ""
			if len(f) > 2 {
				comment = strings.Join(f[2:], " ")
			}
			t.Logf("  %s ...%s %s", f[0], f[1][max(0, len(f[1])-8):], comment)
		}
	}
}
