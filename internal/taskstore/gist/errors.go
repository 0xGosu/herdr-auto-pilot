// This file deliberately imports NOTHING networked. The egress allowlist in
// internal/privacy names gist.go by path, so keeping error shaping, redaction
// and the truncation rule here means the allowlisted surface stays one file.
package gist

import (
	"errors"
	"fmt"
	"strings"
)

// ErrTruncated reports that GitHub returned only part of a task list.
//
// This is the most dangerous failure this backend can have, and it is invisible
// without an explicit check: go-github's GistFile carries no `truncated` field,
// so a read-modify-write over a truncated body would PATCH the truncated bytes
// back and SILENTLY DELETE the tail of the operator's checklist. Every read and
// every mutation refuses on it.
var ErrTruncated = errors.New("the task list is too large: GitHub returned only part of it, " +
	"so writing it back would delete the rest")

// ErrFileListTruncated reports a gist at GitHub's file-count ceiling, where the
// file list itself comes back partial.
//
// It matters because it makes "the file is not in this gist" ambiguous between
// ABSENT and HIDDEN, and the two have opposite correct responses: absent means
// create it, hidden means creating it would shadow a list that already exists.
// Refusing is the only safe reading.
var ErrFileListTruncated = errors.New("this gist holds too many files for GitHub to list them all, " +
	"so hap cannot tell whether the task list is missing or merely hidden")

// ErrBlankContent reports a write GitHub will not accept: a gist file whose
// content is empty or whitespace only.
//
// This is a hard property of the API, not a quota or a permission — verified
// live (2026-08-12) against a real gist, where PATCHing a file with "", "\n" or
// " " all fail identically with `422 Validation Failed {Resource:Gist
// Field:files Code:missing_field}`. GitHub reads a blank body as "this entry
// carries no file", drops it, and then rejects the request for having an empty
// `files` map — so the error names `files`, never the file, and reads as a
// malformed request rather than as the one thing that is actually wrong.
//
// A gist therefore cannot represent an EMPTY task list at all; only a list with
// at least one non-blank line, which is why every caller creates one with a
// header. Refusing is the whole answer: substituting a placeholder would write
// content the caller never asked for, and a newline would not even work.
var ErrBlankContent = errors.New("GitHub will not store a gist file with no content, so this task list " +
	"cannot be written blank — give it at least a header line")

// blank reports whether content is empty or whitespace only, i.e. whether
// GitHub will refuse it.
func blank(content string) bool { return strings.TrimSpace(content) == "" }

// gistFileCeiling is where GitHub starts truncating a gist's file list.
const gistFileCeiling = 300

// truncated reports whether a gist file's returned content is short of its real
// size. GitHub reports the full size but sends a clipped body, so the mismatch
// IS the signal.
func truncated(size int, content string) bool {
	return size > 0 && len(content) != size
}

// redact rewrites an error so it can be logged, audited and shown to an
// operator.
//
// Two things must never survive: the token, and the gist id. The token is
// obvious. The gist id matters because a SECRET gist is protected only by the
// unguessability of its URL — it is effectively a capability — and go-github's
// ErrorResponse.Error() embeds the request URL, whose sanitizer strips only
// `client_secret`. So the id is shortened to a prefix an operator can still
// recognize in their own config.
func redact(gistID string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if gistID != "" {
		msg = strings.ReplaceAll(msg, gistID, shortID(gistID))
	}
	return errors.New(msg)
}

// shortID renders a gist id as a recognizable prefix rather than in full.
func shortID(gistID string) string {
	const keep = 8
	if len(gistID) <= keep {
		return gistID
	}
	return gistID[:keep] + "…"
}

// wrapf builds a redacted, contextual error in one step.
func wrapf(gistID string, err error, format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), redact(gistID, err))
}
