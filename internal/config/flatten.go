package config

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/BurntSushi/toml"
)

// FlattenKeys renders cfg as a map from every dotted config key to a
// comparable rendering of its value, exactly as Save would write it. An array
// (of values or of tables) is ONE key: its elements are addressed by position,
// never by a key of their own.
//
// It exists so a writer can say WHICH keys a config write changed without
// knowing what the write was — the one config-write path serves some twenty
// callers, and a per-caller list of touched keys would be stale the day a new
// caller is added.
func FlattenKeys(cfg Config) (map[string]string, error) {
	cfg.normalizeTaskSources()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return nil, err
	}
	var tree map[string]any
	if _, err := toml.Decode(buf.String(), &tree); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenInto(out, "", tree)
	return out, nil
}

func flattenInto(out map[string]string, prefix string, tree map[string]any) {
	for k, v := range tree {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flattenInto(out, key, sub)
			continue
		}
		// fmt prints maps with sorted keys, so an array of tables renders
		// deterministically.
		out[key] = fmt.Sprintf("%#v", v)
	}
}

// ChangedKeys lists, sorted, every key whose value differs between two
// flattenings.
//
// A key present on one side only is compared against its ZERO value, never
// counted as changed outright. The encoder only ever drops a zero value
// (omitempty), and it drops whole sections the same way — so the first write
// into `[full_self_prompting]` materializes every sibling of the key actually
// set, each at its unchanged zero value.
func ChangedKeys(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		b, ok := before[k]
		if (ok && b != v) || (!ok && !zeroRendering(v)) {
			out = append(out, k)
		}
	}
	for k, v := range before {
		if _, ok := after[k]; !ok && !zeroRendering(v) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// zeroRendering reports whether v is how flattenInto renders a zero value.
func zeroRendering(v string) bool {
	switch v {
	case "false", "0", `""`, "[]interface {}{}", "[]map[string]interface {}{}":
		return true
	}
	return false
}
