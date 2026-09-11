package domain

import "testing"

func TestResolveActor(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		inPane  bool
		want    string
		wantErr bool
	}{
		{name: "unset is the operator", env: "", want: OperatorAuthor},
		{name: "explicit operator", env: "operator", want: OperatorAuthor},
		{name: "orchestrator", env: "orchestrator", want: OrchestratorAuthor},
		{name: "case and whitespace are forgiven", env: "  Orchestrator\n", want: OrchestratorAuthor},
		{name: "the orchestrator pane with no env", env: "", inPane: true, want: OrchestratorAuthor},
		// The environment may promote, never demote: the orchestrator's own
		// pane stays screened and pause-bound whatever it exports.
		{name: "the pane wins over an operator env", env: "operator", inPane: true, want: OrchestratorAuthor},
		// An arbitrary name would be trusted like the operator by the
		// orchestrator gates' exact-match compare, so it is refused.
		{name: "an arbitrary name", env: "my-bot", wantErr: true},
		{name: "the daemon is not selectable", env: "daemon", wantErr: true},
		{name: "a typo fails even in the orchestrator pane", env: "orchestrater", inPane: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveActor(tc.env, tc.inPane)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveActor(%q, %v) = %q, want an error", tc.env, tc.inPane, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveActor(%q, %v): %v", tc.env, tc.inPane, err)
			}
			if got != tc.want {
				t.Errorf("ResolveActor(%q, %v) = %q, want %q", tc.env, tc.inPane, got, tc.want)
			}
		})
	}
}
