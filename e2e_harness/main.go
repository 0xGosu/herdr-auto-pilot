// Command e2e_harness drives one end-to-end consult against a fake herdr:
// it stands up the fake events socket and fake herdr CLI, spawns the real
// hap daemon FROM A DELETED WORKING DIRECTORY (reproducing the operator's
// environment), pushes an approval situation, and waits for the audit
// trail to show the LLM consult outcome. Temporary tool — not shipped.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/fakeherdr"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := os.Args[1]      // short dir for the unix socket
	hapBin := os.Args[2]   // built hap binary
	cfgDir := os.Args[3]   // config dir holding the operator's config.toml
	stateDir := os.Args[4] // state dir

	srv, err := fakeherdr.NewServer(dir)
	if err != nil {
		return err
	}
	defer srv.Close()

	cli, err := fakeherdr.NewFakeCLI(dir)
	if err != nil {
		return err
	}
	if err := cli.SetPaneContent("Bash command: ls docs/\nDo you want to proceed? (y/n)"); err != nil {
		return err
	}

	// Reproduce the reported environment: the daemon's cwd is deleted
	// after launch (herdr started it from a since-removed workspace).
	dead := filepath.Join(dir, "dead-cwd")
	if err := os.Mkdir(dead, 0o700); err != nil {
		return err
	}
	daemon := exec.Command(hapBin, "daemon")
	daemon.Dir = dead
	daemon.Env = append(os.Environ(),
		"HERDR_PLUGIN_CONFIG_DIR="+cfgDir,
		"HERDR_PLUGIN_STATE_DIR="+stateDir,
		"HERDR_SOCKET_PATH="+srv.SocketPath,
		"HERDR_BIN_PATH="+cli.BinPath,
		"HAP_DEBUG=1",
	)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		return err
	}
	defer daemon.Process.Kill()
	time.Sleep(300 * time.Millisecond) // let it chdir-inherit before deletion
	if err := os.Remove(dead); err != nil {
		return err
	}

	time.Sleep(2 * time.Second) // subscriber connect
	srv.AddPane("pane-1", "ws-1")
	srv.PushAgentDetected("pane-1", "ws-1", "claude")
	// Adding a pane makes the subscriber reconnect ("pane set changed",
	// 1s backoff); wait past the resubscribe before the transition.
	time.Sleep(4 * time.Second)
	srv.PushTransition("pane-1", "ws-1", "claude", "blocked")
	fmt.Println("harness: approval situation pushed; waiting for consult outcome...")

	// The real claude CLI needs time to start, call get_context and
	// submit_decision through the hap MCP server.
	deadline := time.Now().Add(150 * time.Second)
	for tick := 0; time.Now().Before(deadline); tick++ {
		time.Sleep(3 * time.Second)
		cmd := exec.Command(hapBin, "audit")
		cmd.Env = daemon.Env
		out, _ := cmd.CombinedOutput()
		if tick == 9 && strings.TrimSpace(string(out)) == "" {
			// Transition lost to another reconnect window: push once more.
			fmt.Println("harness: no audit yet; re-pushing transition")
			srv.PushTransition("pane-1", "ws-1", "claude", "blocked")
		}
		if containsConsultOutcome(string(out)) {
			fmt.Println("=== hap audit ===")
			fmt.Println(string(out))
			return nil
		}
	}
	cmd := exec.Command(hapBin, "audit")
	cmd.Env = daemon.Env
	out, _ := cmd.CombinedOutput()
	fmt.Println("=== hap audit (timeout) ===")
	fmt.Println(string(out))
	return fmt.Errorf("no consult outcome within deadline")
}

func containsConsultOutcome(audit string) bool {
	for _, marker := range []string{"LLM suggested", "llm_no_submit", "llm_timeout", "LLM fallback"} {
		if strings.Contains(audit, marker) {
			return true
		}
	}
	return false
}
