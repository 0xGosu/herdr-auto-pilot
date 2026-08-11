// Package tasklocator resolves a declared task source to the single string that
// identifies its checklist — the LOCATOR — and canonicalizes that string.
//
// It exists because "which file is this source's list?" stopped being a
// filesystem question when task lists gained storage providers, while the
// identity string it produces is still used everywhere a path used to be: it
// keys the advisory lock, the daemon's in-memory claim map, the TUI's task
// grouping, and the persisted task_reservations / task_handouts rows.
//
// The package is PURE — no filesystem access beyond the symlink resolution
// Canonical has always done for local paths, and no network. It imports only
// internal/config, so the resolution rules can be shared by the daemon, the
// front-ends and the storage backends without any of them importing each other.
package tasklocator

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// GistScheme prefixes a locator naming a file inside a GitHub gist:
//
//	gist://<gist-id>/<file-name>
//
// A locator with no scheme is a filesystem path, which is what every locator
// was before storage providers existed — so an existing config, an existing
// ledger row and an existing `--path` argument all keep working untouched.
const GistScheme = "gist://"

// Scheme returns the locator's scheme ("gist://"), or "" for a filesystem path.
func Scheme(locator string) string {
	if strings.HasPrefix(locator, GistScheme) {
		return GistScheme
	}
	return ""
}

// Remote reports whether the locator names a list that is not on this machine.
func Remote(locator string) bool { return Scheme(locator) != "" }

