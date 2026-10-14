package runpod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSSHKeys plays GET/PUT /v2/account/ssh-keys: it holds the account's
// key set, answers GET with it, and replaces it wholesale on PUT the way
// RunPod does. Every PUT body is recorded so tests can assert on exactly
// what was written.
type fakeSSHKeys struct {
	keys    []string
	status  int        // non-200 answers every request with this status
	puts    [][]string // the keys of each PUT, in order
	methods []string
	rawPuts []string
}

func newFakeSSHKeys(t *testing.T, keys ...string) (*fakeSSHKeys, *Provider) {
	t.Helper()
	f := &fakeSSHKeys{keys: keys, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/account/ssh-keys" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing bearer auth")
		}
		f.methods = append(f.methods, r.Method)
		if f.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(problem(f.status, http.StatusText(f.status), "access denied")))
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var raw json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&raw)
			f.rawPuts = append(f.rawPuts, string(raw))
			var body sshKeysBody
			_ = json.Unmarshal(raw, &body)
			f.puts = append(f.puts, body.Keys)
			f.keys = body.Keys
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sshKeysBody{Keys: f.keys})
	}))
	t.Cleanup(srv.Close)
	return f, New(NewClient("test-api-key", WithBaseURL(srv.URL+"/v2"), WithHTTPClient(srv.Client())))
}

const iplaneKey1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1qOJSfqQS1J0CKvqg7n0X2Kvz5kZbAVfAHvKvYrPLB iplane-default-runpod-2026-05-20T15:30:00Z"
const iplaneKey2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1qOJSfqQS1J0CKvqg7n0X2Kvz5kZbAVfAHvKvYrPLC iplane-default-runpod-2026-05-20T15:30:00Z"
const userKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBQQbcAVcAVcAVcAVcAVcAVcAVcAVcAVcAVcAVcAVcA user@laptop"
const iplaneComment = "iplane-default-runpod-2026-05-20T15:30:00Z"

func TestEnsurePublicKey_UploadsWhenAbsent(t *testing.T) {
	f, p := newFakeSSHKeys(t) // empty account
	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1+"\n"), iplaneComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("PUT called %d times, want 1", len(f.puts))
	}
	// One element per key, no trailing newline: the PUT schema validates
	// each entry as a single authorized_keys line.
	if len(f.puts[0]) != 1 || f.puts[0][0] != iplaneKey1 {
		t.Errorf("PUT keys = %q, want exactly [%q]", f.puts[0], iplaneKey1)
	}
}

func TestEnsurePublicKey_SkipsWhenAlreadyPresent(t *testing.T) {
	f, p := newFakeSSHKeys(t, iplaneKey1)
	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), iplaneComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.puts) != 0 {
		t.Errorf("PUT called %d times, want 0 (key already present)", len(f.puts))
	}
}

func TestEnsurePublicKey_PreservesOtherKeys(t *testing.T) {
	f, p := newFakeSSHKeys(t, userKey) // user's own pre-existing key
	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), iplaneComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("PUT called %d times, want 1", len(f.puts))
	}
	// PUT replaces the whole set, so dropping the user's key here would
	// delete it from their account.
	got := strings.Join(f.puts[0], "\n")
	if !strings.Contains(got, "user@laptop") {
		t.Errorf("PUT dropped the user's existing key: %q", f.puts[0])
	}
	if !strings.Contains(got, iplaneComment) {
		t.Errorf("PUT missing the iplane key: %q", f.puts[0])
	}
}

func TestEnsurePublicKey_ReplacesStaleIplaneKey(t *testing.T) {
	// Old iplane key with the same comment-prefix but DIFFERENT bytes
	// (e.g. operator wiped the keystore and a new keypair was
	// generated). EnsurePublicKey PRUNES the stale iplane entry and
	// writes only the current key, so the set does not grow unbounded
	// across keystore regenerations.
	f, p := newFakeSSHKeys(t, iplaneKey2, userKey)
	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), iplaneComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("PUT called %d times, want 1", len(f.puts))
	}
	written := strings.Join(f.puts[0], "\n")
	if !strings.Contains(written, iplaneKey1) {
		t.Errorf("written set missing the current iplane key:\n%s", written)
	}
	if strings.Contains(written, iplaneKey2) {
		t.Errorf("stale iplane key was NOT pruned:\n%s", written)
	}
	if !strings.Contains(written, userKey) {
		t.Errorf("operator's own key was NOT preserved:\n%s", written)
	}
}

func TestEnsurePublicKey_PrunesEvenWhenCurrentKeyAlreadyPresent(t *testing.T) {
	// Current key IS registered, but a stale iplane entry is too (the set
	// accumulated across earlier keystores). EnsurePublicKey should still
	// rewrite to prune the stale one.
	f, p := newFakeSSHKeys(t, iplaneKey2, iplaneKey1, userKey)
	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), iplaneComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("expected 1 PUT to prune stale entries; got %d", len(f.puts))
	}
	if got := strings.Join(f.puts[0], "\n"); strings.Contains(got, iplaneKey2) {
		t.Errorf("stale iplane key was NOT pruned:\n%s", got)
	}
	if len(f.puts[0]) != 2 {
		t.Errorf("PUT keys = %q, want the current iplane key and the user's", f.puts[0])
	}
}

// A GET, a PUT, then a GET has to read back what was written, and the PUT
// body is exactly {"keys": [...]}: UpdateSshKeysRequest is a closed schema.
func TestSSHKeys_RoundTrip(t *testing.T) {
	f, p := newFakeSSHKeys(t, userKey)
	ctx := context.Background()

	if err := p.EnsurePublicKey(ctx, []byte(iplaneKey1), iplaneComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if strings.Join(f.methods, ",") != "GET,PUT" {
		t.Errorf("methods = %v, want GET then PUT", f.methods)
	}
	var put map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.rawPuts[0]), &put); err != nil || len(put) != 1 || put["keys"] == nil {
		t.Errorf("PUT body = %s, want exactly {\"keys\": [...]}", f.rawPuts[0])
	}

	blob, err := p.FetchUserPubKey(ctx)
	if err != nil {
		t.Fatalf("FetchUserPubKey: %v", err)
	}
	if blob != userKey+"\n"+iplaneKey1+"\n" {
		t.Errorf("read back %q, want the user's key then the iplane key, one per line", blob)
	}

	// And the next ensure is a no-op against what the PUT left.
	if err := p.EnsurePublicKey(ctx, []byte(iplaneKey1), iplaneComment); err != nil {
		t.Fatalf("second EnsurePublicKey: %v", err)
	}
	if len(f.puts) != 1 {
		t.Errorf("PUTs = %d after a second ensure, want still 1", len(f.puts))
	}
}

func TestFetchUserPubKey_EmptyAccount(t *testing.T) {
	_, p := newFakeSSHKeys(t)
	blob, err := p.FetchUserPubKey(context.Background())
	if err != nil {
		t.Fatalf("FetchUserPubKey: %v", err)
	}
	if blob != "" {
		t.Errorf("blob = %q, want empty for an account with no keys", blob)
	}
}

func TestEnsurePublicKey_403NamesTheScope(t *testing.T) {
	f, p := newFakeSSHKeys(t)
	f.status = http.StatusForbidden

	err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), iplaneComment)
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "authentication:read") {
		t.Errorf("error %q should name the scope the key lacks", err)
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error %q lost RunPod's detail", err)
	}
}
