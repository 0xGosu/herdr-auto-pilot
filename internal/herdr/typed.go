package herdr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Typed delivery to claude — [agents] claude_typed_input.
//
// Claude Code wraps a PASTE in `<pasted_content>` tags, and its system prompt
// tells the model that instructions inside them "establish user intent only
// where the user's own words outside the tags direct the agent to act on
// them". A hap hand-out delivered as a paste is therefore a message with no
// user words at all: the agent reads its own next task as untrusted quoted
// text. Verified live against Claude Code 2.1.283: a four-line hand-out sent
// through `agent prompt` reached the model wrapped whole.
//
// What Claude counts as a paste is read from its input tokenizer (2.1.283):
//
//   - anything between bracketed-paste markers (ESC[200~ … ESC[201~) — which is
//     how `agent prompt` delivers, honoring the pane's paste mode;
//   - an UNBRACKETED text run longer than claudePasteRunLimit that arrives in
//     ONE input read. A single `pane send-text` of more than that is one.
//
// Anything else is typing. So the typed route writes the text in bursts far
// below the limit, PACED so Claude reads each before the next piece arrives —
// the pacing, not the burst size, is what holds the line. Measured live: with
// no pacing, herdr's socket turns a request around in under a millisecond, and
// a Claude read stall right after the first burst let 3,400 chars pile up into
// one read — 16-char bursts and 64-char bursts both came out pasted. Paced at
// 10ms, 64-char bursts typed a 7,300-char message exactly, including under a
// saturated CPU. claudeTypedChunkGap is twice that.
const (
	// claudePasteRunLimit is Claude Code's own threshold (2.1.283, `t8`): a
	// single unbracketed read longer than this many UTF-16 code units is
	// dispatched as a paste.
	claudePasteRunLimit = 800
	// claudeTypedBurstBytes is the longest single-line body left on the
	// ordinary one-burst route. Measured in BYTES, which bound UTF-16 units
	// from above (one byte per ASCII unit, two or three per BMP unit, four per
	// surrogate pair), and set well under claudePasteRunLimit because that
	// constant is minified and moves between builds. Under it the burst already
	// reads as typing, and keeping it there leaves menu digits and `/rename`
	// byte-for-byte on the route every other safety rule was verified against.
	claudeTypedBurstBytes = 512
	// claudeTypedChunkUnits is one burst's size in UTF-16 code units.
	claudeTypedChunkUnits = 64
	// claudeTypedChunkGap spaces burst STARTS, so the write rate is bounded at
	// claudeTypedChunkUnits per gap (3.2 units/ms): a Claude read stall of up to
	// ~250ms still leaves every read under claudePasteRunLimit. When herdr
	// itself is slower than this (a loaded machine measured 30-50ms a request)
	// no sleep is added at all.
	claudeTypedChunkGap = 20 * time.Millisecond
	// claudeTypedFirstSettle follows the FIRST burst instead of the gap. The one
	// live failure that pacing alone does not explain was a read stall of over
	// 100ms immediately after the first burst — the composer leaving its empty
	// state — so that is where the extra margin goes.
	claudeTypedFirstSettle = 100 * time.Millisecond
	// typedChunkTimeout bounds one burst's round trip.
	typedChunkTimeout = 5 * time.Second
)

// claudeTypedBody reports whether body should be TYPED into a claude
// composer, and the exact text to type. ok=false keeps today's route.
//
// The rule is "never type something that means something different": a paste
// is inert text, while keystrokes are interpreted one by one, so anything that
// keystrokes would reinterpret stays on the route it has always taken.
func claudeTypedBody(body string) (text string, ok bool) {
	text = strings.ReplaceAll(body, "\r\n", "\n")
	// Not a paste today: nothing to fix, and nothing to risk.
	if !strings.Contains(text, "\n") && len(text) <= claudeTypedBurstBytes {
		return "", false
	}
	// A leading `!` switches Claude to SHELL mode — verified live for a lone
	// keystroke and a burst alike — and the rest then runs as a command outside
	// Claude's permission prompts. (`?` only opens the shortcuts panel as a LONE
	// keystroke, which a burst of two or more characters never is.)
	if strings.HasPrefix(text, "!") {
		return "", false
	}
	if !utf8.ValidString(text) {
		return "", false
	}
	for _, r := range text {
		// LF is Claude's `chat:newline` (ctrl+j), verified to insert a line
		// break rather than submit. Every other control character is a KEY
		// when typed: a bare CR submits, Tab is bound, ESC starts a sequence,
		// ^C / ^D interrupt or exit.
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", false
		}
	}
	return text, true
}

