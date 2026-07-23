package llm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vars.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
		wantBad []int
	}{
		{"plain", "FOO=bar\n", []string{"FOO=bar"}, nil},
		{"export prefix", "export FOO=bar\n", []string{"FOO=bar"}, nil},
		{"blank and comment lines", "\n# a comment\n\nFOO=bar\n  # indented comment\n", []string{"FOO=bar"}, nil},
		{"surrounding whitespace", "  FOO =  bar  \n", []string{"FOO=bar"}, nil},
		{"empty value", "FOO=\n", []string{"FOO="}, nil},
		{"value with equals", "URL=https://x/?a=b\n", []string{"URL=https://x/?a=b"}, nil},
		{"double quotes keep spaces", "FOO=\"  bar baz \"\n", []string{"FOO=  bar baz "}, nil},
		{"double quote escapes", `FOO="a\nb\tc\"d\\e"` + "\n", []string{"FOO=a\nb\tc\"d\\e"}, nil},
		{"single quotes are literal", `FOO='a\nb # c'` + "\n", []string{`FOO=a\nb # c`}, nil},
		{"inline comment stripped", "FOO=bar # trailing\n", []string{"FOO=bar"}, nil},
		{"hash without space kept", "FOO=pass#1\n", []string{"FOO=pass#1"}, nil},
		{"crlf line endings", "FOO=bar\r\nBAZ=qux\r\n", []string{"FOO=bar", "BAZ=qux"}, nil},
		{"utf-8 bom", "\ufeffFOO=bar\n", []string{"FOO=bar"}, nil},
		{"file order preserved", "B=2\nA=1\n", []string{"B=2", "A=1"}, nil},
		{"malformed lines reported", "no equals here\n1BAD=x\nBAD KEY=x\n=novalue\nFOO=bar\n", []string{"FOO=bar"}, []int{1, 2, 3, 4}},
		{"unterminated quote is malformed", "FOO=\"-----BEGIN KEY-----\nBAR=ok\n", []string{"BAR=ok"}, []int{1}},
		{"lone quote is malformed", "FOO='\n", nil, []int{1}},
		{"underscore and digits in key", "_A1_B=v\n", []string{"_A1_B=v"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, malformed := parseEnvFile(tc.content)
			if !slices.Equal(got, tc.want) {
				t.Errorf("parseEnvFile = %q, want %q", got, tc.want)
			}
			if !slices.Equal(malformed, tc.wantBad) {
				t.Errorf("malformed lines = %v, want %v", malformed, tc.wantBad)
			}
		})
	}
}

func TestLoadEnvFileErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		// A configured-but-missing file must fail the run: silently launching
		// the CLI without its credentials is the worse outcome.
		if _, err := LoadEnvFile(filepath.Join(dir, "nope.env")); err == nil {
			t.Fatal("missing env file must error")
		}
	})

	t.Run("directory", func(t *testing.T) {
		if _, err := LoadEnvFile(dir); err == nil {
			t.Fatal("directory env file must error")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		big := filepath.Join(dir, "big.env")
		if err := os.WriteFile(big, []byte(strings.Repeat("A", maxEnvFileBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadEnvFile(big)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("oversized env file error = %v, want a size complaint", err)
		}
	})
}

func TestLoadEnvFileExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "hap.env"), []byte("FOO=bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnvFile("~/hap.env")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"FOO=bar"}) {
		t.Errorf("entries = %q, want FOO=bar", got)
	}
}

// lookupEnv finds a key in a composed environment slice.
func lookupEnv(t *testing.T, env []string, key string) (string, bool) {
	t.Helper()
	var value string
	var found bool
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			if found {
				t.Fatalf("key %s appears more than once in the composed environment", key)
			}
			value, found = v, true
		}
	}
	return value, found
}

