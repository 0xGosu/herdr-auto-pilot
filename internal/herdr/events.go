// Package herdr contains the Herdr adapters: the raw-socket event
// subscriber (IR-001) and the CLI action executor via HERDR_BIN_PATH
// (IR-002/IR-003).
//
// Herdr's socket protocol (observed against herdr 0.7): one
// events.subscribe request per connection, after which the connection is a
// pure NDJSON event stream of {"event": name, "data": {...}} frames.
// Herd-wide subscriptions are allowed for pane.created /
// pane.agent_detected / pane.exited (current panes are replayed on
// subscribe), but pane.agent_status_changed requires a pane_id filter — so
// the subscriber runs a discovery connection plus a status connection that
// is rebuilt whenever the monitored pane set changes.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Subscriber maintains the events.subscribe connections and delivers
// agent-status transitions; it reconnects with exponential backoff and
// never sends input while disconnected (FR-023).
type Subscriber struct {
	SocketPath string
	// Dial allows tests to substitute the transport.
	Dial func(ctx context.Context) (net.Conn, error)

	mu    sync.Mutex
	panes map[string]paneInfo // monitored pane set (FR-001)
	dirty chan struct{}       // signals a pane-set change to the status loop
}

type paneInfo struct {
	workspaceID string
	tabID       string
	agentLabel  string
}

// NewSubscriber creates a subscriber for the given Herdr socket path.
func NewSubscriber(socketPath string) *Subscriber {
	s := &Subscriber{
		SocketPath: socketPath,
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

// loop runs fn with exponential backoff, resetting the backoff after a
// connection that stayed healthy for a while.
func (s *Subscriber) loop(ctx context.Context, name string, fn func(context.Context) error) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // healthy stretch: reset the backoff
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

// runDiscovery watches the herd-wide pane lifecycle: pane.created replays
// existing panes on subscribe, pane.agent_detected attaches agent labels,
// pane.exited removes panes.
func (s *Subscriber) runDiscovery(ctx context.Context, out chan<- domain.AgentTransition) error {
	subs := []map[string]string{
		{"type": "pane.created"},
		{"type": "pane.agent_detected"},
		{"type": "pane.exited"},
	}
	return s.stream(ctx, "hap_discovery", subs, func(frame eventFrame) error {
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
			s.upsertPane(d.PaneID, d.WorkspaceID, d.TabID, d.Agent)
			// Surface the discovery as a transition so the daemon can name
			// the agent immediately — herdr replays agent_detected for
			// existing panes on subscribe, so this also covers agents that
			// predate the daemon. The daemon takes no action on "detected".
			if d.Agent != "" {
				tr := domain.AgentTransition{
					AgentID:     d.PaneID,
					AgentType:   d.Agent,
					PaneID:      d.PaneID,
					TabID:       s.tabID(d.PaneID),
					WorkspaceID: d.WorkspaceID,
					Status:      "detected",
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
}

// runStatus subscribes to pane.agent_status_changed for every live pane;
// it returns (to be re-run by loop) whenever the pane set changes. The pane
// set is fetched authoritatively via pane.list on every (re)subscribe —
// discovery events only trigger the refresh — so a pane that exited during
// a reconnect window can never wedge the subscription (herdr rejects
// subscriptions naming dead panes).
func (s *Subscriber) runStatus(ctx context.Context, out chan<- domain.AgentTransition) error {
	paneIDs, err := s.listPanes(ctx)
	if err != nil {
		return fmt.Errorf("pane.list: %w", err)
	}
	if len(paneIDs) == 0 {
		// Nothing to watch yet: wait for discovery to find panes.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.dirty:
			return fmt.Errorf("pane set changed")
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
		agentType := d.Agent
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
	if ctx.Err() == nil && streamCtx.Err() != nil {
		return fmt.Errorf("pane set changed; resubscribing")
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
	}
	s.panes[paneID] = info
	s.mu.Unlock()
	if changed {
		s.signalDirty()
	}
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

// listPanes queries the live pane set over a short-lived connection.
func (s *Subscriber) listPanes(ctx context.Context) ([]string, error) {
	conn, err := s.Dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req, _ := json.Marshal(socketRequest{ID: "hap_pane_list", Method: "pane.list", Params: map[string]any{}})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("connection closed before pane.list response")
	}
	var resp struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Panes []struct {
				PaneID      string `json:"pane_id"`
				TabID       string `json:"tab_id"`
				WorkspaceID string `json:"workspace_id"`
				Agent       string `json:"agent"`
			} `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	ids := make([]string, 0, len(resp.Result.Panes))
	s.mu.Lock()
	live := map[string]bool{}
	for _, p := range resp.Result.Panes {
		ids = append(ids, p.PaneID)
		live[p.PaneID] = true
		info := s.panes[p.PaneID]
		if p.WorkspaceID != "" {
			info.workspaceID = p.WorkspaceID
		}
		if p.TabID != "" {
			info.tabID = p.TabID
		}
		if !domain.IsPlaceholderAgent(p.Agent, "") {
			info.agentLabel = p.Agent
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
	return ids, nil
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
