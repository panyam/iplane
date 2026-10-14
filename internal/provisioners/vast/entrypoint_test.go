package vast

import (
	"reflect"
	"strings"
	"testing"
)

func TestEngineCommandDefaultsForTheVLLMImage(t *testing.T) {
	for _, image := range []string{
		"vllm/vllm-openai:v0.27.1",
		"vllm/vllm-openai",
		"docker.io/vllm/vllm-openai:v0.7.3",
		"vllm/vllm-openai@sha256:abc",
		"vllm/vllm-openai:v0.26.0-cu129",
	} {
		got, err := EngineCommand(image, nil)
		if err != nil {
			t.Errorf("EngineCommand(%q): %v", image, err)
			continue
		}
		if !reflect.DeepEqual(got, vllmAPIServer) {
			t.Errorf("EngineCommand(%q) = %q, want %q", image, got, vllmAPIServer)
		}
	}
}

func TestEngineCommandKeepsAnExplicitEntrypoint(t *testing.T) {
	want := []string{"vllm", "serve"}
	got, err := EngineCommand("vllm/vllm-openai:v0.27.1", want)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("EngineCommand = %q, %v; want %q", got, err, want)
	}
}

func TestEngineCommandRefusesAnImageItCannotStart(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/huggingface/text-generation-inference:3",
		"myorg/vllm-openai:latest",
		"vllm/vllm-openai-rocm:v0.27.1",
		"localhost:5000/vllm/vllm-openai:v1",
	} {
		_, err := EngineCommand(image, nil)
		if err == nil {
			t.Errorf("EngineCommand(%q) should refuse: nothing knows how to start it", image)
			continue
		}
		if !strings.Contains(err.Error(), "--engine-entrypoint") {
			t.Errorf("error %v should name the flag", err)
		}
	}
}
