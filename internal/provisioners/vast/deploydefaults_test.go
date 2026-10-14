package vast

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEngineReadyTimeoutScalesWithTheDisk(t *testing.T) {
	p, _ := newTestProvider(t, http.NotFoundHandler())
	p.engineReadyTimeout = defaultEngineReadyTimeout
	p.engineReadyTimeoutSet = false

	cases := []struct {
		diskGB int
		want   time.Duration
	}{
		{0, defaultEngineReadyTimeout},
		{40, defaultEngineReadyTimeout},
		{200, 10*time.Minute + 2000*time.Second}, // a 72B at BF16: about 43m
		{570, 10*time.Minute + 5700*time.Second}, // GLM-5.2 AWQ: 105m
	}
	for _, c := range cases {
		if got := p.engineReadyTimeoutFor(c.diskGB); got != c.want {
			t.Errorf("engineReadyTimeoutFor(%d) = %s, want %s", c.diskGB, got, c.want)
		}
	}
}

// IPLANE_ENGINE_READY_TIMEOUT is the operator's word; it is not scaled.
func TestEngineReadyTimeoutTheOperatorSetIsNotScaled(t *testing.T) {
	p, _ := newTestProvider(t, http.NotFoundHandler())
	WithEngineReadyTimeout(defaultEngineReadyTimeout)(p)
	if got := p.engineReadyTimeoutFor(570); got != defaultEngineReadyTimeout {
		t.Errorf("explicit timeout scaled to %s", got)
	}
}

func TestEngineReadyDeadlineNamesTheSetting(t *testing.T) {
	if !strings.Contains(engineReadyTimeoutHint, "IPLANE_ENGINE_READY_TIMEOUT") ||
		!strings.Contains(engineReadyTimeoutHint, "--timeout") {
		t.Errorf("hint %q should name the env var and say --timeout does not reach it", engineReadyTimeoutHint)
	}
}

func TestDiskFullHint(t *testing.T) {
	full := errors.New("engine did not answer /health\n--- engine said ---\nOSError: [Errno 28] No space left on device")

	defaulted := withDiskFullHint(full, defaultDiskGB, false)
	for _, want := range []string{"40 GB", "--min-disk-gb", "could not report"} {
		if !strings.Contains(defaulted.Error(), want) {
			t.Errorf("defaulted-disk hint %q should contain %q", defaulted, want)
		}
	}
	sized := withDiskFullHint(full, 200, true)
	if !strings.Contains(sized.Error(), "raise --min-disk-gb above 200") {
		t.Errorf("sized-disk hint %q should say how far to raise it", sized)
	}
	if !errors.Is(sized, full) {
		t.Error("the engine's own error should stay wrapped")
	}

	other := errors.New("engine did not answer /health; CUDA out of memory")
	if got := withDiskFullHint(other, 40, false); got != other {
		t.Errorf("a failure that is not a full disk should pass through, got %v", got)
	}
}
