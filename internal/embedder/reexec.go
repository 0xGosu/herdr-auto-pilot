package embedder

import (
	"context"
	"os"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// envWorkerHelper, when set on a test binary, makes RunWorkerHelperIfChild run
// the embed worker instead of the test suite. It lets a Client under test spawn
// the embed worker by re-execing the current test binary — so the subprocess
// isolation path is exercised without a real `hap` on PATH. Production main.go
// never consults it (the real binary uses the `embed-worker` subcommand).
const envWorkerHelper = "HAP_EMBED_WORKER_HELPER"

// RunWorkerHelperIfChild runs the embed worker and exits when the current
// process was spawned as an embed-worker re-exec (envWorkerHelper set);
// otherwise it returns and the caller proceeds normally. Call it first thing in
// a TestMain whose package constructs a Client via NewReexecClient.
func RunWorkerHelperIfChild() {
	if os.Getenv(envWorkerHelper) != "1" {
		return
	}
	// Config (model/gpu) arrives via the same environment the real worker reads.
	_ = RunWorker(context.Background(), os.Stdin, os.Stdout)
	os.Exit(0)
}

// NewReexecClient builds a Client whose worker is a re-exec of the current test
// binary (os.Args[0]) rather than the `hap` binary — the injectable worker seam
// the tests use. extraEnv is appended to the child environment (e.g.
// EnvWorkerCrash for fault injection). The package's TestMain must call
// RunWorkerHelperIfChild so the re-exec becomes a worker.
func NewReexecClient(cfg config.Embedding, extraEnv ...string) *Client {
	c := New(cfg)
	c.execPath = os.Args[0]
	c.execArgs = nil
	c.extraEnv = append([]string{envWorkerHelper + "=1"}, extraEnv...)
	return c
}
