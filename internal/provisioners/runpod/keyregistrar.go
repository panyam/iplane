package runpod

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/inference-book/inference-plane/internal/sshkeys"
	skhttp "github.com/panyam/servicekit/http"
)

// SSH key registration lives on REST v2's account endpoint:
//
//	GET /account/ssh-keys  -> {"keys": ["<type> <base64> [comment]", ...]}
//	PUT /account/ssh-keys  <- {"keys": [...]}, a complete replacement
//
// Until 2026-10 this was the GraphQL pair `myself { pubKey }` /
// `updateUserSettings(input: {pubKey})` over one newline-joined blob,
// because REST v1 had no SSH-key endpoints at all. v2 carries the same
// set as a list, with the same whole-set replace semantics, so the
// read-modify-write below is unchanged in shape.
//
// Keys reach a pod only when the create sets startSsh (Spawn always does;
// a deploy does under debug_shell). v1 injected them into every pod.

// sshKeysBody is both the GET response and the PUT request.
type sshKeysBody struct {
	Keys []string `json:"keys"`
}

// EnsurePublicKey satisfies provisioners.KeyRegistrar. The Service
// calls it before Spawn so the operator's iplane-generated SSH key
// is registered with RunPod's account and gets injected into
// newly-created pods via /root/.ssh/authorized_keys.
//
// The account's key set is replaced whole on every write. To stay
// idempotent: read the current set, skip-if-an-iplane-key-with-our-bytes
// is present, else add ours and PUT the full set back.
//
// Comment-based identification: each iplane key carries a stable
// comment string like "iplane-<operator>-<provider>-<rfc3339>". If
// a previous iplane invocation already left a key for this
// operator+provider scope, sshkeys.IsIplaneComment + an exact
// public-bytes match will recognize it and skip the upload.
//
// Scoped-key gotcha: the write needs the authentication:write scope. A
// 403 here gets wrapped with a message naming it.
func (p *Provider) EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error {
	current, err := p.fetchSSHKeys(ctx)
	if err != nil {
		return err
	}

	// Parse the desired key so we can compare structurally rather than
	// as text (catches whitespace + comment variations).
	wantParsed, _, _, _, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse own public key: %w", err)
	}
	wantMarshaled := ssh.MarshalAuthorizedKey(wantParsed)

	// Walk the existing set:
	//   - non-iplane keys are preserved verbatim (operator's own keys,
	//     runpodctl entries, anything the RunPod UI added).
	//   - iplane-tagged keys that match our exact bytes are KEPT and
	//     flagged haveMatch; we can short-circuit the write.
	//   - iplane-tagged keys with bytes that DON'T match (stale keys
	//     from prior runs with a wiped keystore) are dropped. A
	//     long-running operator account would otherwise accumulate
	//     iplane-* entries forever.
	//   - an entry that does not parse is preserved verbatim; it is
	//     the operator's, and silently deleting it is worse than
	//     carrying it.
	kept := make([]string, 0, len(current)+1)
	haveMatch := false
	prunedStale := false
	for _, line := range current {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parsed, gotComment, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(line))
		if parseErr != nil || !sshkeys.IsIplaneComment(gotComment) {
			kept = append(kept, line)
			continue
		}
		if bytes.Equal(ssh.MarshalAuthorizedKey(parsed), wantMarshaled) {
			kept = append(kept, line)
			haveMatch = true
			continue
		}
		prunedStale = true
	}

	// Fast path: our key is already registered AND nothing else needed
	// pruning. No write needed -- idempotent no-op.
	if haveMatch && !prunedStale {
		return nil
	}
	if !haveMatch {
		kept = append(kept, strings.TrimSpace(string(publicKey)))
	}
	return p.putSSHKeys(ctx, kept)
}

// FetchUserPubKey returns the account's registered SSH keys as one
// authorized_keys-style blob, a key per line, so diagnostic commands
// (iplane runpod debug-keys) can print it. Returns "" when none are
// registered.
func (p *Provider) FetchUserPubKey(ctx context.Context) (string, error) {
	keys, err := p.fetchSSHKeys(ctx)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", nil
	}
	return strings.Join(keys, "\n") + "\n", nil
}

// fetchSSHKeys reads GET /account/ssh-keys.
func (p *Provider) fetchSSHKeys(ctx context.Context) ([]string, error) {
	req, err := p.client.newReq("GET", "/account/ssh-keys", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch ssh keys: %w", err)
	}
	resp, err := skhttp.Call[sshKeysBody](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, sshKeysErr("fetch ssh keys", "authentication:read", err)
	}
	return resp.Keys, nil
}

// putSSHKeys replaces the account's whole key set via PUT
// /account/ssh-keys. Caller is responsible for read-modify-write
// semantics; anything left out of keys is removed from the account.
func (p *Provider) putSSHKeys(ctx context.Context, keys []string) error {
	if keys == nil {
		keys = []string{}
	}
	req, err := p.client.newReq("PUT", "/account/ssh-keys", nil, sshKeysBody{Keys: keys})
	if err != nil {
		return fmt.Errorf("update ssh keys: %w", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		return sshKeysErr("update ssh keys", "authentication:write", err)
	}
	return nil
}

// sshKeysErr wraps an ssh-keys failure, naming the scope the key lacks on
// a 401/403. RunPod's scoped keys grant per-resource scopes, and the
// account SSH keys sit under authentication:*, which a key scoped to pods
// alone does not carry.
func sshKeysErr(op, scope string, err error) error {
	wrapped := wrapErr(op, err)
	if s := httpStatus(err); s == 401 || s == 403 {
		return fmt.Errorf("RUNPOD_API_KEY needs the %s scope for SSH key registration: %w", scope, wrapped)
	}
	return wrapped
}
