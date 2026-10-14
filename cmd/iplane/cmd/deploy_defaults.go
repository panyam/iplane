package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// defaultDeployTimeout is how long `deployment deploy --wait` waits for a
// terminal state. It was 8 minutes, which a small model clears and a 72B
// does not: its 145 GB download alone runs 20 minutes on a good host. The
// deploy returns as soon as the engine serves or fails, so a long ceiling
// costs nothing on the deploys that finish early.
const defaultDeployTimeout = 45 * time.Minute

// deployTimeoutError rewrites a deploy that ran out of --timeout into a
// message naming the flag. The bare form was "context deadline exceeded",
// which says nothing about which of the three deadlines on the deploy path
// (this one, the daemon's write timeout, the provider's engine-ready wait)
// was the one that fired.
func deployTimeoutError(id string, timeout time.Duration, remote bool, ctxErr, err error) error {
	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("deploy %q: %w", id, err)
	}
	next := fmt.Sprintf("This process was hosting the deploy and exits with it, so a machine may already be rented: release it with `iplane deployment destroy %s`.", id)
	if remote {
		next = fmt.Sprintf("The daemon keeps going: `iplane deployment watch %s` follows it.", id)
	}
	return fmt.Errorf("deploy %q: no terminal state within --timeout %s; a large model's cold start can take longer, so raise --timeout. %s (%w)",
		id, timeout, next, err)
}
