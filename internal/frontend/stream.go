package frontend

// Emitters for the orchestrator event stream (`hap stream orchestrator`).
//
// Every event is written at the one chokepoint that makes its kind of change,
// AFTER that change has committed, and is best-effort: a failed append is
// logged and swallowed (ports.StreamLog). Emitting at the chokepoint rather
// than diffing state somewhere later is what lets an event say WHO acted — the
// orchestrator must tell an operator's manual rule edit from the daemon's own
// learning, and only the writer knows which it is.

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// emit appends one event authored by this App.
func (a *App) emit(ctx context.Context, kind string, fields ...domain.StreamField) {
	a.emitAs(ctx, a.Author, kind, fields...)
}

// emitAs appends one event under an explicit author — for a path the daemon
// runs on an operator's behalf, where the App's own Author is "daemon".
//
// context.WithoutCancel for the reason recordFSPToggle gives: the change has
// already landed, and the likeliest cancellation is the operator leaving.
func (a *App) emitAs(ctx context.Context, author, kind string, fields ...domain.StreamField) {
	if a.Stream == nil {
		return
	}
	now := time.Now()
	if a.Clock != nil {
		now = a.Clock()
	}
	ev := domain.StreamEvent{At: now, Kind: kind, Author: author, Fields: fields}
	if _, err := a.Stream.Append(context.WithoutCancel(ctx), ev); err != nil {
		slog.Warn("could not record an orchestrator stream event", "kind", kind, "error", err)
	}
}

// configSnapshot is what a config write is diffed against once it has saved.
type configSnapshot struct {
	flat    map[string]string
	sources []config.TaskSource
}

// snapshotConfig captures cfg before a write mutates it. The sources are
// COPIED: the mutator edits the slice in place, so a shared backing array
// would make every per-source edit read as no change at all. nil (no stream,
// or an unencodable config) skips the diff.
func (a *App) snapshotConfig(cfg config.Config) *configSnapshot {
	if a.Stream == nil {
		return nil
	}
	flat, err := config.FlattenKeys(cfg)
	if err != nil {
		slog.Warn("could not snapshot config for the orchestrator stream", "error", err)
		return nil
	}
	return &configSnapshot{flat: flat, sources: append([]config.TaskSource(nil), cfg.TaskSources...)}
}

// streamSelfReportingConfigKeys are config keys whose change is announced by
// an event of its own, so config.changed leaves them out rather than saying it
// twice.
func streamSelfReportingConfigKey(key string) bool {
	return key == FSPFieldKey || key == "task_sources" || strings.HasPrefix(key, "task_sources.")
}

// emitConfigDiff announces what a saved config write changed: one
// config.changed naming the keys (never their values — env tables and tokens
// live in this file), plus one task_source.* per source added, removed or
// edited.
func (a *App) emitConfigDiff(ctx context.Context, before *configSnapshot, after config.Config) {
	if before == nil {
		return
	}
	now := a.snapshotConfig(after)
	if now == nil {
		return
	}
	var keys []string
	for _, k := range config.ChangedKeys(before.flat, now.flat) {
		if !streamSelfReportingConfigKey(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		a.emit(ctx, domain.StreamConfigChanged, domain.StreamStr("keys", strings.Join(keys, ",")))
	}
	a.emitTaskSourceDiff(ctx, before.sources, now.sources)
}

// emitTaskSourceDiff compares whole entries, the rule every task-source remover
// follows. A removed source is named by its OLD index: every later index has
// already shifted down by one, which is why a reader re-lists rather than
// trusting indices it cached.
func (a *App) emitTaskSourceDiff(ctx context.Context, before, after []config.TaskSource) {
	edits := domain.PairChanges(domain.DiffSequences(len(before), len(after), func(i, j int) bool {
		return reflect.DeepEqual(before[i], after[j])
	}))
	for _, e := range edits {
		switch e.Op {
		case domain.EditInsert:
			a.emit(ctx, domain.StreamTaskSourceAdded, taskSourceFields(e.After, after[e.After])...)
		case domain.EditDelete:
			a.emit(ctx, domain.StreamTaskSourceRemoved, taskSourceFields(e.Before, before[e.Before])...)
		case domain.EditChange:
			a.emit(ctx, domain.StreamTaskSourceUpdated, taskSourceFields(e.After, after[e.After])...)
		}
	}
}

func taskSourceFields(index int, src config.TaskSource) []domain.StreamField {
	fields := []domain.StreamField{domain.StreamInt("source", int64(index))}
	if src.Agent != "" {
		fields = append(fields, domain.StreamStr("agent", src.Agent))
	}
	return fields
}

// emitChecklistDiff announces the item-level changes one task-list write
// made. A write that changed no item emits nothing.
func (a *App) emitChecklistDiff(ctx context.Context, cfg config.Config, locator, before, after string) {
	if a.Stream == nil || before == after {
		return
	}
	changes := domain.DiffChecklist(domain.ParseChecklist(before), domain.ParseChecklist(after))
	if len(changes) == 0 {
		return
	}
	base := []domain.StreamField{domain.StreamStr("list", tasklocator.Canonical(locator))}
	if i, _, ok := a.sourceIndexForLocator(cfg, "", locator); ok {
		base = append(base, domain.StreamInt("source", int64(i)))
	}
	for _, ev := range domain.ChecklistStreamEvents(base, changes) {
		a.emit(ctx, ev.Kind, ev.Fields...)
	}
}

// emitTaskList announces a database task list created or removed. Only db://
// lists are hap's to create and drop; a file or a gist belongs to the
// operator, so nothing is said about those.
func (a *App) emitTaskList(ctx context.Context, kind, locator string) {
	if tasklocator.Scheme(locator) != tasklocator.DBScheme {
		return
	}
	a.emit(ctx, kind, domain.StreamStr("list", locator))
}

// emitCorrection announces an operator's answer to (or correction of) an
// audit row. send is "true", "false", or "" when the path does not know.
func (a *App) emitCorrection(ctx context.Context, author string, corrID int64, audit *domain.AuditRecord, send string) {
	fields := []domain.StreamField{
		domain.StreamInt("id", corrID), domain.StreamInt("escalation", audit.ID),
	}
	if audit.AgentID != "" {
		fields = append(fields, a.streamAgent(audit.AgentID))
	}
	if send != "" {
		fields = append(fields, domain.StreamStr("send", send))
	}
	a.emitAs(ctx, author, domain.StreamCorrection, fields...)
}

// streamAgent names an escalation's agent the way every hap surface does: its
// short name when this node has one, its pane id otherwise.
func (a *App) streamAgent(agentID string) domain.StreamField {
	_, name := a.agentSpellings(agentID)
	return domain.StreamStr("agent", name)
}

// shortSignature is the signature prefix `hap signatures` prints and accepts.
func shortSignature(sig string) string {
	if len(sig) > 12 {
		return sig[:12]
	}
	return sig
}