// typedChunks splits text into pieces of at most maxUnits UTF-16 code units —
// Claude measures JS string length — never cutting a rune.
func typedChunks(text string, maxUnits int) []string {
	var chunks []string
	start, units := 0, 0
	for i, r := range text {
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if units+n > maxUnits && i > start {
			chunks = append(chunks, text[start:i])
			start, units = i, 0
		}
		units += n
	}
	if start < len(text) {
		chunks = append(chunks, text[start:])
	}
	return chunks
}

// typedDialError marks a failure to REACH the socket: no request was sent, so
// nothing can have reached the pane and another transport may be tried.
type typedDialError struct{ err error }

func (e *typedDialError) Error() string { return e.err.Error() }
func (e *typedDialError) Unwrap() error { return e.err }

// typedDialer returns the socket transport for typed bursts, or nil when there
// is no socket to try (the CLI then types every burst).
//
// This is the THIRD request herdr's socket carries (after the event stream and
// notification.show), and the reason is cost per request: a typed hand-out is
// dozens of writes, and `herdr pane send-text` measured ~42ms a process — a
// 1,000-char hand-out would take a second of process spawns and stall the
// select loop that sends it. The socket answers in about a millisecond.
func (c *CLI) typedDialer() func(context.Context) (net.Conn, error) {
	base := c.dialSocket
	if base == nil {
		if c.SocketPath == "" {
			return nil
		}
		path := c.SocketPath
		base = func(ctx context.Context) (net.Conn, error) {
			d := net.Dialer{Timeout: typedChunkTimeout}
			return d.DialContext(ctx, "unix", path)
		}
	}
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := base(ctx)
		if err != nil {
			return nil, &typedDialError{err: err}
		}
		return conn, nil
	}
}

// submitKeystrokes types text into the pane in paced bursts and then submits
// it with an explicit Enter (so explicitEnter is true, and the claude
// submit-retry loop behaves exactly as on the single-line route).
//
// That Enter is the one keystroke here a modal can capture. The paste route
// carried its Enter inside the same request; this one follows the last burst by
// the whole typing time, and an agent that leaves idle in that window (a
// background shell finishing, a hook) can raise a permission prompt whose
// highlighted option the Enter would COMMIT. A burst is harmless there — its
// digits arrive inside a multi-character run, never as a lone key — so the
// Enter alone gets the look the submit-retry loop takes before each of its
// presses (typedSubmitRefusal).
//
// Failure semantics are the point of the structure: only the FIRST burst may
// change transport, because before it lands nothing has reached the pane.
// After that a failure leaves a partial message in the composer, so it is
// returned as-is, no Enter is pressed, and nothing is retried — a retry would
// type the message a second time behind the first half.
func (c *CLI) submitKeystrokes(ctx context.Context, paneID, text, snapshot string) (bool, error) {
	chunks := typedChunks(text, claudeTypedChunkUnits)
	gap, settle := c.typedPacing()
	dial := c.typedDialer()
	var next time.Time
	for i, chunk := range chunks {
		if err := sleepUntil(ctx, next); err != nil {
			return false, typedStopped(i, len(chunks), err)
		}
		start := time.Now()
		if err := c.typeChunk(ctx, paneID, chunk, &dial, i == 0); err != nil {
			return false, typedStopped(i, len(chunks), err)
		}
		if i == 0 {
			next = start.Add(settle)
		} else {
			next = start.Add(gap)
		}
	}
	if err := sleepUntil(ctx, next); err != nil {
		return false, typedStopped(len(chunks), len(chunks), err)
	}
	if err := c.typedSubmitRefusal(ctx, paneID, snapshot); err != nil {
		return false, typedStopped(len(chunks), len(chunks), err)
	}
	if _, err := c.run(ctx, "pane", "send-keys", paneID, "enter"); err != nil {
		return false, typedStopped(len(chunks), len(chunks), err)
	}
	return true, nil
}

