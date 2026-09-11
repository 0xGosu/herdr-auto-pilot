package domain

import (
	"fmt"
	"strings"
)

// Who a hap front-end process acts as. The value is written as the author of
// every record the process makes (corrections, queued agent actions, pause
// events, stream events) and as the actor on an escalation it settles, which is
// how the operator tells their own decisions from the orchestrator's.
const (
	// OperatorAuthor is the default: a human at the CLI or TUI.
	OperatorAuthor = "operator"
	// DaemonAuthor is hap's own daemon acting on its own behalf. Never
	// selectable through ActorEnv: a process claiming it would put an LLM's
	// decision under the daemon's name.
	DaemonAuthor = "daemon"
	// ActorEnv names the environment variable a front-end process reads to
	// learn who it acts as (see ResolveActor).
	ActorEnv = "HAP_ACTOR"
)

// ResolveActor decides the author of a front-end process from ActorEnv's value
// and whether the process runs inside the orchestrator's own pane.
//
// The environment can only PROMOTE a process to the orchestrator, never
// demote one. OrchestratorAuthor is not just a label: the daemon screens what
// that author sends and refuses it while the herd is paused, so the pane check
// wins over any value the environment carries — an orchestrator exporting
// HAP_ACTOR=operator must not escape its own gates.
//
// Only the two front-end actors are accepted. An arbitrary name would pass the
// orchestrator gates' exact-match compare and so be treated with an operator's
// trust, and "daemon" would put an LLM's decision under hap's own name. An
// unrecognized value is an error rather than a fallback: the likeliest cause is
// a typo in an orchestrator's setup, and silently acting as the operator is
// exactly the mislabelling the variable exists to prevent.
func ResolveActor(envValue string, inOrchestratorPane bool) (string, error) {
	actor := OperatorAuthor
	switch v := strings.ToLower(strings.TrimSpace(envValue)); v {
	case "", OperatorAuthor:
	case OrchestratorAuthor:
		actor = OrchestratorAuthor
	default:
		return "", fmt.Errorf("%s=%q is not a recognized actor: use %q (an orchestrating agent) or %q (the default)",
			ActorEnv, envValue, OrchestratorAuthor, OperatorAuthor)
	}
	if inOrchestratorPane {
		actor = OrchestratorAuthor
	}
	return actor, nil
}
