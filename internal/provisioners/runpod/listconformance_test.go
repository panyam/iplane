package runpod

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/providertest"
)

// podListProvider builds a Provider whose GET /v2/pods answers from `pods`
// in v2's wrapped shape. v2 has no server-side name filter (v1 honoured
// ?name=), so the fake ignores any query and returns the whole account:
// the client-side match is the only narrowing there is, which is exactly
// what the conformance suite has to hold to the contract.
func podListProvider(t *testing.T, pods map[string]string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/pods" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		rows := make([]string, 0, len(pods))
		for id, name := range pods {
			rows = append(rows, `{"id":"`+id+`","name":"`+name+`","status":"RUNNING"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pods":[` + strings.Join(rows, ",") + `],"pagination":{"nextCursor":null,"hasNextPage":false}}`))
	}))
	t.Cleanup(srv.Close)
	return New(NewClient("test-api-key", WithBaseURL(srv.URL+"/v2"), WithHTTPClient(srv.Client())))
}

// RunPod encodes iplane-id in the pod name and nothing else, so the shared
// suite holds it to the contract over that tag. It was the one adapter
// already matching every filter key, and running the suite here is what
// keeps the three of them answering the same way.
func TestListFilterConformance(t *testing.T) {
	p := podListProvider(t, map[string]string{
		"pod-1": podNamePrefix + "foo",
		"pod-2": podNamePrefix + "bar",
	})
	providertest.RunListFilterConformance(t, p, []providertest.SeededInstance{
		{ProviderID: "pod-1", Tags: map[string]string{provisioners.TagID: "foo"}},
		{ProviderID: "pod-2", Tags: map[string]string{provisioners.TagID: "bar"}},
	})
}

// A pod the operator created outside iplane carries no iplane- prefix and so
// no iplane id. The Service's ownership check reads exactly this tag.
func TestListStampsNoIDOnAForeignPod(t *testing.T) {
	p := podListProvider(t, map[string]string{"pod-9": "my-notebook"})
	refs, err := p.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1", len(refs))
	}
	if got := refs[0].GetTags()[provisioners.TagID]; got != "" {
		t.Errorf("foreign pod carries %s=%q, want no iplane id", provisioners.TagID, got)
	}
}