func TestBuildEnvPrecedence(t *testing.T) {
	t.Setenv("LLMENV_INHERITED", "from-os")
	t.Setenv("LLMENV_LAYERED", "from-os")

	baseFile := writeEnvFile(t, "LLMENV_LAYERED=base-file\nBASE_ONLY=base-file\n")
	cmdFile := writeEnvFile(t, "LLMENV_LAYERED=cmd-file\nCMD_ONLY=cmd-file\n")

	env, err := buildEnv(
		EnvSpec{File: baseFile, Vars: map[string]string{"LLMENV_LAYERED": "base-vars"}},
		EnvSpec{File: cmdFile, Vars: map[string]string{"LLMENV_LAYERED": "cmd-vars"}},
		nil,
		"HAP_REQUEST_ID=req-1",
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ key, want string }{
		{"LLMENV_INHERITED", "from-os"}, // untouched os environment survives
		{"BASE_ONLY", "base-file"},
		{"CMD_ONLY", "cmd-file"},
		{"LLMENV_LAYERED", "cmd-vars"}, // os < base file < base vars < cmd file < cmd vars
		{"HAP_REQUEST_ID", "req-1"},
	} {
		got, ok := lookupEnv(t, env, tc.key)
		if !ok {
			t.Errorf("%s missing from composed environment", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestBuildEnvInjectedVarsWinOverOperatorEnv(t *testing.T) {
	// The HAP_* wiring is hap's own protocol with its MCP server; an operator
	// env must not be able to redirect it.
	env, err := buildEnv(
		EnvSpec{Vars: map[string]string{"HAP_REQUEST_ID": "spoofed"}},
		EnvSpec{Vars: map[string]string{"HAP_DB_PATH": "/tmp/spoofed.db"}},
		nil,
		"HAP_REQUEST_ID=req-1", "HAP_DB_PATH=/state/hap.db",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := lookupEnv(t, env, "HAP_REQUEST_ID"); got != "req-1" {
		t.Errorf("HAP_REQUEST_ID = %q, want the injected req-1", got)
	}
	if got, _ := lookupEnv(t, env, "HAP_DB_PATH"); got != "/state/hap.db" {
		t.Errorf("HAP_DB_PATH = %q, want the injected path", got)
	}
}

func TestBuildEnvSubstitutesPlaceholders(t *testing.T) {
	file := writeEnvFile(t, "FROM_FILE=agent-{agent_name}\n")
	repl := strings.NewReplacer("{agent_name}", "brave-otter", "{request_id}", "req-9")
	env, err := buildEnv(
		EnvSpec{},
		EnvSpec{File: file, Vars: map[string]string{"FROM_VARS": "req-{request_id}"}},
		repl,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := lookupEnv(t, env, "FROM_FILE"); got != "agent-brave-otter" {
		t.Errorf("FROM_FILE = %q, want the expanded agent name", got)
	}
	if got, _ := lookupEnv(t, env, "FROM_VARS"); got != "req-req-9" {
		t.Errorf("FROM_VARS = %q, want the expanded request id", got)
	}
}

func TestBuildEnvPropagatesFileError(t *testing.T) {
	_, err := buildEnv(EnvSpec{}, EnvSpec{File: filepath.Join(t.TempDir(), "absent.env")}, nil)
	if err == nil {
		t.Fatal("an unreadable env file must fail the run, not be skipped")
	}
}

func TestEnvSpecIsEmpty(t *testing.T) {
	if !(EnvSpec{}).IsEmpty() {
		t.Error("zero spec must be empty")
	}
	if !(EnvSpec{File: "  "}).IsEmpty() {
		t.Error("blank file path must be empty")
	}
	if (EnvSpec{Vars: map[string]string{"A": "b"}}).IsEmpty() {
		t.Error("spec with vars must not be empty")
	}
	if (EnvSpec{File: "/x.env"}).IsEmpty() {
		t.Error("spec with a file must not be empty")
	}
}

func TestBuildEnvRejectsReservedKeys(t *testing.T) {
	// HAP_*/HERDR_* name hap's own wiring: the MCP handshake, the plugin
	// state/config dirs, the herdr binary. An operator env that redefined
	// them would point a nested hap/herdr at another installation.
	file := writeEnvFile(t, "HERDR_BIN_PATH=/tmp/fake-herdr\nKEEP=yes\n")
	env, err := buildEnv(
		EnvSpec{Vars: map[string]string{"HAP_DB_PATH": "/tmp/spoof.db"}},
		EnvSpec{File: file},
		nil,
		"HAP_DB_PATH=/state/hap.db",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := lookupEnv(t, env, "HAP_DB_PATH"); got != "/state/hap.db" {
		t.Errorf("HAP_DB_PATH = %q, want the injected path", got)
	}
	if got, ok := lookupEnv(t, env, "HERDR_BIN_PATH"); ok && got == "/tmp/fake-herdr" {
		t.Error("an env file must not be able to redirect HERDR_BIN_PATH")
	}
	if got, _ := lookupEnv(t, env, "KEEP"); got != "yes" {
		t.Errorf("KEEP = %q, want the file's other variables to survive", got)
	}
}

func TestLoadEnvFileWithNoVariablesErrors(t *testing.T) {
	// A comments-only or empty file is a misconfiguration: launching the CLI
	// with no credentials would surface as an opaque auth failure later.
	path := writeEnvFile(t, "# just a comment\n\n")
	if _, err := LoadEnvFile(path); err == nil {
		t.Fatal("an env file defining nothing must fail the run")
	}
}

func TestLoadEnvFileErrorsNeverEchoContent(t *testing.T) {
	// The message may name the file and the line; the line's TEXT is exactly
	// where a secret lives, so it must never appear.
	path := writeEnvFile(t, "API_KEY sk-ant-supersecret\n")
	_, err := LoadEnvFile(path)
	if err == nil {
		t.Fatal("a malformed line must fail the run")
	}
	if strings.Contains(err.Error(), "sk-ant-supersecret") {
		t.Errorf("error echoed the line content: %v", err)
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error should name the line number, got: %v", err)
	}
}
