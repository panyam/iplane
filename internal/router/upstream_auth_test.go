package router

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// The credential is read from the daemon's own environment at forward time.
// Only the variable's name is ever persisted, because the deployment record
// goes to the state file and to every DescribeDeployment response.
func TestUpstreamAuthResolvesFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "sk-secret")

	h, v, err := resolveUpstreamAuth(&provisionerv1.UpstreamAuth{
		Header: "Authorization", ValueEnv: "TEST_UPSTREAM_KEY", ValuePrefix: "Bearer ",
	})

	if err != nil || h != "Authorization" || v != "Bearer sk-secret" {
		t.Errorf("got (%q, %q, %v), want the prefixed credential in Authorization", h, v, err)
	}
}

// Everything iplane provisions itself is reachable because we rented it. A
// deployment with no credential gets no header at all, not an empty one.
func TestUpstreamAuthAbsentForOwnedEngines(t *testing.T) {
	if h, v, err := resolveUpstreamAuth(nil); h != "" || v != "" || err != nil {
		t.Errorf("got (%q, %q, %v) for a deployment that named no credential", h, v, err)
	}
}

// A variable that has gone missing since deploy time is an error, never a
// silent skip: skipping used to leave the caller's own Authorization in the
// outbound request.
func TestUpstreamAuthMissingVariableIsAnError(t *testing.T) {
	_, _, err := resolveUpstreamAuth(&provisionerv1.UpstreamAuth{
		Header: "Authorization", ValueEnv: "TEST_UPSTREAM_DEFINITELY_UNSET", ValuePrefix: "Bearer ",
	})
	if err == nil || !strings.Contains(err.Error(), "TEST_UPSTREAM_DEFINITELY_UNSET") {
		t.Errorf("err = %v, want an error naming the variable", err)
	}
}

// A record with a header but no variable is misconfigured, not unauthenticated.
func TestUpstreamAuthWithoutAVariableIsAnError(t *testing.T) {
	if _, _, err := resolveUpstreamAuth(&provisionerv1.UpstreamAuth{Header: "Authorization"}); err == nil {
		t.Error("accepted an upstream auth with no value_env")
	}
}

// Some gateways want the bare token under their own header name.
func TestUpstreamAuthSupportsANonBearerHeader(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "raw-token")

	h, v, err := resolveUpstreamAuth(&provisionerv1.UpstreamAuth{
		Header: "X-Api-Key", ValueEnv: "TEST_UPSTREAM_KEY",
	})

	if err != nil || h != "X-Api-Key" || v != "raw-token" {
		t.Errorf("got (%q, %q, %v), want the bare token in X-Api-Key", h, v, err)
	}
}
