// Package herdr contains the Herdr adapters: the raw-socket event
// subscriber (IR-001) and the CLI action executor via HERDR_BIN_PATH
// (IR-002/IR-003).
//
// Herdr's socket protocol (observed against herdr 0.7): one request per
// connection — herdr answers and closes, so nothing is ever pipelined or
// reused (verified live against 0.8.2: a second request on the same
// connection gets a reset). For events.subscribe the connection then stays
// open as a pure NDJSON stream of {"event": name, "data": {...}} frames.
// Herd-wide subscriptions are allowed for pane.created /
// pane.agent_detected / pane.exited (current panes were replayed on
// subscribe before herdr 0.9.0; 0.9.0+ sends live events only), but
// pane.agent_status_changed requires a pane_id filter — so
// the subscriber runs a discovery connection plus a status connection that
// is rebuilt whenever the monitored pane set changes.
//
// The pane LISTING those two loops are built from goes over the CLI instead
// (PaneLister → CLI.ListPanes), because herdr's own development is
// CLI-first and the CLI is itself a client of the same pane.list method —
// identical rows, one extra process. The accepted consequence is that the
// subscriber needs BOTH transports: a listing can succeed while the stream
// that follows it fails. That is benign — it costs one extra exec per
// retry, bounded by loop()'s 30s ceiling to about two per minute — but it
// is the reason a herdr outage is still reported by the STREAM, never by an
// empty listing (see CLI.ListPanes on why a zero-exit error envelope is
// refused rather than decoded as an empty herd).
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// PaneLister reports herdr's live pane set. CLI.ListPanes is the production
// implementation; tests substitute a fake so the pane set they drive and the
// one the subscriber reads stay a single source of truth.
type PaneLister interface {
	ListPanes(ctx context.Context) ([]domain.PaneRecord, error)
}

// Subscriber maintains the events.subscribe connections and delivers
// agent-status transitions; it reconnects with exponential backoff and
// never sends input while disconnected (FR-023).
type Subscriber struct {
	SocketPath string
	// Dial allows tests to substitute the transport.
	Dial func(ctx context.Context) (net.Conn, error)
	// Panes lists the live pane set. Never nil: NewSubscriber falls back to
	// a default CLI, because listPanes runs on the reconnect loop where a
	// nil deref would panic the daemon path.
	Panes PaneLister

	mu    sync.Mutex
	panes map[string]paneInfo // monitored pane set (FR-001)
	dirty chan struct{}       // signals a pane-set change to the status loop

	// watched is the pane set the status stream last subscribed, each with
	// the agent label it carried then ("" for a shell), and subscribed
	// whether it ever has; both are owned by the status loop alone
	// (runStatus), so they need no lock. See runStatus.
	watched    map[string]string
	subscribed bool

	// resync is set when the STATUS stream reports events_lost: herdr evicted
	// status events this reader had not consumed, so every watched pane's
	// status is suspect, not only a newly watched one's. The next runStatus
	// replays them all from the pane.list it takes. See errEventsLost.
	resync atomic.Bool
}

type paneInfo struct {
	workspaceID string
	tabID       string
	agentLabel  string
	// status is the agent status pane.list last reported, used to replay a
	// newly watched agent's current state (see runStatus).
	status string
}

// NewSubscriber creates a subscriber for the given Herdr socket path, using
// panes for the pane listing. A nil lister falls back to a default CLI so the
// zero-configuration caller still works; pass the process's own CLI adapter
// to inherit its resolved binary path and timeout.
func NewSubscriber(socketPath string, panes PaneLister) *Subscriber {
	if panes == nil {
		panes = NewCLI()
	}
	s := &Subscriber{
		SocketPath: socketPath,
		Panes:      panes,
		panes:      map[string]paneInfo{},
		dirty:      make(chan struct{}, 1),
	}
	s.Dial = func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, "unix", s.SocketPath)
	}
	return s
}

type socketRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// eventFrame is a pushed event or the subscription ack/error.
type eventFrame struct {
	ID    string `json:"id,omitempty"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type eventData struct {
	Type        string `json:"type"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	Pane        *struct {
		PaneID      string `json:"pane_id"`
		TabID       string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"pane,omitempty"`
}

// Subscribe streams transitions into out until ctx is done. It runs the
// discovery and status loops concurrently, each reconnecting with backoff.
func (s *Subscriber) Subscribe(ctx context.Context, out chan<- domain.AgentTransition) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.loop(ctx, "discovery", func(c context.Context) error { return s.runDiscovery(c, out) })
	}()
	go func() {
		defer wg.Done()
		s.loop(ctx, "status", func(c context.Context) error { return s.runStatus(c, out) })
	}()
	wg.Wait()
	return ctx.Err()
}

// errPaneSetChanged marks the two returns that are ORDINARY CONTROL FLOW, not a
// fault: the status loop unwinds on purpose whenever the pane set changes, so it
// can resubscribe to the new set. Every pane open, close, split and
// agent-detection produces one.
//
// It is a sentinel rather than a bare error because loop() must tell the two
// apart, and conflating them cost twice over: every routine pane split logged a
// WARN reading "connection lost", and — worse — it advanced the reconnect
// backoff. Since the healthy-stretch reset needs a full uninterrupted minute
// (rare on a busy herd), ordinary churn ratcheted the delay to the 30s cap and
// hap went BLIND to status events for that long after a normal split.
var errPaneSetChanged = errors.New("pane set changed")

// errEventsLost is herdr telling a subscriber it fell behind (herdr 0.9.2+):
// the server answers the subscription's request id with error code
// "events_lost" and CLOSES the connection, rather than skipping the evicted
// events silently as earlier releases did. History is shared across event
// types, so an overrun is reported even when nothing this subscription
// filters for was evicted.
//
// It is a protocol signal, not an outage — herdr's documented recovery is to
// resubscribe and reconcile from an authoritative read, which is what the
// pane.list on every (re)subscribe already is. So loop() resubscribes at once
// at INFO. A status-stream overrun sets resync, so the next subscribe replays
// EVERY watched agent's current status (a missed status change); a discovery
// overrun marks the pane set dirty, so the status loop re-lists it (a missed
// pane.agent_detected or pane.exited) — herdr 0.9.0+ no longer replays
// existing panes to a new discovery subscription, so that listing is the
// only way back. What is restored is current STATE, never
// the lost history: an agent that went working and back to idle inside the
// gap is replayed once, as idle.
var errEventsLost = errors.New("herdr dropped events for this subscriber (events_lost)")

// eventsLostCode is the error code herdr 0.9.2+ sends with errEventsLost.
const eventsLostCode = "events_lost"

// eventsLostQuietWindow bounds the immediate resubscribe: an overrun after a
// quiet stretch is routine, but a second one inside the window means herdr is
// overrunning this reader repeatedly (or during subscription setup), and
// reconnecting at once would add a pane.list and a subscribe per round to the
// load that caused it. That one falls through to the ordinary backoff.
const eventsLostQuietWindow = time.Minute

// loopHealthyStretch is how long a connection must stay up before loop()
// treats it as healthy and resets the backoff. A var only so a test can
// shorten it.
var loopHealthyStretch = time.Minute

// loop runs fn with exponential backoff, resetting the backoff after a
// connection that stayed healthy for a while.
//
// An expected resubscribe (errPaneSetChanged) is exempt from both: it logs at
// Debug and leaves the backoff where it was, so pane churn can neither fill the
// log nor slow reconnection. It reconnects immediately — there is nothing to
// back off from.
//
// herdr's events_lost (errEventsLost) is exempt too, but only once per
// eventsLostQuietWindow: it reconnects at once and logs at INFO. It does not
// itself reset the backoff (an overrun during subscription setup is no
// evidence of a healthy connection), but the healthy-stretch reset is applied
// first, so a stream that ran for a while before overrunning still clears a
// ladder an old outage left behind. A repeat inside the window takes the
// ordinary backoff and WARN.
func (s *Subscriber) loop(ctx context.Context, name string, fn func(context.Context) error) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	var lastLost time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errPaneSetChanged) {
			// Reaching here at all means the subscribe SUCCEEDED and then ran
			// until the pane set moved — positive evidence the connection is
			// healthy, so reset the ladder rather than merely not advancing it.
			// Without this, a backoff ratcheted by an earlier real outage stays
			// at up to 30s: the healthy-stretch reset below needs a full
			// uninterrupted minute, and `started` restarts every iteration, so
			// on a herd with sub-minute churn it can never fire.
			backoff = time.Second
			slog.Debug("herdr pane set changed; resubscribing",
				"loop", name, "error", err)
			continue
		}
		if time.Since(started) > loopHealthyStretch {
			backoff = time.Second // healthy stretch: reset the backoff
		}
		if errors.Is(err, errEventsLost) {
			repeat := !lastLost.IsZero() && time.Since(lastLost) < eventsLostQuietWindow
			lastLost = time.Now()
			if !repeat {
				slog.Info("herdr dropped events for this subscriber; resubscribing and resyncing from pane.list",
					"loop", name, "error", err)
				continue
			}
		}
		slog.Warn("herdr event connection lost; reconnecting with backoff",
			"loop", name, "error", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runDiscovery watches the herd-wide pane lifecycle: pane.created announces
// panes (and, before herdr 0.9.0, replays existing ones on subscribe),
// pane.agent_detected attaches agent labels, pane.exited removes panes.
func (s *Subscriber) runDiscovery(ctx context.Context, out chan<- domain.AgentTransition) error {
	subs := []map[string]string{
		{"type": "pane.created"},
		{"type": "pane.agent_detected"},
		{"type": "pane.exited"},
	}
	err := s.stream(ctx, "hap_discovery", subs, func(frame eventFrame) error {
		var d eventData
		if err := json.Unmarshal(frame.Data, &d); err != nil {
			slog.Warn("undecodable discovery event ignored", "error", err)
			return nil
		}
		switch normalizeEventName(frame.Event, d.Type) {
		case "pane.created":
			paneID, wsID, tabID := d.PaneID, d.WorkspaceID, d.TabID
			if d.Pane != nil {
				paneID, wsID, tabID = d.Pane.PaneID, d.Pane.WorkspaceID, d.Pane.TabID
			}
			if paneID != "" {
				s.upsertPane(paneID, wsID, tabID, "")
			}
		case "pane.agent_detected":
			if d.PaneID == "" {
				return nil
			}
			// Some plugin/side-panel panes are announced with Herdr's
			// placeholder agent values. A detection event has no meaningful
			// status yet, so the empty status participates in the same two-field
			// filter used for live agent-list rows.
			if domain.IsPlaceholderAgent(d.Agent, d.AgentStatus) {
				return nil
			}
			s.upsertPane(d.PaneID, d.WorkspaceID, d.TabID, domain.CanonicalAgentType(d.Agent))
			// Surface the discovery as a transition so the daemon can name
			// the agent immediately. herdr before 0.9.0 replays
			// agent_detected for existing panes on subscribe, so there this
			// also covers agents that predate the daemon; on 0.9.0+ those
			// reach it through the startup reconcile and pane.list. The
			// daemon takes no action on "detected".
			if d.Agent != "" {
				tr := domain.AgentTransition{
					AgentID:     d.PaneID,
					AgentType:   domain.CanonicalAgentType(d.Agent),
					PaneID:      d.PaneID,
					TabID:       s.tabID(d.PaneID),
					WorkspaceID: d.WorkspaceID,
					Status:      domain.AgentStatusDetected,
					At:          time.Now(),
				}
				select {
				case out <- tr:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case "pane.exited", "pane.closed":
			if d.PaneID != "" {
				s.removePane(d.PaneID)
			}
		}
		return nil
	})
	if errors.Is(err, errEventsLost) {
		// A pane.agent_detected or pane.exited may be among the evicted
		// events, and a new discovery subscription replays nothing (herdr
		// 0.9.0+), so make the status loop re-list the pane set now. That
		// alone recovers them: a missed new agent is newly watched and so
		// replayed, a missed exit is pruned by the listing. No resync — each
		// connection has its own read position, so the status stream lost
		// nothing, and replaying every agent would only cost herdr a capture
		// per parked agent while it is already overloaded.
		s.signalDirty()
	}
	return err
}

// runStatus subscribes to pane.agent_status_changed for every live AGENT pane;
// it returns (to be re-run by loop) whenever the pane set changes. The pane
// set is fetched authoritatively via pane.list on every (re)subscribe —
// discovery events only trigger the refresh — so a pane that exited during
// a reconnect window can never wedge the subscription (herdr rejects
// subscriptions naming dead panes).
//
// Only panes carrying a detected agent label are watched. A status
// subscription is not free for herdr: measured on herdr 0.8.2, holding one for
// each of 13 live panes (2 of them agents) was ~5.5 points of the server's CPU
// — more than half its load with the daemon up — against ~2.8 for the two
// agent panes alone, and a plain shell never reports an agent status to
// watch. A shell that STARTS an agent is announced by pane.agent_detected,
// which labels it and resubscribes (upsertPane).
//
// That resubscribe always comes too late for one event: herdr emits the
// detection and the agent's first status change in the SAME update, so the
// status is sent while the pane is still unwatched. It is not a race to win.
// So a resubscribe that adds an agent pane — or finds a label on one it
// watched as a shell, whose stream the label's own resubscribe tore down
// mid-update — replays that pane's current status from the pane.list
// snapshot it just took — once, and only on a resubscribe:
// the first subscribe is the daemon starting, which its startup reconcile
// already covers. A duplicate (the event did arrive) is harmless, since the
// daemon coalesces captures per pane.
//
// The one exception is herdr's events_lost (errEventsLost): the status events
// it evicted are never resent, so the next subscribe — even a first one —
// replays EVERY watched agent, not only the newly watched. Replayed
// transitions are marked Replayed, so the daemon never reads one as a human
// check-in.
//
// The filter needs pane.list to REPORT agent labels (herdr 0.8.2 does). A
// listing that carries none at all — a herdr that does not report them, or a
// herd with no agent running — subscribes every pane, exactly as before:
// filtering on labels that are never reported would leave an agent that was
// already running when the daemon started with no status subscription at all.
func (s *Subscriber) runStatus(ctx context.Context, out chan<- domain.AgentTransition) error {
	allPanes, labelled, err := s.listPanes(ctx)
	if err != nil {
		// Not wrapped with the method name: call()'s errors already carry it.
		return err
	}
	paneIDs := allPanes
	if labelled {
		paneIDs = s.agentPanes(allPanes)
	}
	labels := s.labels(paneIDs)
	// After events_lost every watched agent's status is suspect, so all of
	// them are replayed, not only the newly watched (see errEventsLost).
	resync := s.resync.Swap(false)
	var fresh []string
	if s.subscribed || resync {
		for _, id := range paneIDs {
			// New to the stream, or watched before only as a shell: either
			// way the detection's status update was not seen as an agent's.
			if prev, ok := s.watched[id]; labels[id] != "" && (resync || !ok || prev == "") {
				fresh = append(fresh, id)
			}
		}
	}
	s.watched = labels
	s.subscribed = true
	for _, tr := range s.replayStatus(fresh) {
		select {
		case out <- tr:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(paneIDs) == 0 {
		// Nothing to watch yet: wait for discovery to find agent panes.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.dirty:
			return errPaneSetChanged
		}
	}

	subs := make([]map[string]string, 0, len(paneIDs))
	for _, id := range paneIDs {
		subs = append(subs, map[string]string{
			"type": "pane.agent_status_changed", "pane_id": id,
		})
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.dirty:
			cancel() // pane set changed: rebuild the subscription
		case <-streamCtx.Done():
		}
	}()

	err = s.stream(streamCtx, "hap_status", subs, func(frame eventFrame) error {
		var d eventData
		if err := json.Unmarshal(frame.Data, &d); err != nil {
			slog.Warn("undecodable status event ignored", "error", err)
			return nil
		}
		if normalizeEventName(frame.Event, d.Type) != "pane.agent_status_changed" || d.PaneID == "" {
			return nil
		}
		agentType := domain.CanonicalAgentType(d.Agent)
		if agentType == "" {
			agentType = s.agentLabel(d.PaneID)
		}
		if domain.IsPlaceholderAgent(agentType, d.AgentStatus) {
			return nil
		}
		tabID := d.TabID
		if tabID == "" {
			tabID = s.tabID(d.PaneID)
		}
		tr := domain.AgentTransition{
			AgentID:     d.PaneID,
			AgentType:   agentType,
			PaneID:      d.PaneID,
			TabID:       tabID,
			WorkspaceID: d.WorkspaceID,
			Status:      d.AgentStatus,
			At:          time.Now(),
		}
		select {
		case out <- tr:
		case <-streamCtx.Done():
			return streamCtx.Err()
		}
		return nil
	})
	// Recorded BEFORE the pane-set mapping below: a dirty signal racing the
	// overrun cancels streamCtx too, and returning errPaneSetChanged without
	// the flag would drop the replay this overrun needs.
	if errors.Is(err, errEventsLost) {
		s.resync.Store(true)
	}
	if ctx.Err() == nil && streamCtx.Err() != nil {
		return fmt.Errorf("resubscribing: %w", errPaneSetChanged)
	}
	return err
}

// stream opens one connection, sends one events.subscribe request, and
// dispatches pushed frames to handle until the connection or ctx ends.
func (s *Subscriber) stream(ctx context.Context, reqID string, subs []map[string]string,
	handle func(eventFrame) error) error {

	conn, err := s.Dial(ctx)
	if err != nil {
		return fmt.Errorf("dial herdr socket: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req, _ := json.Marshal(socketRequest{
		ID: reqID, Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subs},
	})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("write subscribe: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var frame eventFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			slog.Warn("undecodable socket line ignored", "error", err)
			continue
		}
		if frame.Error != nil {
			if frame.Error.Code == eventsLostCode {
				return fmt.Errorf("%w: %s", errEventsLost, frame.Error.Message)
			}
			return fmt.Errorf("herdr socket error: %s: %s", frame.Error.Code, frame.Error.Message)
		}
		if frame.Event == "" {
			continue // subscription ack
		}
		if err := handle(frame); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read socket: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("herdr socket closed")
}

// normalizeEventName reconciles the two observed spellings
// ("pane_created" / "pane.created" / data.type).
func normalizeEventName(names ...string) string {
	for _, n := range names {
		switch n {
		case "pane.created", "pane_created":
			return "pane.created"
		case "pane.agent_detected", "pane_agent_detected":
			return "pane.agent_detected"
		case "pane.exited", "pane_exited":
			return "pane.exited"
		case "pane.closed", "pane_closed":
			return "pane.closed"
		case "pane.agent_status_changed", "pane_agent_status_changed":
			return "pane.agent_status_changed"
		}
	}
	return ""
}

func (s *Subscriber) upsertPane(paneID, workspaceID, tabID, agentLabel string) {
	s.mu.Lock()
	info, exists := s.panes[paneID]
	changed := !exists
	if workspaceID != "" && info.workspaceID != workspaceID {
		info.workspaceID = workspaceID
	}
	if tabID != "" && info.tabID != tabID {
		info.tabID = tabID
	}
	if agentLabel != "" && info.agentLabel != agentLabel {
		info.agentLabel = agentLabel
		// A pane that has just gained an agent joins the status
		// subscription, which only watches agent panes (see runStatus).
		changed = true
	}
	s.panes[paneID] = info
	s.mu.Unlock()
	if changed {
		s.signalDirty()
	}
}

// labels snapshots the agent label of each pane ("" for a shell).
func (s *Subscriber) labels(ids []string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		label := s.panes[id].agentLabel
		if domain.IsPlaceholderAgent(label, "") {
			label = ""
		}
		out[id] = label
	}
	return out
}

// replayStatus builds a transition from pane.list's snapshot for each pane in
// ids that reported a real agent and status — the newly watched panes, or
// every watched one after events_lost (see runStatus). Each is marked
// Replayed: it is current state, not evidence of a change.
func (s *Subscriber) replayStatus(ids []string) []domain.AgentTransition {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.AgentTransition
	for _, id := range ids {
		info := s.panes[id]
		if info.agentLabel == "" || domain.IsPlaceholderAgent("", info.status) {
			continue
		}
		out = append(out, domain.AgentTransition{
			AgentID: id, AgentType: info.agentLabel, PaneID: id,
			TabID: info.tabID, WorkspaceID: info.workspaceID,
			Status: info.status, At: time.Now(), Replayed: true,
		})
	}
	return out
}

// agentPanes keeps the panes that host a detected agent — the only ones the
// status subscription watches (see runStatus).
func (s *Subscriber) agentPanes(ids []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if label := s.panes[id].agentLabel; label != "" && !domain.IsPlaceholderAgent(label, "") {
			out = append(out, id)
		}
	}
	return out
}

func (s *Subscriber) removePane(paneID string) {
	s.mu.Lock()
	_, exists := s.panes[paneID]
	delete(s.panes, paneID)
	s.mu.Unlock()
	if exists {
		s.signalDirty()
	}
}

func (s *Subscriber) signalDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// SocketError is a protocol-level error herdr returned in place of a result.
// It is distinct from a transport failure so callers can tell "herdr answered,
// and said no" from "we never reached herdr".
type SocketError struct {
	Method  string
	Code    string
	Message string
}

func (e *SocketError) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.Method, e.Code, e.Message)
}

// call performs one request/response round trip over a SHORT-LIVED connection
// and decodes the single response line's `result` into out (nil discards it).
// herdr's protocol is newline-delimited JSON with no handshake, so one write
// plus one read is the whole exchange.
//
// This is the request/response counterpart to stream(), which keeps its
// connection open as an event feed instead.
//
// EVERY error it returns names the method, so callers must not wrap with the
// method again — that is what produced "pane.list: herdr pane.list: …".
func call(ctx context.Context, dial func(context.Context) (net.Conn, error),
	id, method string, params any, out any) error {

	conn, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("dial herdr socket for %s: %w", method, err)
	}
	defer conn.Close()
	// Closing the conn is what unblocks the read below; ctx alone cannot
	// interrupt a Scan already in flight.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req, err := json.Marshal(socketRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("write %s request: %w", method, err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read %s response: %w", method, err)
		}
		return fmt.Errorf("connection closed before %s response", method)
	}
	var envelope struct {
		ID    string `json:"id"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	// herdr echoes the request id. A mismatch means we are reading someone
	// else's answer — decoding it as ours would be worse than failing. Only
	// checked when the server actually sent one, so an id-less reply still
	// works.
	if envelope.ID != "" && envelope.ID != id {
		return fmt.Errorf("%s response id %q does not match request %q", method, envelope.ID, id)
	}
	if envelope.Error != nil {
		return &SocketError{Method: method, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	// An absent result is not an error: the pre-extraction code decoded a
	// missing `result` into a zero struct and carried on, so this preserves
	// that. A malformed one still errors, one Unmarshal later.
	if out == nil || len(envelope.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// listPanes queries the live pane set through the CLI, and reports whether
// the listing labelled any pane with its agent (see runStatus).
func (s *Subscriber) listPanes(ctx context.Context) (ids []string, labelled bool, err error) {
	panes, err := s.Panes.ListPanes(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list panes: %w", err)
	}
	for _, p := range panes {
		if !domain.IsPlaceholderAgent(p.Agent, "") {
			labelled = true
			break
		}
	}
	ids = make([]string, 0, len(panes))
	s.mu.Lock()
	live := map[string]bool{}
	for _, p := range panes {
		ids = append(ids, p.PaneID)
		live[p.PaneID] = true
		info := s.panes[p.PaneID]
		if p.WorkspaceID != "" {
			info.workspaceID = p.WorkspaceID
		}
		if p.TabID != "" {
			info.tabID = p.TabID
		}
		info.status = p.AgentStatus
		// A listing that labels agents is the authoritative snapshot: a pane
		// it reports with no agent has none now (the agent exited back to its
		// shell), so the label is cleared rather than kept watching a plain
		// shell. One that labels nothing says nothing about agents, so the
		// labels pane.agent_detected attached are kept.
		if !domain.IsPlaceholderAgent(p.Agent, "") {
			info.agentLabel = domain.CanonicalAgentType(p.Agent)
		} else if labelled {
			info.agentLabel = ""
		}
		s.panes[p.PaneID] = info
	}
	// Prune panes that no longer exist so labels don't leak.
	for id := range s.panes {
		if !live[id] {
			delete(s.panes, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	return ids, labelled, nil
}

func (s *Subscriber) agentLabel(paneID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panes[paneID].agentLabel
}

// tabID returns the tracked tab id for a pane ("" when unknown).
func (s *Subscriber) tabID(paneID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panes[paneID].tabID
}
