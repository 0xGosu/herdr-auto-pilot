package domain

import (
	"regexp"
	"strings"
)

// Claude Code's remote sub-agent launcher renders a "Select remote
// environment" picker: a title line, an optional "Configure environments
// at: …" hint, `❯ N. label (env_id)` options (the account default carries a
// trailing ✔), and an "Enter to select · Esc to cancel" footer. Herdr
// reports the pane IDLE while this modal stands (verified live 2026-07-17),
// so — like Codex's Plan approval — the structural form itself must be
// strong enough to prove Claude is awaiting an approval. Requiring the
// title, the footer at the true end of the capture, and at least two
// numbered options in between keeps a narrated or stale copy in scrollback
// from becoming a live permission prompt.
var (
	claudeRemoteEnvTitleRE = regexp.MustCompile(`(?im)^\s*Select remote environment\s*$`)
	// End-of-text anchor (\s*\z): a live modal sits at the true bottom of
	// the capture (verified: nothing renders below the footer). If a future
	// Claude build draws anything under it, relax this to "footer within the
	// last few lines".
	claudeRemoteEnvFooterRE = regexp.MustCompile(`(?im)^\s*Enter to select\s*·\s*Esc to cancel\s*\z`)
	// The account-default environment carries a trailing check marker; strip
	// it so learned labels stay stable whether or not the marker is present.
	remoteEnvCheckSuffixRE = regexp.MustCompile(`\s*✔\s*$`)
)

// RemoteEnvForm is the parsed live state of Claude Code's "Select remote
// environment" picker.
type RemoteEnvForm struct {
	Region         string           // title→footer slice of the pane (live form only)
	Options        []NumberedOption // labels with the trailing ✔ marker stripped
	SelectedOption string           // digit under the ❯ caret ("" when absent)
}

// ClaudeRemoteEnvForm reports whether pane shows the live picker and parses
// it. It deliberately recognizes only this Claude-specific form; callers
// must additionally gate on agent_type == "claude". The classifier treats a
// standing picker as a parked APPROVAL at idle/done status (launching a
// remote agent spawns a paid cloud environment, so `working` stays excluded
// until live evidence shows Herdr reporting it — extend the parked statuses
// in classify.Classify if that happens).
func ClaudeRemoteEnvForm(pane string) (RemoteEnvForm, bool) {
	titles := claudeRemoteEnvTitleRE.FindAllStringIndex(pane, -1)
	footers := claudeRemoteEnvFooterRE.FindAllStringIndex(pane, -1)
	if len(titles) == 0 || len(footers) == 0 {
		return RemoteEnvForm{}, false
	}
	// Last title and last footer are the live render (a consuming "recent"
	// read can carry earlier complete renders above the current one). The \z
	// anchor already makes an earlier render's footer unmatchable, but pair
	// last-with-last anyway so relaxing the anchor can never re-pair the live
	// title with a stale footer.
	title := titles[len(titles)-1]
	footer := footers[len(footers)-1]
	if footer[0] <= title[0] {
		return RemoteEnvForm{}, false
	}
	region := pane[title[0]:footer[1]]
	opts := ParseNumberedOptions(region)
	if len(opts) < 2 {
		return RemoteEnvForm{}, false
	}
	form := RemoteEnvForm{Region: region, Options: make([]NumberedOption, 0, len(opts))}
	for _, o := range opts {
		o.Label = strings.TrimSpace(remoteEnvCheckSuffixRE.ReplaceAllString(o.Label, ""))
		if o.Label == "" {
			return RemoteEnvForm{}, false
		}
		form.Options = append(form.Options, o)
	}
	if m := mcqTabCaretRE.FindStringSubmatch(region); m != nil {
		form.SelectedOption = m[1]
	}
	return form, true
}

// TrimRemoteEnvCheck strips the picker's trailing ✔ marker from a label, so
// a reply recorded from the raw render still matches the normalized options.
func TrimRemoteEnvCheck(label string) string {
	return strings.TrimSpace(remoteEnvCheckSuffixRE.ReplaceAllString(label, ""))
}

// OptionLabels returns the ✔-stripped option labels in display order (the
// classifier's option set for the enriched approval situation).
func (f RemoteEnvForm) OptionLabels() []string {
	labels := make([]string, 0, len(f.Options))
	for _, o := range f.Options {
		labels = append(labels, o.Label)
	}
	return labels
}
