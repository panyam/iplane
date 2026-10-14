package vast

import (
	"fmt"
	"strings"
)

// vllmAPIServer starts vLLM's OpenAI server by module path. It is the one
// spelling every vllm/vllm-openai release accepts: the image's own
// ENTRYPOINT moved from this to `vllm serve` between releases, but the
// module stayed, and it takes the `--model` flag the onstart script passes.
// The GLM-5.2 and Ch 10 runs on v0.27.1 both started this way.
var vllmAPIServer = []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}

// EngineCommand returns the command Vast's launcher should exec to start the
// engine: the deployment's own engine_entrypoint when it names one, the vLLM
// API server for a vllm/vllm-openai image when it does not, and an error
// otherwise.
//
// Vast never runs the image's entrypoint (see Deploy), so on this provider a
// missing command is not "use the image's default", it is "start nothing".
// For the vLLM image the right answer is known; for anything else guessing
// would rent a box that runs sshd and nothing else, so it refuses before the
// rent.
func EngineCommand(image string, explicit []string) ([]string, error) {
	if len(explicit) > 0 {
		return explicit, nil
	}
	if isVLLMOpenAIImage(image) {
		return append([]string(nil), vllmAPIServer...), nil
	}
	return nil, fmt.Errorf(
		"deployment.engine_entrypoint is required on vast for image %q: the provider replaces the image's "+
			"entrypoint with its own launcher, so the engine's start command has to be supplied "+
			"(iplane fills it in only for vllm/vllm-openai images; e.g. --engine-entrypoint python3 "+
			"--engine-entrypoint=-m --engine-entrypoint vllm.entrypoints.openai.api_server)", image)
}

// isVLLMOpenAIImage reports whether image is vllm/vllm-openai at any tag or
// digest, with or without Docker Hub's registry prefix.
func isVLLMOpenAIImage(image string) bool {
	repo := image
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	}
	// A tag's colon comes after the last slash; a registry port's does not.
	if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
		repo = repo[:i]
	}
	for _, p := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		repo = strings.TrimPrefix(repo, p)
	}
	return repo == "vllm/vllm-openai"
}
