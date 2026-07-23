package llm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// This file builds the environment each LLM CLI is spawned with. Every one of
// the four command templates (command, command_start, task_generate_command,
// task_generate_command_start) can carry its own variables, either inline in
// the config or in a `.env` file whose path the operator configures.
//
// Values are treated as secrets throughout: they are never logged, never
// echoed, and never included in an error message. Failures name the file and
// the line number only.

// maxEnvFileBytes caps a `.env` file. A credentials file is a few hundred
// bytes; anything larger is a misconfigured path (a log, a binary), and
// reading it into the child's environment would be both useless and unsafe.
const maxEnvFileBytes = 1 << 20

// EnvSpec is one layer of environment for a command: inline variables plus an
// optional `.env` file. The file is read at spawn time, so its contents never
// pass through the config file and an edit applies to the next run.
type EnvSpec struct {
	// Vars are inline KEY→VALUE pairs; they override the file.
	Vars map[string]string
	// File is an optional path to a `.env` file ("~/" is expanded). A
	// configured file that cannot be read fails the run.
	File string
}

// IsEmpty reports whether the spec contributes nothing.
func (s EnvSpec) IsEmpty() bool { return len(s.Vars) == 0 && strings.TrimSpace(s.File) == "" }

// entries renders the spec as ordered KEY=VALUE strings — file first, then the
// inline vars (sorted, so the result is deterministic) — with placeholders in
// the values expanded by repl. A nil repl leaves values verbatim.
func (s EnvSpec) entries(repl *strings.Replacer) ([]string, error) {
	var out []string
	if path := strings.TrimSpace(s.File); path != "" {
		fileEntries, err := LoadEnvFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, fileEntries...)
	}
	keys := make([]string, 0, len(s.Vars))
	for k := range s.Vars {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+s.Vars[k])
	}
	out = slices.DeleteFunc(out, func(e string) bool {
		key, _, _ := strings.Cut(e, "=")
		if !reservedEnvKey(key) {
			return false
		}
		// Naming the key is safe; its value is not. hap's own wiring (the
		// MCP handshake, the plugin's state/config dirs, the herdr binary)
		// must stay under hap's control, including for any nested `hap` or
		// `herdr` the CLI runs.
		slog.Warn("ignoring reserved variable in llm environment", "key", key)
		return true
	})
	if repl == nil {
		return out, nil
	}
	for i, e := range out {
		k, v, _ := strings.Cut(e, "=")
		out[i] = k + "=" + repl.Replace(v)
	}
	return out, nil
}

// buildEnv composes the environment for one CLI run, last layer winning:
// the daemon's own environment, the shared base spec, the command's own spec,
// and finally hap's injected HAP_* variables — which stay authoritative so an
// operator's env can never break the MCP wiring.
func buildEnv(base, cmd EnvSpec, repl *strings.Replacer, injected ...string) ([]string, error) {
	env := newEnvSet(os.Environ())
	for _, layer := range []EnvSpec{base, cmd} {
		entries, err := layer.entries(repl)
		if err != nil {
			return nil, err
		}
		env.merge(entries)
	}
	env.merge(injected)
	return env.slice(), nil
}

// reservedEnvKey reports whether a variable belongs to hap/herdr rather than
// the operator. These name the MCP request, the plugin's state and config
// dirs, and the herdr binary: letting a configured environment override them
// would not just break this run's wiring, it would silently point any nested
// `hap`/`herdr` the CLI runs at a different installation.
func reservedEnvKey(key string) bool {
	return strings.HasPrefix(key, "HAP_") || strings.HasPrefix(key, "HERDR_")
}

// envSet is an ordered KEY=VALUE set: a later assignment replaces an earlier
// one in place rather than appending a duplicate. Duplicate keys in an
// environment are resolved inconsistently across platforms, so the winner is
// decided here instead of being left to exec.
type envSet struct {
	order []string
	index map[string]int
}

