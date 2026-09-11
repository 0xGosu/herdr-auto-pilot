package domain

import (
	"strconv"
	"strings"
	"time"
)

// StreamEvent is one entry of the orchestrator event stream (`hap stream
// orchestrator`): a change an operator, the daemon or a front end made, named
// by the ids of what it touched rather than by its content.
//
// The payload is minimal ON PURPOSE. A reader is an agent that re-fetches
// whatever it needs through the hap CLI, so carrying the content would only
// duplicate — and go stale beside — the source of truth, and would put task
// text, config values and secrets into a log that outlives them.
type StreamEvent struct {
	// Seq is the stream counter, assigned by the log on append (0 before).
	Seq    int64
	At     time.Time
	Kind   string
	Author string
	Fields []StreamField
	// Dedupe, when set, makes the append idempotent: a second event carrying
	// the same key is dropped. Used where the emitter re-examines state on a
	// timer and must fire once per resource ("escalation:<id>").
	Dedupe string
	// Rendered is the pre-rendered field list as the log stores it. Set on
	// events READ from the log; Line prefers it over Fields.
	Rendered string
}

// StreamField is one key=value pair of a StreamEvent's payload.
type StreamField struct {
	Key   string
	Value string
}

// Stream event kinds. The spelling is the public contract a reader switches on.
const (
	StreamConfigChanged       = "config.changed"
	StreamTaskSourceAdded     = "task_source.added"
	StreamTaskSourceRemoved   = "task_source.removed"
	StreamTaskSourceUpdated   = "task_source.updated"
	StreamTaskCreated         = "task.created"
	StreamTaskUpdated         = "task.updated"
	StreamTaskDeleted         = "task.deleted"
	StreamTaskMoved           = "task.moved"
	StreamTaskListCreated     = "tasklist.created"
	StreamTaskListDeleted     = "tasklist.deleted"
	StreamEscalation          = "escalation"
	StreamEscalationDismissed = "escalation.dismissed"
	StreamCorrection          = "correction"
	StreamPauseOn             = "pause.on"
	StreamPauseOff            = "pause.off"
	StreamFSPOn               = "fsp.on"
	StreamFSPOff              = "fsp.off"
	StreamRuleStreak          = "rule.streak"
	StreamRuleReset           = "rule.reset"
	StreamRuleDeleted         = "rule.deleted"
	StreamDaemonStarted       = "daemon.started"
)

// StreamStr and StreamInt build a payload field.
func StreamStr(key, value string) StreamField { return StreamField{Key: key, Value: value} }

// StreamInt builds an integer payload field.
func StreamInt(key string, value int64) StreamField {
	return StreamField{Key: key, Value: strconv.FormatInt(value, 10)}
}

// RenderStreamFields renders fields as space-separated key=value pairs, in
// order, quoting any value a naive whitespace split would get wrong.
func RenderStreamFields(fields []StreamField) string {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f.Key)
		b.WriteByte('=')
		b.WriteString(streamValue(f.Value))
	}
	return b.String()
}

// Line renders the event as the one line `hap stream orchestrator` prints:
//
//	<seq> <RFC3339 UTC> <kind> [k=v ...] by=<author>
func (e StreamEvent) Line() string {
	fields := e.Rendered
	if fields == "" {
		fields = RenderStreamFields(e.Fields)
	}
	var b strings.Builder
	b.WriteString(strconv.FormatInt(e.Seq, 10))
	b.WriteByte(' ')
	b.WriteString(e.At.UTC().Format(time.RFC3339))
	b.WriteByte(' ')
	b.WriteString(e.Kind)
	if fields != "" {
		b.WriteByte(' ')
		b.WriteString(fields)
	}
	author := e.Author
	if author == "" {
		author = "unknown"
	}
	b.WriteString(" by=")
	b.WriteString(streamValue(author))
	return b.String()
}

// ChecklistStreamEvents turns one task-list write's item changes into stream
// events (Kind and Fields only), each carrying base — the list's locator and,
// when known, its task-source index — ahead of the item's own fields. Shared by
// every writer so the front ends and the daemon describe a write identically.
func ChecklistStreamEvents(base []StreamField, changes []ChecklistChange) []StreamEvent {
	var out []StreamEvent
	for _, c := range changes {
		fields := append(append([]StreamField(nil), base...), StreamInt("index", int64(c.Index)))
		var kind string
		switch c.Op {
		case EditInsert:
			kind = StreamTaskCreated
		case EditDelete:
			kind = StreamTaskDeleted
		case EditMove:
			kind = StreamTaskMoved
			fields = append(fields, StreamInt("from", int64(c.From)))
		default:
			kind = StreamTaskUpdated
		}
		if c.Op != EditDelete {
			fields = append(fields, StreamStr("mark", c.Mark))
		}
		out = append(out, StreamEvent{Kind: kind, Fields: fields})
	}
	return out
}

// streamValue returns v verbatim when every rune is one a reader can split on
// whitespace and "=" without ambiguity, and strconv.Quote(v) otherwise. An
// empty value is quoted so "k=" never reads as a truncated line.
func streamValue(v string) string {
	if v == "" {
		return `""`
	}
	for _, r := range v {
		if !streamSafeRune(r) {
			return strconv.Quote(v)
		}
	}
	return v
}

func streamSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_-./:@+,#~", r)
}
