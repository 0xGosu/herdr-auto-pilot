package config

import "testing"

// TestSampleConfigParses guards the file operators are told to copy. It is
// mostly comments, but the [llm] recipes embed prompt text in TOML strings —
// an unescaped quote there makes the whole config unloadable, and nothing else
// in CI would notice before it shipped.
func TestSampleConfigParses(t *testing.T) {
	cfg, err := Load("../../sample/config.toml")
	if err != nil {
		t.Fatalf("sample/config.toml does not parse: %v", err)
	}
	if len(cfg.LLM.Command) == 0 {
		t.Error("the sample's active [llm].command recipe did not survive the parse")
	}
}