func newEnvSet(entries []string) *envSet {
	s := &envSet{index: make(map[string]int, len(entries))}
	s.merge(entries)
	return s
}

func (s *envSet) merge(entries []string) {
	for _, e := range entries {
		key, _, ok := strings.Cut(e, "=")
		if !ok || key == "" {
			continue
		}
		if i, seen := s.index[key]; seen {
			s.order[i] = e
			continue
		}
		s.index[key] = len(s.order)
		s.order = append(s.order, e)
	}
}

func (s *envSet) slice() []string { return s.order }

// LoadEnvFile parses a `.env` file into ordered KEY=VALUE entries. It supports
// the common dotenv shape: comments, blank lines, an optional `export` prefix,
// and single-, double-, or un-quoted values. A file with a malformed line, or
// one that defines nothing at all, is an error naming the line NUMBER but
// never its content.
func LoadEnvFile(path string) ([]string, error) {
	resolved, err := expandEnvPath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("read env file %s: %w", resolved, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("read env file %s: is a directory", resolved)
	}
	if info.Size() > maxEnvFileBytes {
		return nil, fmt.Errorf("env file %s is too large (%d bytes > %d cap)",
			resolved, info.Size(), maxEnvFileBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read env file %s: %w", resolved, err)
	}
	entries, malformed := parseEnvFile(string(data))
	// A file the operator pointed at that yields nothing usable is a
	// misconfiguration which would otherwise surface minutes later as an
	// opaque authentication error inside the CLI. Fail here instead, where
	// the message can name the file — but never a line's content, which is
	// exactly where a secret would be.
	if len(malformed) > 0 {
		return nil, fmt.Errorf("env file %s has %d malformed line(s), first at line %d",
			resolved, len(malformed), malformed[0])
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("env file %s defines no variables", resolved)
	}
	return entries, nil
}

// parseEnvFile does the line work for LoadEnvFile: it returns the parsed
// entries plus the 1-based numbers of the lines it could not parse.
func parseEnvFile(content string) (entries []string, malformed []int) {
	content = strings.TrimPrefix(content, "\ufeff") // editors that write a BOM
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, rawValue, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value, valueOK := unquoteEnvValue(rawValue)
		if !ok || !validEnvKey(key) || !valueOK {
			malformed = append(malformed, i+1)
			continue
		}
		entries = append(entries, key+"="+value)
	}
	return entries, malformed
}

// unquoteEnvValue resolves the value half of a `.env` assignment. A
// single-quoted value is literal; a double-quoted one honours the usual
// escapes; an unquoted one is trimmed and loses a trailing ` # comment`.
// A value that opens a quote and never closes it — a pasted multi-line key,
// say — is rejected rather than handed to the CLI mangled.
func unquoteEnvValue(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if q, quoted := openingQuote(value); quoted {
		if len(value) < 2 || value[len(value)-1] != q {
			return "", false
		}
		inner := value[1 : len(value)-1]
		if q == '\'' {
			return inner, true
		}
		return doubleQuoteReplacer.Replace(inner), true
	}
	// An unquoted value ends at an inline comment, which must be preceded by
	// whitespace so that values like `pass#1` survive intact. Quote a value
	// that legitimately contains " #".
	if i := strings.Index(value, " #"); i >= 0 {
		value = value[:i]
	}
	if i := strings.Index(value, "\t#"); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value), true
}

// openingQuote reports the quote character a value starts with, if any.
func openingQuote(value string) (byte, bool) {
	if value == "" {
		return 0, false
	}
	q := value[0]
	return q, q == '\'' || q == '"'
}

var doubleQuoteReplacer = strings.NewReplacer(
	`\n`, "\n",
	`\r`, "\r",
	`\t`, "\t",
	`\"`, `"`,
	`\\`, `\`,
)

// validEnvKey reports whether name is a usable environment variable name
// ([A-Za-z_][A-Za-z0-9_]*).
func validEnvKey(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// expandEnvPath resolves "~" / "~/…" in a configured env file path.
func expandEnvPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand env file path %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
