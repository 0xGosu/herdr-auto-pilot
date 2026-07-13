package embedder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// Environment variables the parent Client sets on the `hap embed-worker`
// child. The worker takes its whole configuration from the environment (not
// argv) so the same spawn mechanism works for the real binary and for the
// test-binary re-exec used by the Client tests.
const (
	// EnvWorkerModel is the gguf path the worker loads (required).
	EnvWorkerModel = "HAP_EMBED_MODEL"
	// EnvWorkerGPULayers is the GPU offload layer count (optional, default 0).
	EnvWorkerGPULayers = "HAP_EMBED_GPU_LAYERS"
	// EnvWorkerCrash is a TEST/FAULT-INJECTION seam: set to N and the worker
	// os.Exit(134)s (mimicking a SIGABRT) when it receives its N-th embed
	// request, before responding. It exists so the isolation contract — a
	// native worker abort surfaces as a Go error on the parent, degrading
	// instead of blocking — can be exercised on platforms where the real
	// llama.cpp GGML_ASSERT (arm64 macOS, #60) cannot be triggered. Never set
	// in production.
	EnvWorkerCrash = "HAP_EMBED_WORKER_CRASH"
)

// crashExitCode mirrors the shell convention for a process killed by SIGABRT
// (128 + SIGABRT(6)), so the injected fault looks like the real native abort.
const crashExitCode = 134

// workerConfigFromEnv reads the worker's embedding config from the environment
// set by the parent Client.
func workerConfigFromEnv() (config.Embedding, error) {
	model := os.Getenv(EnvWorkerModel)
	if model == "" {
		return config.Embedding{}, fmt.Errorf("%s is not set", EnvWorkerModel)
	}
	cfg := config.Embedding{ModelPath: model}
	if v := os.Getenv(EnvWorkerGPULayers); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return config.Embedding{}, fmt.Errorf("invalid %s=%q: %w", EnvWorkerGPULayers, v, err)
		}
		cfg.GPULayers = n
	}
	return cfg, nil
}

// RunWorker is the body of the `hap embed-worker` subcommand. It loads the
// in-process CGO engine once and services length-prefixed embed requests from
// in, writing framed responses to out, until in reaches EOF or ctx is done.
//
// It is deliberately dumb: it holds no failure latch and never retries. If a
// request embeds cleanly it returns the vector; if the engine errors it returns
// an error frame; if llama.cpp aborts natively the process simply dies and the
// parent observes the closed pipe. All supervision — timeouts, the
// degrade-after-N latch, restarts — lives in the parent Client.
func RunWorker(ctx context.Context, in io.Reader, out io.Writer) error {
	cfg, err := workerConfigFromEnv()
	if err != nil {
		return err
	}
	crashAt := 0
	if v := os.Getenv(EnvWorkerCrash); v != "" {
		crashAt, _ = strconv.Atoi(v)
	}

	eng := NewEngine(cfg)
	defer eng.Close()

	// bufio around stdout so each response is a single flushed write; requests
	// are read straight from in (io.ReadFull needs no buffering).
	bw := bufio.NewWriter(out)

	requests := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		text, err := readRequest(in)
		if err != nil {
			if err == io.EOF {
				return nil // parent closed the pipe: clean shutdown
			}
			return err
		}
		requests++
		if crashAt > 0 && requests >= crashAt {
			// Fault-injection seam: die before responding so THIS request errors
			// on the parent, mimicking a mid-embed native abort.
			os.Exit(crashExitCode)
		}

		vec, embErr := eng.EmbedText(ctx, text)
		if embErr != nil {
			if werr := writeErrResponse(bw, embErr.Error()); werr != nil {
				return werr
			}
		} else if werr := writeVecResponse(bw, vec); werr != nil {
			return werr
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
}
