package tui

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestEditPromptOffersStorageOnlyWhenAProviderIsConfigured pins both halves of
// the visibility rule: an install that never touched the storage setting keeps
// the picker it has always had, and one that did gets the rows it needs.
func TestEditPromptOffersStorageOnlyWhenAProviderIsConfigured(t *testing.T) {
	m, app, path := sourcePromptModel(t)
	if msg := submitSourcePrompt(t, m, path+" busy-otter"); msg.err != nil {
		t.Fatal(msg.err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	m.data.cfg = cfg

	t.Run("default install offers exactly the historical three", func(t *testing.T) {
		upd, _ := m.editTaskSourcePrompt(0, path)
		got := upd.(Model)
		if got.prompt == nil {
			t.Fatal("picker did not open")
		}
		if len(got.prompt.options) != 3 {
			t.Errorf("picker options = %v, want only the historical three", got.prompt.options)
		}
		for _, o := range got.prompt.options {
			if strings.HasPrefix(o, tsFieldProvider) || strings.HasPrefix(o, tsFieldGistID) {
				t.Errorf("a default install must not be offered storage settings: %q", o)
			}
		}
	})

	// withCfg returns an independent Model: a Model copy shares its config's
	// TaskSources BACKING ARRAY, so mutating an element in one subtest would
	// leak into the next.
	withCfg := func(mutate func(*config.Config)) Model {
		out := m
		c := cfg
		c.TaskSources = append([]config.TaskSource(nil), cfg.TaskSources...)
		mutate(&c)
		out.data.cfg = c
		return out
	}

	t.Run("a source overridden back to local offers provider but not gist_id", func(t *testing.T) {
		// The DEFAULT is remote — that is what makes storage worth showing —
		// while this source is pinned local, so a gist id would mean nothing.
		local := withCfg(func(c *config.Config) {
			c.TaskSourceProvider.Provider = config.ProviderGitHubGist
			c.TaskSourceProvider.GitHubGist.GistID = "3f2a1b9c4d5e6f70"
			c.TaskSources[0].Provider = config.ProviderLocalFS
		})
		upd, _ := local.editTaskSourcePrompt(0, path)
		got := upd.(Model)
		if !hasOption(got, tsFieldProvider) {
			t.Errorf("provider row missing: %v", got.prompt.options)
		}
		if hasOption(got, tsFieldGistID) {
			t.Errorf("gist_id is meaningless under local storage and must not be offered: %v",
				got.prompt.options)
		}
		if !optionContains(got, tsFieldProvider, "override") {
			t.Errorf("an explicit override must not read as inherited: %v", got.prompt.options)
		}
	})

	t.Run("an inheriting remote source offers both rows", func(t *testing.T) {
		remote := withCfg(func(c *config.Config) {
			c.TaskSourceProvider.Provider = config.ProviderGitHubGist
			c.TaskSourceProvider.GitHubGist.GistID = "3f2a1b9c4d5e6f70"
		})
		upd, _ := remote.editTaskSourcePrompt(0, path)
		got := upd.(Model)
		if !hasOption(got, tsFieldProvider) || !hasOption(got, tsFieldGistID) {
			t.Errorf("a remote provider must offer both storage rows: %v", got.prompt.options)
		}
		// Provenance is on the label: an inherited value and an identical
		// override must not read the same.
		if !optionContains(got, tsFieldProvider, "inherited") {
			t.Errorf("the provider row must say it is inherited: %v", got.prompt.options)
		}
		// A secret gist's URL is effectively a capability, so the id is elided.
		if optionContains(got, tsFieldGistID, "3f2a1b9c4d5e6f70") {
			t.Errorf("the gist id must not be shown in full: %v", got.prompt.options)
		}
	})
}

// TestEditPromptStorageFieldsReachTheConfig closes the gap the dead-end default
// in openTaskSourceFieldPrompt exists for: a key added to the picker without
// its case would open no prompt at all.
func TestEditPromptStorageFieldsReachTheConfig(t *testing.T) {
	m, app, path := sourcePromptModel(t)
	if msg := submitSourcePrompt(t, m, path+" busy-otter"); msg.err != nil {
		t.Fatal(msg.err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.TaskSourceProvider.Provider = config.ProviderGitHubGist
	cfg.TaskSourceProvider.GitHubGist.GistID = "3f2a1b9c"
	m.data.cfg = cfg

	t.Run("provider", func(t *testing.T) {
		msg := driveSourceField(t, m, 0, path, tsFieldProvider, config.ProviderLocalFS)
		if msg.err != nil {
			t.Fatal(msg.err)
		}
		saved, lerr := config.Load(app.ConfigPath)
		if lerr != nil {
			t.Fatal(lerr)
		}
		if got := saved.TaskSources[0].Provider; got != config.ProviderLocalFS {
			t.Errorf("provider = %q, want the override recorded", got)
		}
		// The operator must be told the list did not move with the setting.
		if !strings.Contains(msg.message, "not moved") {
			t.Errorf("message = %q, want it to say the existing list is not migrated", msg.message)
		}
	})

	t.Run("provider inherit clears the override", func(t *testing.T) {
		saved, lerr := config.Load(app.ConfigPath)
		if lerr != nil {
			t.Fatal(lerr)
		}
		m.data.cfg = saved
		m.data.cfg.TaskSourceProvider.Provider = config.ProviderGitHubGist
		if msg := driveSourceField(t, m, 0, path, tsFieldProvider, tsInheritValue); msg.err != nil {
			t.Fatal(msg.err)
		}
		saved, lerr = config.Load(app.ConfigPath)
		if lerr != nil {
			t.Fatal(lerr)
		}
		if got := saved.TaskSources[0].Provider; got != "" {
			t.Errorf("provider = %q, want it CLEARED — without a clearing spelling an "+
				"override is a one-way door", got)
		}
	})
}

func hasOption(m Model, prefix string) bool {
	if m.prompt == nil {
		return false
	}
	for _, o := range m.prompt.options {
		if strings.HasPrefix(o, prefix) {
			return true
		}
	}
	return false
}

func optionContains(m Model, prefix, want string) bool {
	if m.prompt == nil {
		return false
	}
	for _, o := range m.prompt.options {
		if strings.HasPrefix(o, prefix) && strings.Contains(o, want) {
			return true
		}
	}
	return false
}

// driveSourceField runs the two-step edit (settings picker → value prompt) for
// one field, without the historical-three assertion editSourceSetting makes.
func driveSourceField(t *testing.T, m Model, index int, path, field, value string) actionResultMsg {
	t.Helper()
	upd, _ := m.editTaskSourcePrompt(index, path)
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatal("settings picker did not open")
	}
	var chosen string
	for _, o := range m.prompt.options {
		if strings.HasPrefix(o, field) {
			chosen = o
		}
	}
	if chosen == "" {
		t.Fatalf("picker has no %q row: %v", field, m.prompt.options)
	}
	cmd := m.prompt.onSubmit(chosen)
	if cmd == nil {
		t.Fatal("picker returned no command")
	}
	fieldMsg, ok := cmd().(openTaskSourceFieldMsg)
	if !ok {
		t.Fatalf("picker should chain to a value prompt, got %T", cmd())
	}
	upd, _ = m.Update(fieldMsg)
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatalf("%s opened no value prompt — the dead-end default was hit, so the field "+
			"is in the picker but has no case", field)
	}
	cmd = m.prompt.onSubmit(value)
	if cmd == nil {
		t.Fatal("value prompt returned no command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("value prompt should produce an actionResultMsg, got %T", cmd())
	}
	return res
}
