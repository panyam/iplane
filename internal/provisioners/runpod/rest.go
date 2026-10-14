package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// DefaultBaseURL is RunPod's REST API v2. Every call this adapter makes
// goes here: pods, pod logs, the GPU and CPU catalogs, account SSH keys and
// network volumes. Overrideable via WithBaseURL (httptest in unit tests).
//
// Until 2026-10 the adapter spoke three surfaces: REST v1 at
// rest.runpod.io/v1 for pods and volumes, GraphQL at api.runpod.io/graphql
// for the catalog and SSH keys, and v2 for logs and the pod `runtime`
// block. RunPod retires REST v1 on 2026-11-15 and GraphQL in early 2027,
// and v2 covers everything the other two did, so it is the only one left.
const DefaultBaseURL = "https://api.runpod.io/v2"

// Client carries the bits every RunPod request needs (base URL, bearer
// token) plus an optional custom *http.Client for tests. The actual
// HTTP send / read / decode lives in servicekit's http.Call / CallVoid.
// We keep this struct thin: build the request, hand it to servicekit.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client // when nil, servicekit's DefaultHttpClient is used
}

// ClientOption configures the Client at construction time.
type ClientOption func(*Client)

// WithHTTPClient swaps in a custom *http.Client. Tests pass an
// httptest.Server's client; production keeps the default.
func WithHTTPClient(c *http.Client) ClientOption {
	return func(cl *Client) { cl.httpClient = c }
}

// WithBaseURL points the client at a different endpoint, including the
// version segment (production is https://api.runpod.io/v2). Tests pass
// the httptest.Server URL plus "/v2" so fakes match the real paths.
func WithBaseURL(u string) ClientOption {
	return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") }
}

// NewClient builds a RunPod REST client. apiKey is the operator's
// RUNPOD_API_KEY, attached as a bearer token on every request.
//
// When IPLANE_RUNPOD_DEBUG is set, the underlying http.Client is wrapped
// with a debugTransport that prints the request/response bytes to stderr
// (sans Authorization header). The wrap is non-destructive -- we clone
// any caller-supplied *http.Client so re-use elsewhere keeps its
// original transport.
func NewClient(apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		apiKey:  apiKey,
	}
	for _, opt := range opts {
		opt(c)
	}
	if os.Getenv(EnvHTTPDebug) != "" {
		base := c.httpClient
		if base == nil {
			base = http.DefaultClient
		}
		cloned := *base
		cloned.Transport = newDebugTransport(cloned.Transport)
		c.httpClient = &cloned
	}
	return c
}

// newReq builds a JSON-shaped request with bearer auth attached. The
// caller passes it to servicekit's Call/CallVoid, which adds the
// context and performs the round trip.
func (c *Client) newReq(method, path string, query url.Values, body any) (*http.Request, error) {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}
	if len(query) > 0 {
		endpoint = endpoint + "?" + query.Encode()
	}
	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
	}
	req, err := skhttp.NewBytesRequest(method, endpoint, bodyBytes)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// podLogsURL builds the pod-logs stream URL. source=container drops
// RunPod's system log noise; tail backfills recent history so a marker
// printed before the poll started is still seen.
func (c *Client) podLogsURL(podID string) string {
	return fmt.Sprintf("%s/pods/%s/logs?source=container&tail=1000", c.baseURL, url.PathEscape(podID))
}

// callOpts returns the servicekit options to apply. Tests inject a
// custom *http.Client; production lets servicekit's secure default
// apply (TLS verification on).
func (c *Client) callOpts() []skhttp.CallOption {
	if c.httpClient != nil {
		return []skhttp.CallOption{skhttp.WithClient(c.httpClient)}
	}
	return nil
}

// pagination is v2's cursor block, carried by every list response.
type pagination struct {
	NextCursor  *string `json:"nextCursor"`
	HasNextPage bool    `json:"hasNextPage"`
}

// maxListPages bounds a paginated walk. Pages default to 1000 items, so
// this is a guard against a cursor that never terminates rather than a
// limit anyone should reach.
const maxListPages = 100

// listAll walks a cursor-paginated v2 list endpoint and returns every
// item. page extracts the items and the pagination block from one
// decoded response, since each endpoint wraps its list under its own key
// ({"pods": [...]}, {"networkVolumes": [...]}).
func listAll[R, T any](ctx context.Context, c *Client, op, path string, page func(R) ([]T, pagination)) ([]T, error) {
	q := url.Values{}
	var out []T
	for i := 0; i < maxListPages; i++ {
		req, err := c.newReq("GET", path, q, nil)
		if err != nil {
			return nil, wrapErr(op, err)
		}
		resp, err := skhttp.Call[R](ctx, req, c.callOpts()...)
		if err != nil {
			return nil, wrapErr(op, err)
		}
		items, pg := page(resp)
		out = append(out, items...)
		if !pg.HasNextPage || pg.NextCursor == nil || *pg.NextCursor == "" {
			return out, nil
		}
		q.Set("cursor", *pg.NextCursor)
	}
	return nil, wrapErr(op, fmt.Errorf("%s did not finish paginating after %d pages", path, maxListPages))
}

// wrapErr translates a servicekit HTTPError into a *provisioners.ProviderError
// with RunPod's problem+json body parsed. Preserves the detail text so the
// original message reaches the operator.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var httpErr *skhttp.HTTPError
	if errors.As(err, &httpErr) {
		msg := parseErrorMessage(httpErr.Body)
		cause := errors.New(msg)
		switch httpErr.Code {
		case http.StatusNotFound:
			cause = fmt.Errorf("%s: %w", msg, provisioners.ErrNotFound)
		case http.StatusUnauthorized, http.StatusForbidden:
			cause = fmt.Errorf("auth failed: %s", msg)
		}
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, cause, httpErr.Code)
	}
	return provisioners.NewProviderError(provisioners.ProviderRunPod, op, err, 0)
}

// problemBody is v2's error shape, RFC 9457 problem+json:
// {title, status, detail, errors[]}. `errors` lists individual
// validation failures on a 422 and is the actionable part when present.
//
// Error and Message are not part of v2's schema. They catch the bodies
// v1 used to send, in case anything upstream of the v2 handler (a proxy,
// a gateway) still answers in that shape.
type problemBody struct {
	Title   string   `json:"title"`
	Status  int      `json:"status"`
	Detail  string   `json:"detail"`
	Errors  []string `json:"errors"`
	Error   string   `json:"error"`
	Message string   `json:"message"`
}

// parseErrorMessage extracts the operator-facing message from a RunPod
// error body. detail first, then each validation error, so a 422 reads
// "Request validation failed.: $.gpu.id: ...". Falls back to title, then
// to the v1-era keys, then to the raw body. Errors are the operator's
// actionable signal -- never truncate them.
func parseErrorMessage(body []byte) string {
	var p problemBody
	if err := json.Unmarshal(body, &p); err == nil {
		var parts []string
		if p.Detail != "" {
			parts = append(parts, p.Detail)
		}
		if len(p.Errors) > 0 {
			parts = append(parts, strings.Join(p.Errors, "; "))
		}
		if len(parts) > 0 {
			return strings.Join(parts, ": ")
		}
		for _, s := range []string{p.Title, p.Error, p.Message} {
			if s != "" {
				return s
			}
		}
	}
	return string(body)
}

// httpStatus returns the HTTP status behind err, or 0 when the failure
// never reached a response (DNS, dial, timeout).
func httpStatus(err error) int {
	var httpErr *skhttp.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Code
	}
	var pe *provisioners.ProviderError
	if errors.As(err, &pe) {
		return pe.HTTP
	}
	return 0
}
