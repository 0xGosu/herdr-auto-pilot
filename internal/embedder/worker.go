package embedder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// Environment variables the parent Client sets on the `hap embed-worker`
// child. The worker takes its whole configuration from the environment (not
// argv) so the same spawn mechanism works for the real binary and for the
// test-binary re-exec used by the Client tests.
const (
	// EnvWorkerModel is the gguf path the worker loads (required).
	EnvWorkerModel = "HAP_EMBED_MODEL"
	// EnvWorkerContextWindow overrides the model's context window / token cap
	// (optional; empty or "0" → DefaultContextWindow).
	EnvWorkerContextWindow = "HAP_EMBED_CONTEXT_WINDOW"
	// EnvWorkerEmbedTimeout / EnvWorkerWarmTimeout forward the parent's stall
	// guards in milliseconds (optional; empty or "0" → the built-in defaults).
	// They MUST travel to the child: the worker runs the same engine, with the
	// same guards, so a child still holding the 2s default would time out a
	// slow model before the parent's raised budget ever expired — making
	// embedding.embed_timeout_ms look like it did nothing.
	EnvWorkerEmbedTimeout = "HAP_EMBED_TIMEOUT_MS"
	EnvWorkerWarmTimeout  = "HAP_EMBED_WARM_TIMEOUT_MS"
	// EnvWorkerMaxFailures forwards the degrade-latch threshold (optional;
	// empty or "0" → the built-in default).
	EnvWorkerMaxFailures = "HAP_EMBED_MAX_FAILURES"
	// EnvWorkerCrash is a TEST/FAULT-INJECTION seam: set to N and the worker
	// os.Exit(134)s (mimicking a SIGABRT) when it receives its N-th embed
	// request, before responding. It exists so the isolation contract — a
	// native worker abort surfaces as a Go error on the parent, degrading
	// instead of blocking — can be exercised on platforms where the real
	// llama.cpp GGML_ASSERT (arm64 macOS, #60) cannot be triggered. Never set
	// in production.
	EnvWorkerCrash = "HAP_EMBED_WORKER_CRASH"
	// EnvWorkerStall is the sibling TEST/FAULT-INJECTION seam for the STALL
	// path: set to N and the worker blocks forever on its N-th embed request
	// instead of answering, so the parent's stall guard is what ends the call.
	// It exists because a hung native embed — the case the guards and the
	// configurable timeouts are for — cannot otherwise be produced on demand.
	// Never set in production.
	EnvWorkerStall = "HAP_EMBED_WORKER_STALL"
)

// crashExitCode mirrors the shell convention for a process killed by SIGABRT
// (128 + SIGABRT(6)), so the injected fault looks like the real native abort.
const crashExitCode = 134

// stallBackstop bounds the EnvWorkerStall fault injection so an orphaned test
// worker cannot outlive its harness.
const stallBackstop = time.Minute

// workerConfigFromEnv reads the worker's embedding config from the environment
// set by the parent Client.
func workerConfigFromEnv() (config.Embedding, error) {
	model := os.Getenv(EnvWorkerModel)
	if model == "" {
		return config.Embedding{}, fmt.Errorf("%s is not set", EnvWorkerModel)
	}
	cfg := config.Embedding{ModelPath: model}
	for _, f := range []struct {
		env string
		dst *int
	}{
		{EnvWorkerContextWindow, &cfg.ModelContextWindow},
		{EnvWorkerEmbedTimeout, &cfg.EmbedTimeoutMs},
		{EnvWorkerWarmTimeout, &cfg.WarmTimeoutMs},
		{EnvWorkerMaxFailures, &cfg.MaxConsecutiveFailures},
	} {
		v := os.Getenv(f.env)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return config.Embedding{}, fmt.Errorf("invalid %s=%q: %w", f.env, v, err)
		}
		*f.dst = n
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
	stallAt := 0
	if v := os.Getenv(EnvWorkerStall); v != "" {
		stallAt, _ = strconv.Atoi(v)
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
		if stallAt > 0 && requests >= stallAt {
			// Fault-injection seam: never answer. The parent's stall guard is
			// the only thing that should end this call — which is the point.
			// The stallBackstop keeps an orphaned worker from living forever,
			// and a plain `<-ctx.Done()` would trip Go's deadlock detector
			// when ctx is Background (the re-exec worker's ctx).
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(stallBackstop):
				return fmt.Errorf("stall fault injection expired after %s", stallBackstop)
			}
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
