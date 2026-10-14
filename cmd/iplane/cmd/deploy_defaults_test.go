package cmd

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The default image is the pinned engine, so the book's commands can leave
// --image off and still run the version the chapters were written against.
func TestDefaultEngineImageMatchesThePin(t *testing.T) {
	f, err := os.Open("../../../pinned-versions.env")
	if err != nil {
		t.Fatalf("open pinned-versions.env: %v", err)
	}
	defer f.Close()
	var version string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ENGINE_VERSION="); ok {
			version = strings.TrimSpace(v)
		}
	}
	if version == "" {
		t.Fatal("ENGINE_VERSION not found in pinned-versions.env")
	}
	if want := "vllm/vllm-openai:v" + version; defaultEngineImage != want {
		t.Errorf("defaultEngineImage = %q, want %q (pinned-versions.env ENGINE_VERSION)", defaultEngineImage, want)
	}
}

func TestDeployImageDefaultsToThePinnedEngine(t *testing.T) {
	f := deploymentDeployCmd.Flags().Lookup("image")
	if !strings.Contains(f.Usage, defaultEngineImage) {
		t.Errorf("--image usage %q should name the default %s", f.Usage, defaultEngineImage)
	}
	if up := upCmd.Flags().Lookup("image"); up.DefValue != defaultEngineImage {
		t.Errorf("up --image default = %q, want %q", up.DefValue, defaultEngineImage)
	}
}

// Eight minutes was shorter than a 72B's download. The book prints 45m.
func TestDeployTimeoutDefaultCoversALargeColdStart(t *testing.T) {
	f := deploymentDeployCmd.Flags().Lookup("timeout")
	d, err := time.ParseDuration(f.DefValue)
	if err != nil {
		t.Fatal(err)
	}
	if d < 45*time.Minute {
		t.Errorf("--timeout default = %s, want at least 45m", d)
	}
}

func TestDeployTimeoutErrorNamesTheFlag(t *testing.T) {
	cause := errors.New("rpc error: code = DeadlineExceeded desc = context deadline exceeded")

	inproc := deployTimeoutError("big", 45*time.Minute, false, context.DeadlineExceeded, cause)
	for _, want := range []string{"--timeout 45m0s", "raise --timeout", "iplane deployment destroy big"} {
		if !strings.Contains(inproc.Error(), want) {
			t.Errorf("in-process error %q should contain %q", inproc, want)
		}
	}
	remote := deployTimeoutError("big", time.Minute, true, context.DeadlineExceeded, cause)
	if !strings.Contains(remote.Error(), "iplane deployment watch big") {
		t.Errorf("remote error %q should say how to follow the deploy", remote)
	}
	if !errors.Is(remote, cause) {
		t.Error("the original error should stay wrapped")
	}

	other := deployTimeoutError("big", time.Minute, false, nil, errors.New("model spec: not found"))
	if strings.Contains(other.Error(), "--timeout") {
		t.Errorf("a non-timeout failure should not blame --timeout: %v", other)
	}
}
