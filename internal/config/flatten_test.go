package config

import (
	"reflect"
	"testing"
)

func TestChangedKeysNamesExactlyWhatMoved(t *testing.T) {
	base := Default()
	before, err := FlattenKeys(base)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := FlattenKeys(base); len(ChangedKeys(before, again)) != 0 {
		t.Fatalf("flattening the same config twice reported changes: %v", ChangedKeys(before, again))
	}

	edited := Default()
	edited.LLM.Command = []string{"claude", "-p", "x"}
	edited.TaskSources = append(edited.TaskSources, TaskSource{Agent: "calm-pika"})
	after, err := FlattenKeys(edited)
	if err != nil {
		t.Fatal(err)
	}
	got := ChangedKeys(before, after)
	want := []string{"llm.command", "task_sources"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedKeys = %v, want %v", got, want)
	}
}

func TestChangedKeysReportsARemovedKey(t *testing.T) {
	got := ChangedKeys(map[string]string{"a.b": "1", "c": "2"}, map[string]string{"c": "2"})
	if !reflect.DeepEqual(got, []string{"a.b"}) {
		t.Fatalf("ChangedKeys = %v, want [a.b]", got)
	}
}

// Setting one key in an omitempty section writes the whole section; its
// siblings appear at their zero values and must not read as changed.
func TestChangedKeysIgnoresASectionMaterializingAtZero(t *testing.T) {
	before, err := FlattenKeys(Default())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.FullSelfPrompting.HonourLimits = true
	after, err := FlattenKeys(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := ChangedKeys(before, after); !reflect.DeepEqual(got, []string{"full_self_prompting.honour_limits"}) {
		t.Fatalf("ChangedKeys = %v, want only full_self_prompting.honour_limits", got)
	}
	// And back: the section vanishing again is one change, not three.
	if got := ChangedKeys(after, before); !reflect.DeepEqual(got, []string{"full_self_prompting.honour_limits"}) {
		t.Fatalf("reverse ChangedKeys = %v", got)
	}
}
