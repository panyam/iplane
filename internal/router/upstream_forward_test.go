package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// recordingUpstream stands in for a hosted API and records what reached it.
type recordingUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	hits    int
	path    string
	headers http.Header
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		u.path = r.URL.Path
		u.headers = r.Header.Clone()
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ok"}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func runningDep(endpoint string, auth *provisionerv1.UpstreamAuth) *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:             "hosted",
		Model:          "m/hosted",
		State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		EngineEndpoint: endpoint,
		UpstreamAuth:   auth,
	}
}

// postFlat and postDeployID send one chat completion through each route with
// the caller credentials a client might plausibly carry.
var callerCredentials = map[string]string{
	"Authorization":       "Bearer caller-secret",
	"X-Api-Key":           "caller-x-api-key",
	"Api-Key":             "caller-api-key",
	"Cookie":              "session=caller-cookie",
	"Proxy-Authorization": "Basic Y2FsbGVyOnNlY3JldA==",
}

func postThrough(t *testing.T, dep *provisionerv1.Deployment, flat bool) *http.Response {
	t.Helper()
	var r *Router
	if flat {
		r = newFlatTestRouter(func(*provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
			return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{dep}}, nil
		})
	} else {
		r = newTestRouter(func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: dep}, nil
		})
	}
	srv := httptest.NewServer(serveThroughMux(r))
	t.Cleanup(srv.Close)

	path := "/v1/chat/completions"
	if !flat {
		path = "/v1/" + dep.GetId() + "/v1/chat/completions"
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path,
		strings.NewReader(`{"model":"m/hosted","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range callerCredentials {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func forBothRoutes(t *testing.T, fn func(t *testing.T, flat bool)) {
	t.Run("flat", func(t *testing.T) { fn(t, true) })
	t.Run("deploy-id", func(t *testing.T) { fn(t, false) })
}

// The leak the Ch 11 review found: --upstream-auth-env names a variable that
// is unset in the daemon's environment, and the caller's own Authorization
// went to the hosted provider in its place. The router must refuse the
// forward rather than send anything.
func TestUpstreamAuthMissingVariableFailsClosed(t *testing.T) {
	forBothRoutes(t, func(t *testing.T, flat bool) {
		up := newRecordingUpstream(t)
		dep := runningDep(up.URL, &provisionerv1.UpstreamAuth{
			Header: "Authorization", ValueEnv: "TEST_UPSTREAM_FORWARD_UNSET", ValuePrefix: "Bearer ",
		})

		resp := postThrough(t, dep, flat)

		if up.hits != 0 {
			t.Fatalf("upstream was dialed with Authorization=%q; a missing credential must not forward at all",
				up.headers.Get("Authorization"))
		}
		if resp.StatusCode < 500 {
			t.Errorf("status = %d, want a 5xx", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "TEST_UPSTREAM_FORWARD_UNSET") {
			t.Errorf("error should name the missing variable; got %s", body)
		}
		if strings.Contains(string(body), "caller-secret") {
			t.Errorf("error body echoes the caller's credential: %s", body)
		}
	})
}

// The same variable set to the empty string is the same failure.
func TestUpstreamAuthEmptyVariableFailsClosed(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_FORWARD_EMPTY", "")
	up := newRecordingUpstream(t)
	dep := runningDep(up.URL, &provisionerv1.UpstreamAuth{
		Header: "Authorization", ValueEnv: "TEST_UPSTREAM_FORWARD_EMPTY", ValuePrefix: "Bearer ",
	})

	resp := postThrough(t, dep, true)

	if up.hits != 0 || resp.StatusCode < 500 {
		t.Errorf("hits=%d status=%d; want no forward and a 5xx", up.hits, resp.StatusCode)
	}
}

// Caller credentials never reach an upstream, whether or not the deployment
// presents one of its own.
func TestCallerCredentialsAreNeverForwarded(t *testing.T) {
	forBothRoutes(t, func(t *testing.T, flat bool) {
		up := newRecordingUpstream(t)

		resp := postThrough(t, runningDep(up.URL, nil), flat)

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d; body=%s", resp.StatusCode, b)
		}
		for k := range callerCredentials {
			if v := up.headers.Values(k); len(v) > 0 {
				t.Errorf("upstream received caller's %s: %v", k, v)
			}
		}
	})
}

func TestUpstreamAuthReplacesCallerCredentials(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_FORWARD_KEY", "ours")
	forBothRoutes(t, func(t *testing.T, flat bool) {
		up := newRecordingUpstream(t)
		dep := runningDep(up.URL, &provisionerv1.UpstreamAuth{
			Header: "X-Api-Key", ValueEnv: "TEST_UPSTREAM_FORWARD_KEY",
		})

		resp := postThrough(t, dep, flat)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := up.headers.Values("X-Api-Key"); len(got) != 1 || got[0] != "ours" {
			t.Errorf("X-Api-Key = %v, want exactly our credential", got)
		}
		for _, k := range []string{"Authorization", "Api-Key", "Cookie", "Proxy-Authorization"} {
			if v := up.headers.Values(k); len(v) > 0 {
				t.Errorf("upstream received caller's %s: %v", k, v)
			}
		}
	})
}

// A hosted base URL is usually written the way the vendor documents it, with
// /v1 on the end. Both routes must land on the vendor's /v1/chat/completions,
// not /v1/v1/..., and a vendor prefix such as /openai/v1 must survive on both.
func TestUpstreamBasePathJoinsWithoutDoubling(t *testing.T) {
	cases := []struct{ base, want string }{
		{"", "/v1/chat/completions"},
		{"/", "/v1/chat/completions"},
		{"/v1", "/v1/chat/completions"},
		{"/v1/", "/v1/chat/completions"},
		{"/openai/v1", "/openai/v1/chat/completions"},
		{"/openai", "/openai/v1/chat/completions"},
	}
	for _, c := range cases {
		t.Run(c.base, func(t *testing.T) {
			forBothRoutes(t, func(t *testing.T, flat bool) {
				up := newRecordingUpstream(t)
				resp := postThrough(t, runningDep(up.URL+c.base, nil), flat)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				if up.path != c.want {
					t.Errorf("base %q: upstream path = %q, want %q", c.base, up.path, c.want)
				}
			})
		})
	}
}

func TestJoinUpstreamPath(t *testing.T) {
	cases := []struct{ base, tail, want string }{
		{"", "/v1/models", "/v1/models"},
		{"/v1", "/v1/models", "/v1/models"},
		{"/v1", "/v1", "/v1"},
		{"/v1", "/v10/x", "/v1/v10/x"},
		{"/api/v1/", "/v1/chat/completions", "/api/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := joinUpstreamPath(c.base, c.tail); got != c.want {
			t.Errorf("joinUpstreamPath(%q, %q) = %q, want %q", c.base, c.tail, got, c.want)
		}
	}
}