// errTypedSubmitModal refuses the typed route's Enter: a prompt appeared while
// the message was being typed.
var errTypedSubmitModal = errors.New("claude raised a prompt while the message was being typed; " +
	"Enter was not pressed, so the message is waiting in the composer")

// typedSubmitRefusal is the last look before the typed route's Enter. It refuses
// only on EVIDENCE of a modal — the status moving to blocked since the
// pre-send snapshot, or a standing form on screen (paneShowsStandingForm, which
// also counts an unreadable pane, as the retry loop does). Keyed on the CHANGE:
// an agent that was already blocked when the operator chose to send is the
// operator's call, exactly as on the other routes.
func (c *CLI) typedSubmitRefusal(ctx context.Context, paneID, snapshot string) error {
	if st, ok := c.probeAgentStatus(ctx, paneID); ok && st == "blocked" && snapshot != "blocked" {
		return errTypedSubmitModal
	}
	if c.paneShowsStandingForm(ctx, paneID) {
		return errTypedSubmitModal
	}
	return nil
}

// typeChunk writes one burst, over the socket while it is usable. A socket that
// is unreachable, or a herdr that does not know `pane.send_text`, demotes the
// rest of THIS send to `pane send-text` — decided on the first burst only.
func (c *CLI) typeChunk(ctx context.Context, paneID, chunk string,
	dial *func(context.Context) (net.Conn, error), first bool) error {

	if *dial != nil {
		cctx, cancel := context.WithTimeout(ctx, typedChunkTimeout)
		id := fmt.Sprintf("hap_type_%d", c.typedSeq.Add(1))
		err := call(cctx, *dial, id, "pane.send_text",
			map[string]any{"pane_id": paneID, "text": chunk}, nil)
		cancel()
		if err == nil {
			return nil
		}
		if !first || !typedSocketUnusable(ctx, err) {
			return err
		}
		slog.Warn("herdr socket cannot take typed input; typing through the CLI instead",
			"pane", paneID, "error", err)
		*dial = nil
	}
	_, err := c.run(ctx, "pane", "send-text", paneID, chunk)
	return err
}

// typedSocketUnusable reports whether a failed first burst provably reached
// nothing AND the CLI could do better. A herdr refusal about the PANE is not
// that: the CLI reaches the same herdr and is refused the same way. A failure
// after the request was written is not either — herdr may have typed it.
func typedSocketUnusable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var de *typedDialError
	if errors.As(err, &de) {
		return true
	}
	// herdr 0.9.0 rejects an unknown method as `invalid_request` ("unknown
	// variant"); older builds said `method_not_found`. Either way the request
	// was refused before anything was typed.
	var se *SocketError
	return errors.As(err, &se) && (se.Code == "invalid_request" || se.Code == "method_not_found")
}

// typedStopped words a typed-route failure by how far it got, so an operator
// reading the escalation knows whether the composer holds a partial message.
func typedStopped(done, total int, err error) error {
	switch {
	case done == 0:
		return err
	case done < total:
		return fmt.Errorf("typed delivery stopped after %d of %d bursts; the composer holds a partial message and no Enter was pressed: %w",
			done, total, err)
	default:
		return fmt.Errorf("typed delivery wrote the whole message but could not submit it: %w", err)
	}
}

// typedPacing returns the burst gap and first-burst settle, honoring test
// overrides.
func (c *CLI) typedPacing() (gap, settle time.Duration) {
	gap, settle = claudeTypedChunkGap, claudeTypedFirstSettle
	if c.typedGap > 0 {
		gap = c.typedGap
	}
	if c.typedSettle > 0 {
		settle = c.typedSettle
	}
	return gap, settle
}

// sleepUntil waits for deadline (a zero or past deadline returns at once).
func sleepUntil(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d := time.Until(deadline); d > 0 {
		return sleepCtx(ctx, d)
	}
	return nil
}