// Canonical returns the single identity string for a task-list locator: the key
// the advisory lock hashes, the daemon's claim map uses, and the ledger
// persists.
//
// A locator carrying a scheme is ALREADY canonical and is returned verbatim.
// This is the load-bearing branch, because the corruption it prevents is
// silent: filepath.Abs("gist://abc/f.md") does not fail — Clean collapses the
// double slash and it returns "<cwd>/gist:/abc/f.md". Each hap process has a
// different working directory (the daemon runs from the state dir, the CLI from
// the operator's shell), so the same source would key DIFFERENTLY in each of
// them, and EvalSymlinks would then fail and be skipped without erroring. The
// daemon's reservations and the TUI's grouping would simply stop agreeing.
func Canonical(locator string) string {
	if Scheme(locator) != "" {
		return locator
	}
	p := config.ExpandPath(locator)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// GistRef is a parsed gist locator.
type GistRef struct {
	GistID string
	File   string
}

// ParseGist splits a gist locator into its id and file name. It reports
// ok=false for anything that is not a well-formed gist locator, including a
// bare filesystem path — callers dispatch on that rather than on configuration,
// so a ledger row written under an older provider still routes correctly.
func ParseGist(locator string) (GistRef, bool) {
	if Scheme(locator) != GistScheme {
		return GistRef{}, false
	}
	id, file, found := strings.Cut(strings.TrimPrefix(locator, GistScheme), "/")
	if !found || id == "" || file == "" {
		return GistRef{}, false
	}
	// A gist has no directories, so a second separator means this locator was
	// built from something path-shaped and must not be silently flattened.
	if strings.Contains(file, "/") {
		return GistRef{}, false
	}
	return GistRef{GistID: id, File: file}, true
}

// GistLocator builds the locator for one file inside a gist.
func GistLocator(gistID, file string) string {
	return GistScheme + gistID + "/" + file
}

// Resolved is a task source's storage identity: which backend serves it, with
// what credentials, and which list within that backend.
type Resolved struct {
	config.ResolvedProvider
	// Locator addresses the list for I/O. It is expanded and absolute (or a
	// scheme'd string for a remote store), but it is NOT the identity key:
	// callers that compare or persist one apply Canonical to it, which is what
	// collapses two spellings of a file to a single lock and claim key.
	Locator string
	// Display is the operator- and agent-facing address ({task_list_path}).
	// For a local list it equals Locator; for a remote one it is a URL the
	// operator can actually open, rather than the gist:// addressing string.
	Display string
}

// Resolve maps a task source to its backend identity and locator.
//
// agentName is the SHORT NAME of the agent the source was matched for, and it
// is a parameter rather than something read back out of the config because that
// is what "one file per agent, created on demand" requires: a source with a
// catch-all selector must give each matched agent its own <agent-name>.md.
// Every resolution site on the delivery path already has the name in hand —
// the daemon's task-source matcher takes it, the idle sweep iterates a live
// agent listing, and the front-end resolves by agent.
//
// Pass "" only from a surface that enumerates SOURCES rather than agents (the
// TUI's aggregate task view, `hap config task-source list`). Those get
// ErrAgentNameRequired for a derived source and render a template instead;
// nothing on the delivery path can reach it.
//
// The result is a pure function of (config, agent name): no process state, no
// working directory, no live lookup. That is what lets every hap process
// compute the same locator, and what makes the locator safe to persist.
func Resolve(cfg config.Config, src config.TaskSource, agentName string) (Resolved, error) {
	p := cfg.ResolveProvider(src)
	out := Resolved{ResolvedProvider: p}

	if !p.Remote() {
		if strings.TrimSpace(src.Path) == "" {
			return Resolved{}, fmt.Errorf("task source has no path and provider=%s does not derive one", p.Name)
		}
		// Expanded and absolute, but NOT symlink-resolved. Resolution is what
		// IDENTITY needs, and every identity site applies it itself —
		// taskfile.LockPath, the daemon's claim key and the TUI's grouping all
		// call Canonical on whatever they are given. Pre-resolving here instead
		// leaked into everything that COMPARES a locator against a configured
		// path (the max_tasks lookup) or PRINTS one ({task_list_path}), where on
		// macOS every /var/… path came back as /private/var/… — a spelling the
		// operator never wrote and their config does not match.
		out.Locator = DisplayPath(src.Path)
		out.Display = out.Locator
		return out, nil
	}

	if p.Name != config.ProviderGitHubGist {
		return Resolved{}, fmt.Errorf("unknown task source provider %q", p.Name)
	}
	if strings.TrimSpace(p.GistID) == "" {
		return Resolved{}, fmt.Errorf("task source provider %s has no gist_id", p.Name)
	}

	file := strings.TrimSpace(src.Path)
	if file == "" {
		// The derived, one-list-per-agent form.
		if agentName == "" {
			return Resolved{}, ErrAgentNameRequired
		}
		file = DerivedFileName(agentName)
	}
	if err := config.ValidateStoreFileName(file); err != nil {
		return Resolved{}, fmt.Errorf("task source file name: %w", err)
	}
	out.Locator = Canonical(GistLocator(p.GistID, file))
	out.Display = Display(out.Locator)
	return out, nil
}

// DisplayFor renders any locator as its operator-facing address: a gist
// locator becomes its web URL, and a filesystem locator is already the address
// (it is expanded and absolute but not symlink-resolved, so it still reads as
// the operator wrote it).
func DisplayFor(locator string) string { return Display(locator) }

// DisplayPath renders a filesystem task-list path as an operator reads it:
// ~/$VAR expanded and made absolute, so it means the same thing from any
// working directory, but NOT symlink-resolved. See Resolved.Display.
func DisplayPath(path string) string {
	p := config.ExpandPath(path)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// ErrAgentNameRequired reports that a source derives its file name per agent
// and the caller supplied none. It is expected — not exceptional — on surfaces
// that list sources rather than agents, which render a template instead.
var ErrAgentNameRequired = fmt.Errorf("this task source derives one list per matched agent, so it needs an agent name to resolve")

// Display renders a locator as the address {task_list_path} shows an operator
// or an agent.
//
// A filesystem locator is already that address. A gist locator becomes its web
// URL, and the alternatives are all worse: a bare file name ("brave-otter.md")
// is something the agent resolves against its OWN working directory, where it
// either fails confusingly or finds an unrelated file; an empty string turns
// "read the list at {task_list_path}" into nonsense. A URL fails loudly and is
// something the operator can actually open.
func Display(locator string) string {
	ref, ok := ParseGist(locator)
	if !ok {
		return locator
	}
	return "https://gist.github.com/" + ref.GistID + "#file-" + anchorSlug(ref.File)
}

// anchorSlug renders a gist file name as GitHub's per-file anchor: lowercased,
// with every run of non-alphanumerics collapsed to a single hyphen.
func anchorSlug(file string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(file) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// DerivedFileName is the checklist file name for an agent whose source names
// none. It reuses the same sanitization the generated-task bootstrap has always
// applied to build <state>/tasks/<name>.md, so a bootstrapped source and a
// derived one land on the same name for the same agent.
func DerivedFileName(agentName string) string {
	return config.SanitizeTaskFileName(agentName) + ".md"
}
